// Package embed serves an Ollama-shaped embeddings endpoint backed by a hosted
// provider.
//
// memoryweb generates embeddings by POSTing {"model","input"} to an Ollama
// /api/embed endpoint and reading back {"embeddings":[[...]]}. Rather than run
// and babysit an Ollama process per host, we speak that shape ourselves on a
// loopback listener and translate to a hosted provider.
//
// Two constraints are load-bearing:
//
//   - memoryweb stores 1024-dimensional vectors and nothing else, so a provider
//     must return exactly 1024 floats.
//   - memoryweb treats an embedding failure as a signal to fall back to literal
//     LIKE search rather than an error. Returning a non-200 here degrades
//     recall; it does not break the product.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// Dimensions is the vector width memoryweb's schema is built around.
const Dimensions = 1024

// Provider turns text into a 1024-dimensional vector.
type Provider interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	// Name identifies the provider in logs and on the health page.
	Name() string
}

// ErrDisabled reports that no embedding provider is configured.
var ErrDisabled = errors.New("embed: no provider configured")

// Disabled is the zero-cost provider. With it, memoryweb falls back to literal
// matching — recall still works, it is just less associative. This is a
// legitimate way to run before there are enough tenants to justify the spend.
type Disabled struct{}

func (Disabled) Name() string { return "disabled" }

// Embed always fails, which memoryweb reads as "use LIKE search".
func (Disabled) Embed(context.Context, string) ([]float32, error) { return nil, ErrDisabled }

// Voyage calls Voyage AI's embeddings API. voyage-3.5 and voyage-3.5-lite
// default to 1024 dimensions, which is exactly what memoryweb needs.
type Voyage struct {
	APIKey string
	Model  string
	client *http.Client
}

// NewVoyage builds a Voyage provider. Model defaults to voyage-3.5-lite, which
// is the cheap end and more than good enough for recalling business facts.
func NewVoyage(apiKey, model string) *Voyage {
	if model == "" {
		model = "voyage-3.5-lite"
	}
	return &Voyage{
		APIKey: apiKey,
		Model:  model,
		client: &http.Client{Timeout: 20 * time.Second},
	}
}

func (v *Voyage) Name() string { return "voyage:" + v.Model }

type voyageRequest struct {
	Input           []string `json:"input"`
	Model           string   `json:"model"`
	OutputDimension int      `json:"output_dimension"`
	InputType       string   `json:"input_type,omitempty"`
}

type voyageResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
}

// Embed returns a single 1024-dimensional vector for text.
func (v *Voyage) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(voyageRequest{
		Input:           []string{text},
		Model:           v.Model,
		OutputDimension: Dimensions,
		InputType:       "document",
	})
	if err != nil {
		return nil, fmt.Errorf("embed: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.voyageai.com/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embed: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+v.APIKey)

	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: call voyage: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("embed: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed: voyage returned %d: %s", resp.StatusCode, raw)
	}

	var out voyageResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("embed: decode response: %w", err)
	}
	if len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
		return nil, errors.New("embed: voyage returned no embedding")
	}
	if got := len(out.Data[0].Embedding); got != Dimensions {
		return nil, fmt.Errorf("embed: expected %d dimensions, got %d", Dimensions, got)
	}
	return out.Data[0].Embedding, nil
}

// --- the Ollama-shaped surface memoryweb talks to ---

type ollamaRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type ollamaResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// Handler serves POST /api/embed in Ollama's shape, backed by p.
//
// Mount this on a loopback-only listener and point MEMORYWEB_OLLAMA_ENDPOINT
// at it. It must never be exposed publicly: it would let anyone spend your
// embedding budget.
func Handler(p Provider) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/embed", func(w http.ResponseWriter, r *http.Request) {
		var req ollamaRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.Input == "" {
			http.Error(w, "empty input", http.StatusBadRequest)
			return
		}

		vec, err := p.Embed(r.Context(), req.Input)
		if err != nil {
			if !errors.Is(err, ErrDisabled) {
				// Worth seeing in logs: it means recall quietly got worse.
				log.Printf("[embed] %s failed, falling back to literal search: %v", p.Name(), err)
			}
			http.Error(w, "embedding unavailable", http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(ollamaResponse{Embeddings: [][]float32{vec}}); err != nil {
			log.Printf("[embed] write response: %v", err)
		}
	})
	return mux
}

// FromEnv picks a provider from configuration. An unset VOYAGE_API_KEY yields
// Disabled, so the product runs end to end with no embedding spend.
func FromEnv(apiKey, model string) Provider {
	if apiKey == "" {
		return Disabled{}
	}
	return NewVoyage(apiKey, model)
}
