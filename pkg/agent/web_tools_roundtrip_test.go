package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// Exercise the same Accumulate -> ToParam path as the agent loop, as well as
// non-streaming responses. These fixtures cover the bugs that required the fork.
func TestWebToolRoundTripPreservesContent(t *testing.T) {
	fixtures := map[string]string{
		"search results":       `{"type":"web_search_tool_result","tool_use_id":"srvtoolu_search","caller":{"type":"direct"},"content":[{"type":"web_search_result","url":"https://example.com","title":"Example","encrypted_content":"opaque","page_age":"1 day ago"}]}`,
		"empty search results": `{"type":"web_search_tool_result","tool_use_id":"srvtoolu_search","content":[]}`,
		"search error":         `{"type":"web_search_tool_result","tool_use_id":"srvtoolu_search","content":{"type":"web_search_tool_result_error","error_code":"too_many_requests"}}`,
		"fetched document":     `{"type":"web_fetch_tool_result","tool_use_id":"srvtoolu_fetch","caller":{"type":"direct"},"content":{"type":"web_fetch_result","url":"https://example.com","retrieved_at":"2026-09-30T12:00:00Z","content":{"type":"document","title":"Example","source":{"type":"text","media_type":"text/plain","data":"Fetched document"},"citations":{"enabled":true}}}}`,
		"fetch error":          `{"type":"web_fetch_tool_result","tool_use_id":"srvtoolu_fetch","caller":{"type":"direct"},"content":{"type":"web_fetch_tool_result_error","error_code":"url_not_accessible"}}`,
		"search citation":      `{"type":"text","text":"Cited answer","citations":[{"type":"web_search_result_location","cited_text":"Example","title":"Example","url":"https://example.com","encrypted_index":"opaque-index"}]}`,
	}
	for name, block := range fixtures {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/streaming=%t", name, streaming), func(t *testing.T) {
				var msg anthropic.Message
				if streaming {
					events := []string{
						`{"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}`,
						`{"type":"content_block_start","index":0,"content_block":` + block + `}`,
						`{"type":"content_block_stop","index":0}`,
						`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`,
						`{"type":"message_stop"}`,
					}
					for _, raw := range events {
						var event anthropic.MessageStreamEventUnion
						if err := json.Unmarshal([]byte(raw), &event); err != nil {
							t.Fatal(err)
						}
						if err := msg.Accumulate(event); err != nil {
							t.Fatal(err)
						}
					}
				} else {
					raw := `{"id":"msg_test","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[` + block + `],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`
					if err := json.Unmarshal([]byte(raw), &msg); err != nil {
						t.Fatal(err)
					}
				}
				data, err := json.Marshal(msg.ToParam())
				if err != nil {
					t.Fatal(err)
				}
				var replay struct {
					Content []map[string]any `json:"content"`
				}
				if err := json.Unmarshal(data, &replay); err != nil {
					t.Fatal(err)
				}
				var want map[string]any
				if err := json.Unmarshal([]byte(block), &want); err != nil {
					t.Fatal(err)
				}
				if len(replay.Content) != 1 || !reflect.DeepEqual(replay.Content[0], want) {
					t.Fatalf("tool content changed during replay:\nwant %s\ngot  %s", block, data)
				}
			})
		}
	}
}
