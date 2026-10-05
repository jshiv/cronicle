package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

type echoTool struct{ calls int }

func (e *echoTool) Name() string { return "echo" }
func (e *echoTool) Definition() anthropic.ToolUnionParam {
	return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{Name: "echo", InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{"value": map[string]any{"type": "string"}}, Required: []string{"value"}}}}
}
func (e *echoTool) Execute(_ context.Context, input json.RawMessage) (string, bool) {
	e.calls++
	return string(input), false
}

func openAIServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	t.Setenv("OPENAI_BASE_URL", s.URL+"/")
	t.Setenv("OPENAI_API_KEY", "env-test-key")
}
func emitResponse(w http.ResponseWriter, status string, output []any) {
	w.Header().Set("Content-Type", "text/event-stream")
	event := map[string]any{"type": "response." + status, "response": map[string]any{"id": "resp_test", "status": status, "model": "gpt-4.1-mini", "output": output, "usage": map[string]any{"input_tokens": 100, "output_tokens": 10, "input_tokens_details": map[string]any{"cached_tokens": 20}}}}
	b, _ := json.Marshal(event)
	fmt.Fprintf(w, "event: response.%s\ndata: %s\n\n", status, b)
}
func functionOutput() []any {
	return []any{
		map[string]any{"type": "reasoning", "id": "rs_1", "summary": []any{}, "encrypted_content": "opaque"},
		map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "echo", "arguments": `{"value":"hello"}`, "status": "completed"},
	}
}
func textOutput() []any {
	return []any{map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "OK", "annotations": []any{}}}}}
}

func TestOpenAIToolLoop(t *testing.T) {
	tool := &echoTool{}
	calls := 0
	var events []StreamEvent
	openAIServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer explicit-test-key" {
			t.Errorf("wrong route/auth: %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["model"] != "gpt-4.1-mini" || body["instructions"] != "Be concise" || body["store"] != false || body["stream"] != true {
			t.Errorf("bad request: %v", body)
		}
		if calls == 1 {
			emitResponse(w, "completed", functionOutput())
			return
		}
		encoded, _ := json.Marshal(body["input"])
		for _, want := range []string{"function_call_output", "call_1", "opaque", "hello"} {
			if !strings.Contains(string(encoded), want) {
				t.Errorf("history missing %s: %s", want, encoded)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n")
		emitResponse(w, "completed", textOutput())
	})
	res, err := Run(context.Background(), Config{Provider: "openai", Prompt: "Echo hello", System: "Be concise", APIKey: "explicit-test-key", Tools: []Tool{tool}, TranscriptDir: t.TempDir(), StreamHandler: func(e StreamEvent) { events = append(events, e) }})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || tool.calls != 1 || res.Stdout != "OK" || res.ExitStatus != 0 {
		t.Fatalf("calls=%d tool=%d result=%+v", calls, tool.calls, res)
	}
	if res.InputTokens != 160 || res.CacheReadIn != 40 || res.OutputTokens != 20 || math.Abs(res.CostUSD-0.0001) > 1e-9 {
		t.Errorf("incorrect accounting: %+v", res)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, string(e.Type))
	}
	if strings.Join(kinds, ",") != "tool_use_start,tool_result,turn_start,text_delta" {
		t.Errorf("events: %v", kinds)
	}
	transcript, err := os.ReadFile(res.TranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"type":"response"`, `"type":"tool_result"`, `"type":"accounting"`} {
		if !strings.Contains(string(transcript), want) {
			t.Errorf("transcript missing %s", want)
		}
	}
	if strings.Contains(string(transcript), "explicit-test-key") {
		t.Fatal("key leaked into transcript")
	}
}

func TestOpenAIBudgetStopsBeforeTool(t *testing.T) {
	tool := &echoTool{}
	openAIServer(t, func(w http.ResponseWriter, r *http.Request) { emitResponse(w, "completed", functionOutput()) })
	res, err := Run(context.Background(), Config{Provider: "openai", Prompt: "hi", Tools: []Tool{tool}, BudgetUSD: 0.000001})
	if !errors.Is(err, ErrBudgetExceeded) || tool.calls != 0 || res.ExitStatus != 1 {
		t.Fatalf("result=%+v err=%v tool=%d", res, err, tool.calls)
	}
}
func TestOpenAIIncompleteAndTruncatedStreamsFail(t *testing.T) {
	for _, status := range []string{"incomplete", "failed", "truncated"} {
		t.Run(status, func(t *testing.T) {
			tool := &echoTool{}
			openAIServer(t, func(w http.ResponseWriter, r *http.Request) {
				if status == "truncated" {
					w.Header().Set("Content-Type", "text/event-stream")
					return
				}
				emitResponse(w, status, functionOutput())
			})
			res, err := Run(context.Background(), Config{Provider: "openai", Prompt: "hi", Tools: []Tool{tool}})
			if err == nil || res.ExitStatus != 1 || tool.calls != 0 {
				t.Fatalf("result=%+v err=%v tool=%d", res, err, tool.calls)
			}
		})
	}
}
func TestOpenAIMaxTurnsAndCancellation(t *testing.T) {
	tool := &echoTool{}
	openAIServer(t, func(w http.ResponseWriter, r *http.Request) { emitResponse(w, "completed", functionOutput()) })
	res, err := Run(context.Background(), Config{Provider: "openai", Prompt: "hi", Tools: []Tool{tool}, MaxTurns: 1})
	if err == nil || res.ExitStatus != 1 || tool.calls != 0 {
		t.Fatalf("limit must stop before unconsumed tool: %+v %v calls=%d", res, err, tool.calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Run(ctx, Config{Provider: "openai", Prompt: "hi"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
func TestUnknownProviderFails(t *testing.T) {
	_, err := Run(context.Background(), Config{Provider: "typo", Prompt: "hi"})
	if err == nil {
		t.Fatal("unknown provider accepted")
	}
}

func TestOpenAIHTTPError(t *testing.T) {
	openAIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer env-test-key" {
			t.Error("SDK did not use OPENAI_API_KEY")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":{"message":"invalid test key","type":"invalid_request_error","code":"invalid_api_key"}}`)
	})
	res, err := Run(context.Background(), Config{Provider: "openai", Prompt: "hi"})
	if err == nil || res.ExitStatus != 1 || !strings.Contains(res.Stderr, "invalid test key") {
		t.Fatalf("result=%+v err=%v", res, err)
	}
}
func TestOpenAIUnknownPricingBudgetRejected(t *testing.T) {
	openAIServer(t, func(w http.ResponseWriter, r *http.Request) { t.Error("must not call an unpriced model with a budget") })
	_, err := Run(context.Background(), Config{Provider: "openai", Model: "unpriced-model", Prompt: "hi", BudgetUSD: 1})
	if err == nil || !strings.Contains(err.Error(), "pricing") {
		t.Fatalf("got %v", err)
	}
	for _, model := range []string{"gpt-4.1-mini", "gpt-4.1-mini-2025-04-14", "gpt-4o-mini-2024-07-18"} {
		if p, ok := openAIPrice(model); !ok || p.in <= 0 {
			t.Errorf("missing price for %s", model)
		}
	}
}
func TestOpenAIUnsupportedHostedTool(t *testing.T) {
	openAIServer(t, func(w http.ResponseWriter, r *http.Request) { t.Error("must not call API with unsupported tool") })
	_, err := Run(context.Background(), Config{Provider: "openai", Prompt: "hi", Tools: []Tool{&hostedTestTool{}}})
	if err == nil || !strings.Contains(err.Error(), "function definition") {
		t.Fatalf("got %v", err)
	}
}

type hostedTestTool struct{ echoTool }

func (hostedTestTool) Definition() anthropic.ToolUnionParam {
	return anthropic.ToolUnionParam{OfWebSearchTool20250305: &anthropic.WebSearchTool20250305Param{}}
}

func TestOpenAIToolErrorsReturnToModel(t *testing.T) {
	for _, kind := range []string{"unknown", "invalid-json"} {
		t.Run(kind, func(t *testing.T) {
			tool := &echoTool{}
			requests := 0
			openAIServer(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				if requests == 1 {
					output := functionOutput()
					call := output[1].(map[string]any)
					if kind == "unknown" {
						call["name"] = "missing"
					} else {
						call["arguments"] = "not-json"
					}
					emitResponse(w, "completed", output)
					return
				}
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				encoded, _ := json.Marshal(body["input"])
				if !strings.Contains(string(encoded), "Tool error:") {
					t.Errorf("error not returned: %s", encoded)
				}
				emitResponse(w, "completed", textOutput())
			})
			_, err := Run(context.Background(), Config{Provider: "openai", Prompt: "hi", Tools: []Tool{tool}})
			if err != nil || requests != 2 || tool.calls != 0 {
				t.Fatalf("err=%v requests=%d tool calls=%d", err, requests, tool.calls)
			}
		})
	}
}
func TestOpenAICancelsInFlightRequest(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	openAIServer(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-entered; cancel() }()
	_, err := Run(ctx, Config{Provider: "openai", Prompt: "hi"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
