package agent

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// TestWebSearchMultiTurnRoundTrip reproduces the bug from
// anthropics/anthropic-sdk-go#346: multi-turn conversations with
// web_search_tool_result fail on the second API call because ToParam()
// produces content the API rejects.
//
// This test uses the raw JSON passthrough (msgToParam) to verify the
// workaround works.
func TestWebSearchMultiTurnRoundTrip(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}

	client := anthropic.NewClient()
	ctx := context.Background()

	// Turn 1: ask something that triggers web_search
	t.Log("Turn 1: sending message with web_search tool...")
	msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeSonnet4_6,
		MaxTokens: 1024,
		Tools: []anthropic.ToolUnionParam{
			{OfWebSearchTool20250305: &anthropic.WebSearchTool20250305Param{
				Name: "web_search",
				Type: "web_search_20250305",
			}},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(
				anthropic.NewTextBlock("Search the web for 'anthropic claude' and tell me what you find. Keep it brief."),
			),
		},
	})
	if err != nil {
		t.Fatalf("turn 1 failed: %v", err)
	}

	t.Logf("Turn 1 succeeded: stop_reason=%s, %d content blocks", msg.StopReason, len(msg.Content))

	// Check that web_search_tool_result is present
	hasWebSearch := false
	for _, block := range msg.Content {
		if block.Type == "web_search_tool_result" {
			hasWebSearch = true
			break
		}
	}
	if !hasWebSearch {
		t.Skip("model didn't use web_search on this turn — can't test round-trip")
	}

	// Build conversation using ToParam() — the SDK fork uses raw JSON passthrough
	conversation := []anthropic.MessageParam{
		anthropic.NewUserMessage(
			anthropic.NewTextBlock("Search the web for 'anthropic claude' and tell me what you find. Keep it brief."),
		),
		msg.ToParam(),
		anthropic.NewUserMessage(
			anthropic.NewTextBlock("Thanks. Summarize in one sentence."),
		),
	}

	// Turn 2: this is where the bug manifests — ToParam() produces invalid content
	t.Log("Turn 2: sending conversation history with web_search_tool_result...")
	msg2, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeSonnet4_6,
		MaxTokens: 256,
		Tools: []anthropic.ToolUnionParam{
			{OfWebSearchTool20250305: &anthropic.WebSearchTool20250305Param{
				Name: "web_search",
				Type: "web_search_20250305",
			}},
		},
		Messages: conversation,
	})
	if err != nil {
		t.Fatalf("turn 2 FAILED: %v", err)
	}

	t.Logf("Turn 2 succeeded: stop_reason=%s, %d content blocks", msg2.StopReason, len(msg2.Content))

}

// TestToParamPreservesWebSearchContent verifies that ToParam (via the SDK
// fork's raw JSON passthrough) produces valid JSON with web_search content.
func TestToParamPreservesWebSearchContent(t *testing.T) {
	apiResponse := []byte(`{
		"id": "msg_test",
		"type": "message",
		"role": "assistant",
		"model": "claude-sonnet-4-6",
		"stop_reason": "end_turn",
		"content": [
			{
				"type": "web_search_tool_result",
				"tool_use_id": "srvtoolu_123",
				"content": [
					{
						"type": "web_search_result",
						"url": "https://example.com",
						"title": "Test",
						"encrypted_content": "dGVzdA=="
					}
				]
			}
		],
		"usage": {"input_tokens": 100, "output_tokens": 50}
	}`)

	var msg anthropic.Message
	if err := json.Unmarshal(apiResponse, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	mp := msg.ToParam()
	data, err := json.Marshal(mp)
	if err != nil {
		t.Fatalf("marshal param: %v", err)
	}
	if !json.Valid(data) {
		t.Fatal("invalid JSON produced")
	}
	t.Logf("ToParam output: %s", string(data))

	// Verify content block preserved
	var raw map[string]json.RawMessage
	json.Unmarshal(data, &raw)
	var blocks []json.RawMessage
	json.Unmarshal(raw["content"], &blocks)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(blocks))
	}
	var block map[string]json.RawMessage
	json.Unmarshal(blocks[0], &block)
	if string(block["type"]) != `"web_search_tool_result"` {
		t.Fatalf("expected web_search_tool_result, got %s", string(block["type"]))
	}
	if block["content"] == nil {
		t.Fatal("web_search_tool_result.content is missing")
	}
}
