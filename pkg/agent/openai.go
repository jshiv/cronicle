package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jshiv/cronicle/pkg/exec"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

const DefaultOpenAIModel = "gpt-4.1-mini"

// openAIPricing is OpenAI's standard-tier price per 1M text tokens: input,
// output, and cached input (counted separately from input). A model with
// no cached-input price is charged full input for cached tokens. From
// https://developers.openai.com/api/docs/pricing (2026-10-23), for
// prompts within the short-context limit; longer prompts on some models
// cost more and aren't modeled. Snapshots (-YYYY-MM-DD) resolve to their
// base model unless listed.
var openAIPricing = map[string]modelPrice{
	"gpt-6-astra":       {in: 10.00, out: 50.00, cacheRead: 1.00},
	"gpt-6.1-sol":       {in: 2.00, out: 10.00, cacheRead: 0.10},
	"gpt-6-sol":         {in: 2.00, out: 10.00, cacheRead: 0.20},
	"gpt-6-luna":        {in: 0.10, out: 0.50, cacheRead: 0.01},
	"gpt-5.6-sol":       {in: 4.00, out: 20.00, cacheRead: 0.40},
	"gpt-5.6-terra":     {in: 2.00, out: 12.00, cacheRead: 0.20},
	"gpt-5.6-luna":      {in: 0.20, out: 1.20, cacheRead: 0.02},
	"gpt-5.5":           {in: 5.00, out: 30.00, cacheRead: 0.50},
	"gpt-5.5-pro":       {in: 30.00, out: 180.00, cacheRead: 30.00},
	"gpt-5.4":           {in: 2.50, out: 15.00, cacheRead: 0.25},
	"gpt-5.4-pro":       {in: 30.00, out: 180.00, cacheRead: 30.00},
	"gpt-5.4-mini":      {in: 0.75, out: 4.50, cacheRead: 0.075},
	"gpt-5.4-nano":      {in: 0.20, out: 1.25, cacheRead: 0.02},
	"gpt-5.3-codex":     {in: 1.75, out: 14.00, cacheRead: 0.175},
	"gpt-5.2":           {in: 1.75, out: 14.00, cacheRead: 0.175},
	"gpt-5.2-pro":       {in: 21.00, out: 168.00, cacheRead: 21.00},
	"gpt-5.1":           {in: 1.25, out: 10.00, cacheRead: 0.125},
	"gpt-5":             {in: 1.25, out: 10.00, cacheRead: 0.125},
	"gpt-5-mini":        {in: 0.25, out: 2.00, cacheRead: 0.025},
	"gpt-5-nano":        {in: 0.05, out: 0.40, cacheRead: 0.005},
	"gpt-5-pro":         {in: 15.00, out: 120.00, cacheRead: 15.00},
	"gpt-4.1":           {in: 2.00, out: 8.00, cacheRead: 0.50, legacy: true},
	"gpt-4.1-mini":      {in: 0.40, out: 1.60, cacheRead: 0.10, legacy: true},
	"gpt-4.1-nano":      {in: 0.10, out: 0.40, cacheRead: 0.025, legacy: true},
	"gpt-4o":            {in: 2.50, out: 10.00, cacheRead: 1.25, legacy: true},
	"gpt-4o-2024-05-13": {in: 5.00, out: 15.00, cacheRead: 5.00, legacy: true},
	"gpt-4o-mini":       {in: 0.15, out: 0.60, cacheRead: 0.075, legacy: true},
	"o1":                {in: 15.00, out: 60.00, cacheRead: 7.50, legacy: true},
	"o1-pro":            {in: 150.00, out: 600.00, cacheRead: 150.00, legacy: true},
	"o3":                {in: 2.00, out: 8.00, cacheRead: 0.50, legacy: true},
	"o3-pro":            {in: 20.00, out: 80.00, cacheRead: 20.00, legacy: true},
	"o3-mini":           {in: 1.10, out: 4.40, cacheRead: 0.55, legacy: true},
	"o4-mini":           {in: 1.10, out: 4.40, cacheRead: 0.275, legacy: true},
}

func openAIPrice(model string) (modelPrice, bool) {
	if p, ok := openAIPricing[model]; ok {
		return p, true
	}
	// OpenAI snapshot suffixes use -YYYY-MM-DD.
	if len(model) > 11 {
		if _, err := time.Parse("2006-01-02", model[len(model)-10:]); err == nil && model[len(model)-11] == '-' {
			p, ok := openAIPricing[model[:len(model)-11]]
			return p, ok
		}
	}
	return modelPrice{}, false
}

// OpenAIHostedTool is a tool that OpenAI runs on its side (web search),
// sent as its own Responses tool type rather than as a function. Calls to
// it come back as output items, never as function calls to dispatch.
type OpenAIHostedTool interface {
	OpenAIDefinition() responses.ToolUnionParam
}

func openAIToolDefinition(tool Tool) (responses.ToolUnionParam, error) {
	if hosted, ok := tool.(OpenAIHostedTool); ok {
		return hosted.OpenAIDefinition(), nil
	}
	var def FunctionDefinition
	if function, ok := tool.(FunctionTool); ok {
		def = function.FunctionDefinition()
	} else if custom := tool.Definition().OfTool; custom != nil {
		def.Description = custom.Description.Value
		raw, err := json.Marshal(custom.InputSchema)
		if err != nil {
			return responses.ToolUnionParam{}, err
		}
		if err = json.Unmarshal(raw, &def.Parameters); err != nil {
			return responses.ToolUnionParam{}, err
		}
	} else {
		return responses.ToolUnionParam{}, fmt.Errorf("tool %q has no OpenAI function definition", tool.Name())
	}
	return responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{
		Name: tool.Name(), Description: openai.String(def.Description), Parameters: def.Parameters,
		// Existing local/MCP schemas may have optional properties. Preserve their
		// semantics instead of silently making every property required.
		Strict: openai.Bool(false),
	}}, nil
}

// runOpenAI uses Responses with explicit conversation history and store=false.
// Including encrypted reasoning preserves reasoning context across tool turns
// without retaining server-side response state.
func runOpenAI(ctx context.Context, cfg Config) (res Result, runErr error) {
	model := cfg.Model
	if model == "" {
		model = DefaultOpenAIModel
	}
	res = Result{Result: exec.Result{Command: []string{"agent", model}}, Model: model}
	var text strings.Builder
	var tw *transcriptWriter
	turns := 0
	defer func() {
		res.Stdout = text.String()
		if runErr != nil {
			res.Error = runErr
			res.Stderr = runErr.Error()
			res.ExitStatus = 1
			if tw != nil {
				tw.writeError(runErr, time.Now().UTC())
			}
		}
		if tw != nil {
			tw.writeAccounting(turns, res, time.Now().UTC())
			tw.close()
		}
	}()
	// A model missing from the pricing table still runs, with its cost
	// estimated from a similar one so budget_usd keeps working (see
	// computeCost). Failing here made every new model a config error.
	price, known := openAIPrice(model)
	if !known {
		var basis string
		price, basis = estimatePrice(openAIPricing, model,
			[]string{"pro", "nano", "mini", "luna", "terra", "sol", "astra", "codex"}, []string{"pro"})
		warnEstimated("openai", model, basis)
		res.CostEstimated = true
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	toolMap := make(map[string]Tool, len(cfg.Tools))
	var defs []responses.ToolUnionParam
	for _, tool := range cfg.Tools {
		def, err := openAIToolDefinition(tool)
		if err != nil {
			return res, err
		}
		defs = append(defs, def)
		toolMap[tool.Name()] = tool
	}
	maxTokens := cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = DefaultMaxTokens
	}
	maxTurns := cfg.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 1
		if len(cfg.Tools) > 0 {
			maxTurns = 30
		}
	}
	opts := []option.RequestOption{option.WithMaxRetries(0)}
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	client := openai.NewClient(opts...)
	tw, runErr = openTranscript(cfg, model, time.Now().UTC())
	if runErr != nil {
		return res, runErr
	}
	if tw != nil {
		res.TranscriptPath = tw.path
	}
	conversation := []responses.ResponseInputItemUnionParam{responses.ResponseInputItemParamOfMessage(cfg.Prompt, "user")}
	emit := func(e StreamEvent) {
		if cfg.StreamHandler != nil {
			cfg.StreamHandler(e)
		}
	}
	for turn := 0; turn < maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if turn > 0 {
			emit(StreamEvent{Type: StreamEventTurnStart, TurnIndex: turn})
		}
		params := responses.ResponseNewParams{
			Model: model, MaxOutputTokens: openai.Int(int64(maxTokens)), Store: openai.Bool(false),
			Input: responses.ResponseNewParamsInputUnion{OfInputItemList: conversation},
			Tools: defs, Include: []responses.ResponseIncludable{"reasoning.encrypted_content"},
		}
		if cfg.System != "" {
			params.Instructions = openai.String(cfg.System)
		}
		stream := client.Responses.NewStreaming(ctx, params)
		var response *responses.Response
		var eventErr error
		for stream.Next() {
			e := stream.Current()
			switch e.Type {
			case "response.output_text.delta":
				text.WriteString(e.Delta)
				emit(StreamEvent{Type: StreamEventTextDelta, Text: e.Delta})
			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				emit(StreamEvent{Type: StreamEventThinkingDelta, Text: e.Delta})
			case "response.completed", "response.failed", "response.incomplete":
				copy := e.Response
				response = &copy
			case "error":
				eventErr = fmt.Errorf("OpenAI stream error: %s: %s", e.Code, e.Message)
			}
		}
		streamErr := stream.Err()
		_ = stream.Close()
		if streamErr != nil {
			return res, withOpenAIMessage(streamErr)
		}
		if eventErr != nil {
			return res, eventErr
		}
		if response == nil {
			return res, fmt.Errorf("OpenAI stream ended without a terminal response: %w", io.ErrUnexpectedEOF)
		}
		turns = turn + 1
		cached := int(response.Usage.InputTokensDetails.CachedTokens)
		res.InputTokens += int(response.Usage.InputTokens) - cached
		res.CacheReadIn += cached
		res.OutputTokens += int(response.Usage.OutputTokens)
		res.CostUSD = (float64(res.InputTokens)*price.in + float64(res.CacheReadIn)*price.cacheRead + float64(res.OutputTokens)*price.out) / 1e6
		res.StopReason = string(response.Status)
		if tw != nil {
			_ = tw.enc.Encode(map[string]any{"type": "response", "turn": turn, "finished_at": time.Now().UTC(), "id": response.ID, "stop_reason": response.Status, "content": response.Output, "usage": response.Usage})
		}
		if response.Status != "completed" {
			return res, fmt.Errorf("OpenAI response %s: %s %s", response.Status, response.Error.Message, response.IncompleteDetails.Reason)
		}
		if cfg.BudgetUSD > 0 && res.CostUSD > cfg.BudgetUSD {
			return res, fmt.Errorf("%w: $%.4f > $%.2f after turn %d", ErrBudgetExceeded, res.CostUSD, cfg.BudgetUSD, turn+1)
		}
		var calls []responses.ResponseFunctionToolCall
		for _, item := range response.Output {
			// Preserve reasoning (including encrypted content) and function call IDs.
			var input responses.ResponseInputItemUnionParam
			if err := json.Unmarshal([]byte(item.RawJSON()), &input); err != nil {
				return res, fmt.Errorf("OpenAI conversation item: %w", err)
			}
			conversation = append(conversation, input)
			switch item.Type {
			case "function_call":
				calls = append(calls, item.AsFunctionCall())
			case "web_search_call":
				emitWebSearch(emit, item.AsWebSearchCall())
			}
		}
		if len(calls) == 0 {
			return res, nil
		}
		if turn+1 == maxTurns {
			res.StopReason = "max_turns"
			return res, fmt.Errorf("OpenAI agent reached max_turns (%d) with pending tool calls", maxTurns)
		}
		for _, call := range calls {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			emit(StreamEvent{Type: StreamEventToolUseStart, ToolID: call.CallID, ToolName: call.Name, ToolInput: call.Arguments})
			started := time.Now()
			output, isError := fmt.Sprintf("Error: tool %q not registered", call.Name), true
			if tool, ok := toolMap[call.Name]; ok {
				if !json.Valid([]byte(call.Arguments)) {
					output = "Error: invalid tool arguments JSON"
				} else {
					output, isError = tool.Execute(ctx, json.RawMessage(call.Arguments))
				}
			}
			if tw != nil {
				tw.writeToolResult(turn, call.CallID, call.Name, output, isError)
			}
			emit(StreamEvent{Type: StreamEventToolResult, ToolID: call.CallID, ToolName: call.Name, ToolOutput: output, IsError: isError, DurationMs: time.Since(started).Milliseconds()})
			// Responses has no is_error flag; return an explicit marker on failures.
			if isError {
				output = "Tool error: " + output
			}
			result := responses.ResponseInputItemParamOfFunctionCallOutput(output)
			result.OfFunctionCallOutput.CallID = openai.String(call.CallID)
			conversation = append(conversation, result)
		}
	}
	return res, nil
}

// emitWebSearch reports a hosted web search OpenAI already ran, so the
// stream shows `→ web_search: <query>` like any other tool call.
func emitWebSearch(emit func(StreamEvent), ws responses.ResponseFunctionWebSearch) {
	input := map[string]string{}
	switch ws.Action.Type {
	case "search":
		input["query"] = ws.Action.Query
		if input["query"] == "" {
			input["query"] = strings.Join(ws.Action.Queries, "; ")
		}
	case "open_page", "find_in_page":
		input["url"] = ws.Action.URL
	}
	raw, _ := json.Marshal(input)
	emit(StreamEvent{Type: StreamEventToolUseStart, ToolID: ws.ID, ToolName: "web_search", ToolInput: string(raw)})
	failed := ws.Status == "failed" || ws.Status == "incomplete"
	emit(StreamEvent{Type: StreamEventToolResult, ToolID: ws.ID, ToolName: "web_search", ToolOutput: string(ws.Status), IsError: failed})
}

// withOpenAIMessage adds the API's own error message ("Incorrect API key
// provided", "model not found") to an API error. Since openai-go v3.71
// the error's text is just the status ("OpenAI API error: 401
// Unauthorized"), which leaves an operator guessing; the message is
// still on the error, and OpenAI masks any key it echoes.
func withOpenAIMessage(err error) error {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) && apiErr.Message != "" {
		return fmt.Errorf("%w: %s", err, apiErr.Message)
	}
	return err
}
