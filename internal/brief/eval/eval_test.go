//go:build eval

// Run with:  make eval
//
// This calls the real API and costs real money, which is why it is behind a
// build tag: `make check` must stay free and offline so it is run constantly.
package eval_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/brief"
	"github.com/Cadet94957/Exec-Assist-AI/internal/brief/eval"
	"github.com/Cadet94957/Exec-Assist-AI/internal/llm"
	"github.com/Cadet94957/Exec-Assist-AI/internal/obs"
)

func TestBriefPrompt(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY is not set")
	}

	client, err := llm.New(llm.Config{
		APIKey:  key,
		Model:   os.Getenv("ANTHROPIC_MODEL"),
		BaseURL: os.Getenv("ANTHROPIC_BASE_URL"),
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	var (
		totalCost  int64
		totalCalls int
		failures   int
	)

	for _, c := range eval.Cases() {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			started := time.Now()
			out, err := brief.Render(ctx, client, c.Input)
			elapsed := time.Since(started)
			if err != nil {
				t.Fatalf("render: %v", err)
			}

			cost := obs.CostMicros(obs.AICall{
				Model:      client.Model(),
				Input:      out.Usage.Input,
				Output:     out.Usage.Output,
				CacheRead:  out.Usage.CacheRead,
				CacheWrite: out.Usage.CacheWrite,
			})
			totalCost += cost
			totalCalls++

			t.Logf("%s  %.1fs  $%.4f  (%d in, %d cached, %d out)",
				c.Name, elapsed.Seconds(), float64(cost)/1e6,
				out.Usage.Input, out.Usage.CacheRead, out.Usage.Output)
			t.Logf("headline: %s", out.Headline)
			t.Logf("body:\n%s", out.Body)

			for _, res := range c.Check(eval.InputText(c.Input), out) {
				if res.Passed {
					t.Logf("  PASS  %s", res.Name)
					continue
				}
				failures++
				t.Errorf("  FAIL  %s — %s", res.Name, res.Detail)
			}
		})
	}

	fmt.Printf("\n%d cases, %d checks failed, $%.4f total (%d calls)\n",
		len(eval.Cases()), failures, float64(totalCost)/1e6, totalCalls)
}
