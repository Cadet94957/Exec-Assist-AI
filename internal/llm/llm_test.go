package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Cadet94957/Exec-Assist-AI/internal/llm"
)

// stubAPI answers every request with the given stop reason and content, and
// records the request body so tests can see what was actually sent.
func stubAPI(t *testing.T, stopReason string, content []any) (*llm.Client, *map[string]any) {
	t.Helper()
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &sent)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": llm.DefaultModel,
			"content": content, "stop_reason": stopReason,
			"usage": map[string]any{"input_tokens": 900, "output_tokens": 16000},
		})
	}))
	t.Cleanup(srv.Close)

	c, err := llm.New(llm.Config{APIKey: "test-key", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c, &sent
}

func request() llm.ToolRequest {
	return llm.ToolRequest{
		System:   "Write the brief.",
		Evidence: "Three stops today.",
		ToolName: "write_brief",
		Schema:   map[string]any{"body": map[string]any{"type": "string"}},
		Required: []string{"body"},
	}
}

// A response cut off by max_tokens has to be reported as that, not as "the
// model chose not to call the tool" — which is what a quiet day looks like, and
// would make a truncated brief on a busy day impossible to tell apart from one.
func TestTruncationIsItsOwnError(t *testing.T) {
	// Opus 5 thinks before it answers, so a cut-off response can hold nothing
	// but a thinking block.
	c, _ := stubAPI(t, "max_tokens", []any{
		map[string]any{"type": "thinking", "thinking": "", "signature": "sig"},
	})

	raw, _, err := c.CallTool(context.Background(), request())
	if !errors.Is(err, llm.ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
	if raw != nil {
		t.Errorf("returned %s from a truncated response", raw)
	}
}

// Declining to call the tool is a legitimate outcome — for extraction it means
// there was nothing worth recording — and must stay distinct from truncation.
func TestNoToolCallIsNotAnError(t *testing.T) {
	c, _ := stubAPI(t, "end_turn", []any{
		map[string]any{"type": "text", "text": "Nothing to record."},
	})

	raw, _, err := c.CallTool(context.Background(), request())
	if err != nil || raw != nil {
		t.Fatalf("got (%s, %v), want (nil, nil)", raw, err)
	}
}

// Thinking counts against max_tokens, so a request that leaves the ceiling
// unset must still leave room for it.
func TestDefaultCeilingLeavesRoomToThink(t *testing.T) {
	c, sent := stubAPI(t, "tool_use", []any{
		map[string]any{"type": "tool_use", "id": "t1", "name": "write_brief", "input": map[string]any{"body": "ok"}},
	})

	if _, _, err := c.CallTool(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if got := (*sent)["max_tokens"]; got != float64(16000) {
		t.Errorf("max_tokens sent = %v, want 16000", got)
	}
}
