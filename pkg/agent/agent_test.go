package agent

import "testing"

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
		// Already in the table verbatim — no strip.
		"claude-haiku-4-5-20251001": "claude-haiku-4-5-20251001",
		// Not a date — passthrough (cost falls back to $0).
		"unknown-model":        "unknown-model",
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

// computeCost yields a positive number for known models and zero for
// unknown ones (no panic, no negative).
func TestComputeCostKnownAndUnknown(t *testing.T) {
	if got := computeCost("claude-opus-4-7", 1000, 500, 0, 0); got <= 0 {
		t.Fatalf("known model cost should be > 0, got %f", got)
	}
	if got := computeCost("not-a-model", 1000, 500, 0, 0); got != 0 {
		t.Fatalf("unknown model cost should be 0, got %f", got)
	}
	// Dated revision routes through priceKey.
	if got := computeCost("claude-opus-4-7-20260301", 1000, 500, 0, 0); got <= 0 {
		t.Fatalf("dated revision cost should be > 0, got %f", got)
	}
}
