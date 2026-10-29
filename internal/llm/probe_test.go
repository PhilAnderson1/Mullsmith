package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"mullsmith/internal/config"
)

func modelsServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func probeCfg(ts *httptest.Server) *config.Config {
	return &config.Config{LLM: config.LLM{Endpoint: ts.URL + "/v1"}}
}

func TestProbeModelsReadsWindows(t *testing.T) {
	// One listing, three vendor spellings, plus a model that says nothing.
	ts := modelsServer(t, `{"data":[
		{"id":"llama","context_length":131072},
		{"id":"vllm","max_model_len":8192},
		{"id":"old","meta":{"n_ctx":2048}},
		{"id":"mute"}
	]}`)
	models, err := ProbeModels(context.Background(), probeCfg(ts))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"llama": 131072, "vllm": 8192, "old": 2048, "mute": 0}
	if len(models) != len(want) {
		t.Fatalf("got %d models, want %d", len(models), len(want))
	}
	for _, m := range models {
		if want[m.ID] != m.ContextLength {
			t.Errorf("%s window = %d, want %d", m.ID, m.ContextLength, want[m.ID])
		}
	}
	if w, ok := ContextLengthFor(models, "llama"); !ok || w != 131072 {
		t.Errorf("ContextLengthFor(llama) = %d,%v", w, ok)
	}
	// Silent model and unknown id both answer "not reported".
	if _, ok := ContextLengthFor(models, "mute"); ok {
		t.Error("mute model reported a window")
	}
	if _, ok := ContextLengthFor(models, "ghost"); ok {
		t.Error("unknown id reported a window")
	}
}

func TestProbeStillReturnsIDSOnly(t *testing.T) {
	ts := modelsServer(t, `{"data":[{"id":"a","context_length":9},{"id":"b"}]}`)
	ids, err := Probe(context.Background(), probeCfg(ts))
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Errorf("ids = %v", ids)
	}
}

func TestProbeModelsEndpointDown(t *testing.T) {
	ts := modelsServer(t, `{"data":[]}`)
	ts.Close() // now unreachable
	if _, err := ProbeModels(context.Background(), probeCfg(ts)); err == nil {
		t.Error("closed endpoint must return an error")
	}
}
