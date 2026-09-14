// Package llm is the single place this product talks to Claude.
//
// Everything here goes through one shape: a system prompt, some evidence, and
// exactly one tool the model is asked to call. Extraction and (later) the
// morning brief both want structured output rather than prose, and a tool with
// a strict schema is the most reliable way to get it.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// DefaultModel is the reasoning model. Quality here is the product: a brief
// that misses the thing that mattered is worse than no brief.
const DefaultModel = "claude-opus-5"

// Config holds what is needed to reach the API.
type Config struct {
	APIKey string
	Model  string
	// BaseURL overrides the API endpoint. Empty means Anthropic. Tests point
	// it at a local server so the whole call path runs offline.
	BaseURL string
}

// Configured reports whether an API key is present.
func (c Config) Configured() bool { return c.APIKey != "" }

// Client talks to Claude.
type Client struct {
	api   anthropic.Client
	model string
}

// New builds a client from config.
func New(cfg Config) (*Client, error) {
	if !cfg.Configured() {
		return nil, errors.New("llm: ANTHROPIC_API_KEY is not set")
	}
	model := cfg.Model
	if model == "" {
		model = DefaultModel
	}

	opts := []option.RequestOption{option.WithAPIKey(cfg.APIKey)}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	return &Client{api: anthropic.NewClient(opts...), model: model}, nil
}

// Model reports which model this client calls.
func (c *Client) Model() string { return c.model }

// ToolRequest asks the model to call exactly one tool and returns its argument.
type ToolRequest struct {
	// System is the instruction set. It is cached, so keep it byte-identical
	// across calls and put everything that varies in Evidence.
	System string
	// Evidence is the data to reason over. This is the volatile part and must
	// come after the cache breakpoint.
	Evidence string
	// ToolName, ToolDescription and Schema define the single tool on offer.
	ToolName        string
	ToolDescription string
	Schema          map[string]any
	// Required names the schema's required properties.
	Required []string
	// MaxTokens bounds the reply. Extraction replies are small.
	MaxTokens int64
}

// Usage reports what a call cost, so spend per tenant is observable rather
// than a surprise on the bill.
type Usage struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// CallTool runs one request and returns the raw JSON the model passed to the
// tool. It returns nil when the model chose not to call the tool, which is a
// legitimate outcome — for extraction it means "nothing here worth recording".
func (c *Client) CallTool(ctx context.Context, req ToolRequest) (json.RawMessage, Usage, error) {
	if req.MaxTokens == 0 {
		req.MaxTokens = 8000
	}

	schema := anthropic.ToolInputSchemaParam{Properties: req.Schema}
	if len(req.Required) > 0 {
		schema.Required = req.Required
	}
	// additionalProperties:false is required for a strict schema, and strict
	// is what guarantees the arguments actually match what we declared.
	schema.ExtraFields = map[string]any{"additionalProperties": false}

	tool := anthropic.ToolParam{
		Name:        req.ToolName,
		Description: anthropic.String(req.ToolDescription),
		InputSchema: schema,
		Strict:      anthropic.Bool(true),
	}

	resp, err := c.api.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: req.MaxTokens,
		System: []anthropic.TextBlockParam{{
			Text: req.System,
			// The system prompt is identical on every call, so caching it
			// turns the bulk of the input cost into a cache read.
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}},
		Tools: []anthropic.ToolUnionParam{{OfTool: &tool}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(req.Evidence)),
		},
	})
	if err != nil {
		return nil, Usage{}, fmt.Errorf("llm: call %s: %w", req.ToolName, err)
	}

	usage := Usage{
		Input:      resp.Usage.InputTokens,
		Output:     resp.Usage.OutputTokens,
		CacheRead:  resp.Usage.CacheReadInputTokens,
		CacheWrite: resp.Usage.CacheCreationInputTokens,
	}

	// A refusal is an HTTP 200 with a stop reason, not an error — check it
	// before reading content or the result looks like an empty answer.
	if resp.StopReason == anthropic.StopReasonRefusal {
		return nil, usage, fmt.Errorf("llm: model declined the request")
	}

	for _, block := range resp.Content {
		if variant, ok := block.AsAny().(anthropic.ToolUseBlock); ok {
			if variant.Name == req.ToolName {
				return json.RawMessage(variant.JSON.Input.Raw()), usage, nil
			}
		}
	}

	log.Printf("[llm] %s: model returned no tool call (stop_reason=%s)", req.ToolName, resp.StopReason)
	return nil, usage, nil
}
