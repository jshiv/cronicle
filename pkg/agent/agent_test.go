package agent

import (
	"math"
	"testing"
)

// priceKey strips a trailing -YYYYMMDD revision so dated ids cost the same
// as their named alias — without this, any new dated revision would silently
// cost $0 in the run accounting.
func TestPriceKey(t *testing.T) {
	cases := map[string]string{
		// Direct hit.
		"claude-opus-4-7":   "claude-opus-4-7",
		"claude-sonnet-4-6": "claude-sonnet-4-6",
		// Dated revision → strip to alias.
		"claude-opus-4-7-20260301": "claude-opus-4-7",
		// Dated snapshots of 4.5-generation models resolve to their alias.
		"claude-haiku-4-5-20251001":  "claude-haiku-4-5",
		"claude-sonnet-4-5-20250929": "claude-sonnet-4-5",
		"claude-3-5-haiku-20241022":  "claude-3-5-haiku",
		// Not a date — passthrough (cost is then estimated).
		"unknown-model":         "unknown-model",
		"claude-3-5-not-a-date": "claude-3-5-not-a-date",
		// Short string — passthrough.
		"x": "x",
	}
	for in, want := range cases {
		if got := priceKey(in); got != want {
			t.Fatalf("priceKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// computeCost prices known models exactly, and estimates unknown ones —
// never $0, so budget_usd still bounds a model the table hasn't caught
// up with.
func TestComputeCostKnownAndUnknown(t *testing.T) {
	if got, est := computeCost("claude-opus-4-7", 1000, 500, 0, 0); got <= 0 || est {
		t.Fatalf("known model: cost %f estimated %v", got, est)
	}
	// Dated revision routes through priceKey: exact, not estimated.
	if got, est := computeCost("claude-opus-4-7-20260301", 1000, 500, 0, 0); got <= 0 || est {
		t.Fatalf("dated revision: cost %f estimated %v", got, est)
	}
	sonnet, _ := computeCost("claude-sonnet-4-6", 1000, 500, 0, 0)
	opus, _ := computeCost("claude-opus-4-7", 1000, 500, 0, 0)
	fable, _ := computeCost("claude-fable-5-1", 1000, 500, 0, 0)
	for model, want := range map[string]float64{
		"claude-sonnet-9":     sonnet, // a new model is priced like its family's priciest current model
		"claude-opus-9-1":     opus,   // not like retired Opus 4.1 at $75/MTok out
		"some-future-model-x": fable,  // no family: the priciest current model
	} {
		got, est := computeCost(model, 1000, 500, 0, 0)
		if !est || got != want {
			t.Errorf("%s: cost %f estimated %v, want %f estimated", model, got, est, want)
		}
	}
}

// TestPricing_ListPrices pins a few list prices (per 1M tokens) from
// platform.claude.com/docs/en/about-claude/pricing.
func TestPricing_ListPrices(t *testing.T) {
	for model, want := range map[string][2]float64{
		"claude-opus-5-5":   {4, 20},
		"claude-opus-4-7":   {5, 25}, // was mispriced at $15 / $75
		"claude-sonnet-5-5": {2, 10},
		"claude-haiku-4-5":  {1, 5},
		"claude-fable-5-1":  {10, 50},
	} {
		in, _ := computeCost(model, 1_000_000, 0, 0, 0)
		out, _ := computeCost(model, 0, 1_000_000, 0, 0)
		if in != want[0] || out != want[1] {
			t.Errorf("%s: $%g in / $%g out per MTok, want $%g / $%g", model, in, out, want[0], want[1])
		}
	}
}

// TestPricing_HaikuLongPromptTier: Haiku 5.5 charges more for a request
// whose prompt (input + cache) is over 100K tokens, request by request.
func TestPricing_HaikuLongPromptTier(t *testing.T) {
	short, _ := computeCost("claude-haiku-5-5", 100_000, 1_000, 0, 0)
	long, _ := computeCost("claude-haiku-5-5", 60_000, 1_000, 0, 40_001)
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-12 }
	if want := (100_000*0.10 + 1_000*0.50) / 1e6; !near(short, want) {
		t.Errorf("at the threshold: %g, want %g", short, want)
	}
	if want := (60_000*0.50 + 1_000*2.50 + 40_001*0.05) / 1e6; !near(long, want) {
		t.Errorf("over it (cache reads count): %g, want %g", long, want)
	}
}

func TestPricing_OpenAIEstimates(t *testing.T) {
	for model, basisWant := range map[string]string{
		"gpt-7":        "gpt-6-astra", // no family: the priciest current non-pro model
		"gpt-7-mini":   "gpt-5.4-mini",
		"gpt-7-pro":    "gpt-5.4-pro",
		"gpt-6.2-luna": "gpt-5.6-luna",
	} {
		if _, basis := estimatePrice(openAIPricing, model,
			[]string{"pro", "nano", "mini", "luna", "terra", "sol", "astra", "codex"}, []string{"pro"}); basis != basisWant {
			t.Errorf("%s estimated from %s, want %s", model, basis, basisWant)
		}
	}
}
