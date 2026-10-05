package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/jshiv/cronicle/pkg/exec"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

const DefaultOpenAIModel = "gpt-4.1-mini"

// Prices per million text tokens; cached input is counted separately from input.
// https://developers.openai.com/api/docs/models/gpt-4.1-mini
// https://developers.openai.com/api/docs/models/gpt-4.1
// https://developers.openai.com/api/docs/models/gpt-4o-mini
var openAIPricing = map[string]modelPrice{
	"gpt-4.1-mini": {in: 0.40, out: 1.60, cacheRead: 0.10},
	"gpt-4.1":      {in: 2.00, out: 8.00, cacheRead: 0.50},
	"gpt-4o-mini":  {in: 0.15, out: 0.60, cacheRead: 0.075},
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

func openAIToolDefinition(tool Tool) (responses.ToolUnionParam, error) {
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
	price, known := openAIPrice(model)
	if cfg.BudgetUSD > 0 && !known {
		return res, fmt.Errorf("OpenAI model %q has no configured pricing; cannot enforce budget_usd", model)
	}
	if !known {
		if _, warned := warnedUnknownModels.LoadOrStore("openai:"+model, struct{}{}); !warned {
			slog.Warn("agent: OpenAI model not in pricing table; cost unavailable (reported as $0)", "model", model)
		}
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
			return res, streamErr
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
			if item.Type == "function_call" {
				calls = append(calls, item.AsFunctionCall())
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
