// Package chat owns accepted chat operations independently of HTTP/SSE.
package chat

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"mullsmith/internal/agent"
	"mullsmith/internal/auth"
	"mullsmith/internal/config"
	"mullsmith/internal/llm"
	"mullsmith/internal/store"
	"mullsmith/internal/tools"
)

type Refusal struct {
	Code, Message string
	RetryAfter    int
}

func (e *Refusal) Error() string { return e.Message }

type Command struct {
	ConversationID, AutoTitle, Content, RegenerateID, RequestID string
	AttachmentPaths                                             []string
	Thinking                                                    *string
}
type Service struct {
	cfg         *config.Config
	store       *store.DB
	logger      *slog.Logger
	generator   *agent.Generator
	limiter     *auth.Limiter
	Tools       *tools.Registry
	Generations *Registry
	Approvals   *Approvals
	Window      func(string) (int64, bool)
}

func New(cfg *config.Config, db *store.DB, logger *slog.Logger, registry *tools.Registry) *Service {
	return &Service{cfg: cfg, store: db, logger: logger, generator: &agent.Generator{LLM: llm.NewClient(cfg, nil)}, limiter: auth.NewLimiter(logger), Tools: registry, Generations: NewRegistry(), Approvals: NewApprovals()}
}
func (s *Service) overflowLine(model string) (int64, bool) {
	if s.Window == nil {
		return 0, false
	}
	return s.Window(model)
}

type Operation struct {
	service    *Service
	ctx        context.Context
	cancel     context.CancelFunc
	stop       func() bool
	once       sync.Once
	generation *Generation
	conv       *store.Conversation
	draft      *store.Message
	cmd        Command
	userID     string
	messages   []llm.ChatMessage
	specs      []llm.ToolSpec
	vision     bool
}

func (o *Operation) Cancel() { o.cancel() }
func (o *Operation) Close() {
	o.once.Do(func() { o.stop(); o.cancel(); o.service.Generations.Finish(o.generation) })
}
func (o *Operation) AbortBeforeStart() {
	_ = o.service.finishMessage(o.draft.ID, "", "aborted", "", 0, 0, 0, 0, 0)
}

var ApprovalTimeout = 10 * time.Minute

const ToolResultBudgetRunes = 8 * 1024

type RoundOutcome struct {
	MsgID         string `json:"message_id"`
	Status        string `json:"status"`
	Completion    int64  `json:"completion_tokens,omitempty"`
	DurationMS    int64  `json:"duration_ms,omitempty"`
	FirstAnswerMS int64  `json:"first_answer_ms,omitempty"`
	GenerationMS  int64  `json:"generation_ms,omitempty"`
	Note          string `json:"note,omitempty"`
}

// Terminal notes (C7). The reason a transcript whose rows all saved cleanly
// still has no answer must be DURABLE, or a reload quietly disagrees with
// what the live stream said — and these exact strings are what the client
// renders, live and on reload, from messages.status_note.
const (
	noteStoppedShort   = "Stopped before the answer finished."
	noteSaveIncomplete = "The reply could not be fully saved; reload this conversation."
	noteWindowMidLoop  = "This conversation reached its context window during tool rounds — start a new chat."
)

func (s *Service) Begin(ctx context.Context, userID string, cmd Command) (_ *Operation, err error) {
	if max := s.cfg.AppLimits.ChatMessagesPerHour; max > 0 && !s.limiter.Allow("chat:"+userID, max, time.Hour) {
		return nil, &Refusal{Code: "chat_limited", Message: "hourly message limit reached, please try again later", RetryAfter: 3600}
	}
	genCtx, cancel := context.WithCancel(context.Background())
	g, err := s.Generations.Start(cmd.ConversationID, userID, s.cfg.AppLimits.ConcurrentStreamsPerUser, s.cfg.AppLimits.ConcurrentStreamsTotal, cancel)
	if err != nil {
		cancel()
		return nil, err
	}
	op := &Operation{service: s, ctx: genCtx, cancel: cancel, generation: g, cmd: cmd, userID: userID}
	op.stop = context.AfterFunc(ctx, cancel)
	defer func() {
		if err != nil {
			op.Close()
		}
	}()
	conv, err := s.store.ConversationForUser(genCtx, userID, cmd.ConversationID)
	if err != nil {
		return nil, err
	}

	// Overflow gate (spec §6.1): with a known window, a send that would
	// cross it is refused before anything is persisted or paid for. The
	// estimate covers the actual built request (finding 7) including this
	// turn's content and attachment manifests, and the row cap binds with
	// or without a discovered window (review2 finding 1). The history the
	// verdict was measured against rides into the prompt: decision and
	// request come from ONE read under ONE reservation. Unknown window:
	// the token gate is open and the endpoint's own error stands.
	// A regeneration's retire set is read HERE, under the reservation and
	// before admission (C7): the same rows BeginTurnRegeneration will delete,
	// so what the gate measured and the prompt built both describe the
	// history the swap actually leaves behind.
	var excludeIDs []string
	if cmd.RegenerateID != "" {
		ids, err := s.store.TurnRetireIDs(genCtx, conv.ID, cmd.RegenerateID)
		if err != nil {
			return nil, err
		}
		excludeIDs = ids
	}

	adm, err := s.AdmitSend(genCtx, conv, userID, cmd.Content, cmd.AttachmentPaths, excludeIDs)
	if err != nil {
		// Typed store failure (C1): refuse without persisting — never a
		// silently re-read second opinion over a half-built snapshot.
		return nil, err
	}
	if adm.Full {
		return nil, &Refusal{Code: adm.Reason, Message: adm.Reason}
	}

	// A thinking choice shipped with the message is validated against the
	// backend's effective list and persisted once capacity admitted (C1):
	// an invalid one is a plain 400 that never reaches the endpoint, a
	// capacity refusal never leaves it applied, and a valid one is this
	// generation's snapshot — later tool rounds of the same reply speak it
	// even if the selector moves mid-flight.
	if cmd.Thinking != nil {
		choice, fp, ok := s.ValidateThinkingChoice(s.Model(conv.Model), *cmd.Thinking)
		if !ok {
			return nil, &Refusal{Code: "invalid_thinking_choice", Message: "unknown thinking choice for this endpoint and profile"}
		}
		if choice != conv.Thinking || fp != conv.ThinkingCtx {
			updated, err := s.store.UpdateConversation(genCtx, userID, conv.ID,
				store.ConversationPatch{Thinking: &choice, ThinkingCtx: &fp})
			if err != nil {
				return nil, err
			}
			conv = updated
		}
	}

	// Acceptance (C2): ONE store call makes the turn durable — BeginSend
	// commits user row, attachment links, draft and activity bump
	// together; BeginTurnRegeneration swaps the retired turn for its
	// placeholder atomically. A failure here is a plain HTTP error with
	// nothing committed; past this point the turn belongs to the accepted
	// generation.
	var am *store.Message
	if cmd.RegenerateID != "" {
		am, err = s.store.BeginTurnRegeneration(genCtx, userID, conv.ID, cmd.RegenerateID, g.ID)
	} else {
		_, am, err = s.store.BeginSend(genCtx, userID, conv.ID, cmd.Content, cmd.AttachmentPaths, int64(agent.EstimateTokens(cmd.Content)), g.ID)
	}
	if err != nil {
		return nil, err
	}

	op.conv, op.draft = conv, am
	op.messages, op.specs = s.BuildRequest(conv, userID, adm.History)
	op.vision = s.cfg.LLM.VisionFor(s.Model(conv.Model))
	return op, nil
}
