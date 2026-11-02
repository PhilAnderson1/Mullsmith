package chat

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"mullsmith/internal/agent"
	"mullsmith/internal/id"
	"mullsmith/internal/llm"
)

type cancellationTransport func(*http.Request) (*http.Response, error)

func (f cancellationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestStopCancelsTitleAndReleasesGeneration(t *testing.T) {
	s, _, conv := chatFixture(t)
	entered := make(chan context.Context, 1)
	s.generator = &agent.Generator{LLM: llm.NewClient(s.cfg, cancellationTransport(func(r *http.Request) (*http.Response, error) {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		entered <- r.Context()
		<-r.Context().Done()
		return nil, r.Context().Err()
	}))}
	op, err := s.Begin(t.Context(), "user", Command{ConversationID: conv.ID, Content: "hello", AutoTitle: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	defer op.Cancel()
	done := make(chan struct{})
	go func() { defer close(done); op.Run(func(Event) error { return nil }) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("title did not start")
	}
	op.Cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop left the title or generation active")
	}
	if s.Generations.Count() != 0 {
		t.Fatal("generation slot retained after Stop")
	}
}

func TestShutdownClosesConcurrentGenerationAdmission(t *testing.T) {
	registry := NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Go(func() {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			g, err := registry.Start("conversation-"+id.New(), "user", 0, 0, cancel)
			if errors.Is(err, ErrShuttingDown) {
				return
			}
			if err != nil {
				t.Error(err)
				return
			}
			defer registry.Finish(g)
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Error("shutdown missed an admitted generation")
			}
		})
	}
	registry.Shutdown()
	wg.Wait()
	if _, err := registry.Start("late", "user", 0, 0, func() {}); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("late admission returned %v", err)
	}
	if registry.Shutdown() != 0 {
		t.Fatal("shutdown was not idempotent")
	}
}
