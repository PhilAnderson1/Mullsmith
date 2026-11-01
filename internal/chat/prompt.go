package chat

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"mullsmith/internal/agent"
	"mullsmith/internal/llm"
	"mullsmith/internal/scratch"
	"mullsmith/internal/store"
)

// buildRequest is THE prompt builder (finding 7): stored history becomes the
// exact request the next model round carries — conversation system prompt,
// tool guidance, the vision-filtered tool list, attachment manifests — so
// the streaming loop and the admission gate measure and send the same
// thing. Returns the messages and the specs that round would advertise;
// the caller-provided turn a send has not yet persisted is appended to hist
// by the gate itself, so this function only ever reads stored rows.
func (s *Service) BuildRequest(conv *store.Conversation, userID string, hist []store.Message) ([]llm.ChatMessage, []llm.ToolSpec) {
	// The tool list and picture pipeline follow the conversation's model:
	// view_image is offered only when its entry spells vision: true (§7.2),
	// and a hallucinated call to a tool that was never offered is answered
	// as an error, not executed.
	visionOn := s.cfg.LLM.VisionFor(s.Model(conv.Model))
	toolSpecs := s.Tools.Specs()
	if !visionOn {
		filtered := toolSpecs[:0:0]
		for _, sp := range toolSpecs {
			if sp.Function.Name != "view_image" {
				filtered = append(filtered, sp)
			}
		}
		toolSpecs = filtered
	}
	sysPrompt := conv.SystemPrompt
	if len(toolSpecs) > 0 {
		sysPrompt += agent.ToolGuidance
		if _, hasCode := s.Tools.Get("code_run"); hasCode {
			sysPrompt += agent.CodeRunGuidance
		}
	}
	return agent.BuildContext(sysPrompt, s.HistoryToLLM(hist, len(toolSpecs) > 0, visionOn, userID)), toolSpecs
}

// --- history replay ----------------------------------------------------------------

// historyToLLM maps persisted rows to OpenAI protocol messages with strict
// pairing: an assistant call replays only if its tool answer exists, and a
// tool row replays only if some replayed assistant call matches it. A crash
// between the two writes therefore degrades to plain text instead of
// poisoning every later request with an orphan protocol row. When tools are
// disabled the replay strips all protocol furniture — the endpoint is never
// told about tools in that mode. userID owns the conversation; it is the
// sandbox root owner for the attachment manifests. visionOn is the running
// model's entry flag (§7.2): with it off the manifest names no view_image
// and no synthetic turn embeds bytes — history replayed to a text-only
// model is text-only, whatever model wrote the rows.
func (s *Service) HistoryToLLM(hist []store.Message, toolsEnabled, visionOn bool, userID string) []llm.ChatMessage {
	answered := map[string]bool{}
	for _, m := range hist {
		if m.Role == "tool" && m.ToolCallID != "" {
			answered[m.ToolCallID] = true
		}
	}
	// Pass 1: which calls survive (answered AND from an assistant row).
	replayed := map[string]bool{}
	for _, m := range hist {
		if m.Role != "assistant" {
			continue
		}
		for _, c := range m.Calls {
			if c.LLMCallID != "" && answered[c.LLMCallID] {
				replayed[c.LLMCallID] = true
			}
		}
	}
	// Pass 2: emit, pairing both directions on `replayed`.
	out := make([]llm.ChatMessage, 0, len(hist))
	for _, m := range hist {
		switch m.Role {
		case "tool":
			if !replayed[m.ToolCallID] {
				continue // orphan: its assistant call is gone or denied
			}
			out = append(out, llm.ChatMessage{Role: "tool", Content: m.Content, ToolCallID: m.ToolCallID})
		case "assistant":
			var replay []llm.ChatToolCall
			for _, c := range m.Calls {
				if replayed[c.LLMCallID] {
					replay = append(replay, llm.ChatToolCall{
						ID: c.LLMCallID, Type: "function",
						Function: llm.ChatToolCallFunc{Name: c.Tool, Arguments: c.Arguments},
					})
				}
			}
			out = append(out, llm.ChatMessage{Role: "assistant", Content: m.Content, ToolCalls: replay})
		default:
			content := m.Content
			var imgs []string
			if m.Role == "user" && len(m.Attachments) > 0 {
				// Uploads ride as manifest lines; only synthetic rows
				// (view_image turns, §7.2) carry their bytes, decided by
				// the DB flag, not by sniffing the note text.
				embed := visionOn && m.Synthetic
				var manifest string
				manifest, imgs = attachmentManifest(s.cfg.Scratchpad.Root, userID,
					m.Attachments, manifestOpts{visionOn: visionOn, embed: embed,
						maxBytes: s.visionMaxBytes()})
				content += manifest
			}
			out = append(out, llm.ChatMessage{Role: m.Role, Content: content, Images: imgs})
		}
	}
	if !toolsEnabled {
		// Strip protocol furniture when tools are off, but never lose the
		// assistant's actual words: only purely-protocol rows disappear.
		clean := make([]llm.ChatMessage, 0, len(out))
		for _, m := range out {
			if m.Role == "tool" {
				continue
			}
			if len(m.ToolCalls) > 0 {
				m.ToolCalls = nil
				if m.Content == "" {
					continue
				}
			}
			clean = append(clean, m)
		}
		return clean
	}
	return out
}

// jsonQuote marshals one string, for embedding sanitized error text.
func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

func int64ptr(v int64) *int64 { return &v }

type manifestOpts struct {
	visionOn bool
	embed    bool
	maxBytes int64
}

// attachmentManifest tells the model where a turn's attachments live so
// it can fs_read them (§7.3). The manifest is generated server-side;
// upload CONTENT remains untrusted data. Size and existence come from
// the disk — the single source of truth: an attachment that vanished
// since (the file, or a work/ TTL sweep) is skipped in silence (the
// historical chat chip stays, the model is simply not told about bytes
// that are not there).
//
// Uploads NEVER travel as pictures: since view_image (§7.2) the model
// asks for the ones it wants, so image bytes cross the wire only when
// they matter to the answer. o.embed is that asking made flesh — used
// for the synthetic view_image turn, whose persisted marker makes
// historyToLLM re-embed it on every replay. A missing or oversized file
// simply drops out of urls (and the inline=yes annotation), so the
// message degrades to manifest-only, a plain string on the wire.
func attachmentManifest(root, userID string, atts []store.Attachment,
	o manifestOpts,
) (string, []string) {
	var b strings.Builder
	var urls []string
	head := "\n\n[Attached files — stored in your scratchpad; open them with fs_read"
	if o.visionOn {
		head += "; to see an image, call view_image with its path"
	}
	b.WriteString(head + ":]")
	for _, a := range atts {
		fi, err := scratch.Stat(root, userID, a.Path)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		note := ""
		if o.embed {
			if url, ok := imageDataURL(root, userID, a.Path, o.maxBytes); ok {
				urls = append(urls, url)
				note = " inline=yes"
			}
		}
		b.WriteString("\n- name=" + jsonQuote(a.Name()) +
			" path=" + jsonQuote(a.Path) +
			" media=" + scratch.MediaTypeForPath(a.Path) +
			" bytes=" + strconv.FormatInt(fi.Size(), 10) + note)
	}
	return b.String(), urls
}

// imageDataURL reads a sandboxed file (§9.7 open chain) and encodes it as
// a data: URL when its SNIFFED content type is a raster image endpoints
// consume. The type comes from the bytes (http.DetectContentType, spec
// §9.4), never the extension: an SVG or HTML file wearing a .png name is
// not a picture, and active content must never reach the model's context
// as one. Reads are capped: a file that keeps reading past maxBytes is
// refused whole, not truncated into a corrupt image.
func imageDataURL(root, userID, rel string, maxBytes int64) (string, bool) {
	f, err := scratch.OpenFile(root, userID, rel)
	if err != nil {
		return "", false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil || int64(len(data)) > maxBytes || len(data) == 0 {
		return "", false
	}
	mime := http.DetectContentType(data)
	switch mime {
	case "image/png", "image/jpeg", "image/webp":
	default:
		return "", false
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), true
}

// --- POST /api/v1/messages/{id}/regenerate (spec §5.2) ------------------------------
