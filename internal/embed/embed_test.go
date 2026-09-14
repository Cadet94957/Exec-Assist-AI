package embed_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Cadet94957/Exec-Assist-AI/internal/embed"
)

type stub struct {
	vec []float32
	err error
}

func (s stub) Name() string { return "stub" }
func (s stub) Embed(context.Context, string) ([]float32, error) {
	return s.vec, s.err
}

// memoryweb POSTs {"model","input"} and reads back {"embeddings":[[...]]}. If
// this shape drifts, memoryweb silently falls back to literal search and recall
// quietly gets worse without anything failing — so pin it.
func TestHandlerSpeaksOllamaShape(t *testing.T) {
	srv := httptest.NewServer(embed.Handler(stub{vec: make([]float32, embed.Dimensions)}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/embed", "application/json",
		strings.NewReader(`{"model":"snowflake-arctic-embed","input":"Harborview gate code"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Embeddings) != 1 {
		t.Fatalf("got %d embeddings, want exactly 1", len(got.Embeddings))
	}
	if len(got.Embeddings[0]) != embed.Dimensions {
		t.Errorf("got %d dimensions, want %d — memoryweb's schema stores nothing else",
			len(got.Embeddings[0]), embed.Dimensions)
	}
}

// A non-200 is the contract for "use literal search instead". It must not be a
// 500 storm and must not hang.
func TestDisabledProviderReturnsUnavailable(t *testing.T) {
	srv := httptest.NewServer(embed.Handler(embed.Disabled{}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/embed", "application/json",
		strings.NewReader(`{"model":"x","input":"anything"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestProviderErrorBecomesUnavailable(t *testing.T) {
	srv := httptest.NewServer(embed.Handler(stub{err: errors.New("provider exploded")}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/embed", "application/json",
		strings.NewReader(`{"model":"x","input":"anything"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestHandlerRejectsBadInput(t *testing.T) {
	srv := httptest.NewServer(embed.Handler(stub{vec: make([]float32, embed.Dimensions)}))
	defer srv.Close()

	for _, body := range []string{`not json`, `{"model":"x","input":""}`} {
		resp, err := http.Post(srv.URL+"/api/embed", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post %q: %v", body, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, resp.StatusCode)
		}
	}
}

func TestFromEnvDefaultsToDisabled(t *testing.T) {
	if _, ok := embed.FromEnv("", "").(embed.Disabled); !ok {
		t.Error("an unset api key should yield the Disabled provider, not a live one")
	}
	if _, ok := embed.FromEnv("key-123", "").(embed.Disabled); ok {
		t.Error("a set api key should yield a live provider")
	}
}
