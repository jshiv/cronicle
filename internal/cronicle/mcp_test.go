package cronicle

import (
	"testing"
)

// mcpSchemaToAnthropic round-trips a typical JSON-Schema object plus
// keywords beyond the supported core: additionalProperties, $defs etc.
// These must reach Anthropic via ExtraFields; otherwise MCP servers see
// schemas different from what they declared.
func TestMCPSchemaToAnthropic(t *testing.T) {
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "where to read",
			},
		},
		"required":             []any{"path"},
		"additionalProperties": false,
	}
	out, err := mcpSchemaToAnthropic(in)
	if err != nil {
		t.Fatalf("mcpSchemaToAnthropic: %v", err)
	}
	if len(out.Required) != 1 || out.Required[0] != "path" {
		t.Fatalf("Required: got %v", out.Required)
	}
	if out.Properties == nil {
		t.Fatalf("Properties dropped")
	}
	if got, ok := out.ExtraFields["additionalProperties"]; !ok || got != false {
		t.Fatalf("additionalProperties not forwarded via ExtraFields: %v", out.ExtraFields)
	}

	// nil input degrades cleanly.
	if _, err := mcpSchemaToAnthropic(nil); err != nil {
		t.Fatalf("nil schema: %v", err)
	}
}

// MCPServerNames returns sorted server labels; nil/empty handles produce
// no allocation.
func TestMCPServerNames(t *testing.T) {
	if MCPServerNames(nil) != nil {
		t.Fatalf("nil handles not nil-out")
	}
	got := MCPServerNames([]*MCPHandle{
		{Name: "github"},
		{Name: "fs"},
		{Name: "alpha"},
	})
	want := []string{"alpha", "fs", "github"}
	if len(got) != len(want) {
		t.Fatalf("len: got %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("at [%d]: got %q, want %q", i, got[i], w)
		}
	}
}

// isValidToolNamePart enforces the regex Anthropic uses on tool names so
// our `<server>__<tool>` namespacing won't construct names the API rejects.
func TestIsValidToolNamePart(t *testing.T) {
	cases := map[string]bool{
		"github":     true,
		"my-server":  true,
		"my_server":  true,
		"server123":  true,
		"":           false,
		"has space":  false,
		"has.period": false,
		"has/slash":  false,
		"has@at":     false,
	}
	for in, want := range cases {
		if got := isValidToolNamePart(in); got != want {
			t.Fatalf("isValidToolNamePart(%q) = %v, want %v", in, got, want)
		}
	}
}
