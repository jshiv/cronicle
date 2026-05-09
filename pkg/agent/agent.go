// Package agent runs a Claude agent invocation and returns its result alongside
// token/cost accounting and a JSONL transcript.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/jshiv/cronicle/pkg/exec"
)

const (
	DefaultModel     = "claude-opus-4-7"
	DefaultMaxTokens = 4096
)

// Config carries the inputs for a single agent invocation.
type Config struct {
	Prompt        string
	System        string
	Model         string
	MaxTokens     int
	BudgetUSD     float64
	APIKey        string
	TranscriptDir string
	RunID         string
	// StreamHandler, when non-nil, is called with semantic streaming events
	// (text/thinking deltas, tool use start/delta/stop) as they arrive from
	// the API. Use it to render deltas in real time. The aggregated Result
	// is still returned by Run regardless. nil = silent (still streamed
	// internally; no per-delta callback fired).
	StreamHandler StreamHandler
	// Tools is the set of tools the model can invoke. Each tool's
	// Definition is sent on every turn; Execute is called when the model
	// emits a matching tool_use block. When empty, the agent runs single
	// turn (no tool dispatch).
	Tools []Tool
	// MaxTurns bounds the agent loop iterations. Default 1 when no Tools,
	// otherwise treated as 30. The loop also exits naturally when the
	// model's stop_reason != "tool_use".
	MaxTurns int
}

// Tool is an executable tool the agent can invoke. Implementations live in
// the cronicle wrapper (so they can use pkg/exec etc. without an import
// cycle on this package).
type Tool interface {
	// Name is the tool name as the model sees it (e.g. "bash").
	Name() string
	// Definition returns the SDK tool param union (one of the Anthropic-
	// defined tool variants like ToolBash20250124Param, or a custom
	// ToolParam).
	Definition() anthropic.ToolUnionParam
	// Execute runs the tool with input from the model. The returned string
	// becomes the tool_result content; isError reports whether the tool
	// failed (the model gets a tool_result with is_error=true and can
	// recover).
	Execute(ctx context.Context, input json.RawMessage) (output string, isError bool)
}

// StreamEventType identifies the kind of streaming event.
type StreamEventType string

const (
	StreamEventTextDelta     StreamEventType = "text_delta"
	StreamEventThinkingDelta StreamEventType = "thinking_delta"
	StreamEventToolUseStart  StreamEventType = "tool_use_start"
	StreamEventToolUseDelta  StreamEventType = "tool_use_delta"
	StreamEventToolUseStop   StreamEventType = "tool_use_stop"
	// StreamEventToolResult fires after cronicle has executed a tool and
	// has the result string ready. Not from the API stream — emitted by
	// the agent loop itself.
	StreamEventToolResult StreamEventType = "tool_result"
	// StreamEventTurnStart fires at the top of each loop iteration after
	// the first. Useful for renderers that want to delineate turns.
	StreamEventTurnStart StreamEventType = "turn_start"
)

// StreamEvent is a single semantic event from an agent run.
type StreamEvent struct {
	Type       StreamEventType
	Text       string // text_delta and thinking_delta carry the partial text here
	ToolID     string // tool_use_* events
	ToolName   string // tool_use_start; also tool_result
	ToolInput  string // tool_use_delta carries partial JSON here
	ToolOutput string // tool_result: the executed tool's output
	IsError    bool   // tool_result: did the tool fail
	DurationMs int64  // tool_result: execution time
	TurnIndex  int    // turn_start: 0-based turn number
}

// StreamHandler is called with each StreamEvent as it arrives.
type StreamHandler func(StreamEvent)

// Result is what an agent run produces. It embeds exec.Result so it can drop
// into the rest of cronicle's task execution path; the agent-specific fields
// (tokens, cost, transcript path) are additive.
type Result struct {
	exec.Result
	Model          string
	InputTokens    int
	OutputTokens   int
	CacheReadIn    int
	CacheWriteIn   int
	CostUSD        float64
	StopReason     string
	TranscriptPath string
}

// modelPrice is per-1M-token USD pricing. Centralized so a model swap doesn't
// silently mis-cost. Update when Anthropic publishes new pricing.
type modelPrice struct {
	in, out, cacheWrite, cacheRead float64
}

var pricing = map[string]modelPrice{
	"claude-opus-4-7":           {15.00, 75.00, 18.75, 1.50},
	"claude-opus-4-6":           {15.00, 75.00, 18.75, 1.50},
	"claude-sonnet-4-6":         {3.00, 15.00, 3.75, 0.30},
	"claude-haiku-4-5-20251001": {1.00, 5.00, 1.25, 0.10},
	"claude-haiku-4-5":          {1.00, 5.00, 1.25, 0.10},
}

// ErrBudgetExceeded is returned when the run's actual cost crosses cfg.BudgetUSD.
var ErrBudgetExceeded = errors.New("agent run exceeded configured budget")

// Run drives the multi-turn agent loop: send conversation, stream the
// response, dispatch any tool_use blocks to the configured Tools, append
// tool_result blocks back into the conversation, and continue until the
// model stops calling tools, MaxTurns is reached, the context is cancelled
// (for wallclock bounds), or BudgetUSD is exceeded mid-run. With no Tools
// configured, the loop terminates after one turn (single-turn behavior
// preserved). The transcript captures every turn and tool result.
func Run(ctx context.Context, cfg Config) (Result, error) {
	model := cfg.Model
	if model == "" {
		model = DefaultModel
	}
	maxTokens := int64(cfg.MaxTokens)
	if maxTokens == 0 {
		maxTokens = DefaultMaxTokens
	}

	res := Result{
		Result: exec.Result{Command: []string{"agent", model}},
		Model:  model,
	}

	var opts []option.RequestOption
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	client := anthropic.NewClient(opts...)

	toolMap := make(map[string]Tool, len(cfg.Tools))
	var toolDefs []anthropic.ToolUnionParam
	for _, t := range cfg.Tools {
		toolMap[t.Name()] = t
		toolDefs = append(toolDefs, t.Definition())
	}

	maxTurns := cfg.MaxTurns
	if maxTurns <= 0 {
		if len(cfg.Tools) > 0 {
			maxTurns = 30
		} else {
			maxTurns = 1
		}
	}

	conversation := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock(cfg.Prompt)),
	}

	startedAt := time.Now().UTC()
	tw, _ := openTranscript(cfg, model, startedAt)
	if tw != nil {
		defer tw.close()
		res.TranscriptPath = tw.path
	}

	var (
		accText   strings.Builder
		cumUsage  anthropic.Usage
		finalStop anthropic.StopReason
		turnsRun  int
		runErr    error
	)

	for turn := 0; turn < maxTurns; turn++ {
		if turn > 0 && cfg.StreamHandler != nil {
			cfg.StreamHandler(StreamEvent{Type: StreamEventTurnStart, TurnIndex: turn})
		}

		params := anthropic.MessageNewParams{
			Model:     anthropic.Model(model),
			MaxTokens: maxTokens,
			Messages:  conversation,
		}
		if cfg.System != "" {
			params.System = []anthropic.TextBlockParam{{Text: cfg.System}}
		}
		if len(toolDefs) > 0 {
			params.Tools = toolDefs
		}

		msg := anthropic.Message{}

		stream := client.Messages.NewStreaming(ctx, params)
		for stream.Next() {
			event := stream.Current()
			_ = msg.Accumulate(event)

			// During the stream we only fire text / thinking deltas. Tool
			// events fire later from the dispatch loop with the fully
			// parsed input, so each tool's header / body / result renders
			// as an atomic block even when the model emits multiple
			// tool_use blocks in one turn.
			if event.Type != "content_block_delta" {
				continue
			}
			switch event.Delta.Type {
			case "text_delta":
				accText.WriteString(event.Delta.Text)
				if cfg.StreamHandler != nil {
					cfg.StreamHandler(StreamEvent{Type: StreamEventTextDelta, Text: event.Delta.Text})
				}
			case "thinking_delta":
				if cfg.StreamHandler != nil {
					cfg.StreamHandler(StreamEvent{Type: StreamEventThinkingDelta, Text: event.Delta.Thinking})
				}
			}
		}

		if streamErr := stream.Err(); streamErr != nil {
			runErr = streamErr
			res.Stderr = streamErr.Error()
			if tw != nil {
				tw.writeError(streamErr, time.Now().UTC())
			}
			break
		}

		turnsRun = turn + 1
		finalStop = msg.StopReason

		cumUsage.InputTokens += msg.Usage.InputTokens
		cumUsage.OutputTokens += msg.Usage.OutputTokens
		cumUsage.CacheCreationInputTokens += msg.Usage.CacheCreationInputTokens
		cumUsage.CacheReadInputTokens += msg.Usage.CacheReadInputTokens

		if tw != nil {
			tw.writeResponse(turn, &msg, time.Now().UTC())
		}

		// Mid-run budget abort
		currentCost := computeCost(model,
			int(cumUsage.InputTokens), int(cumUsage.OutputTokens),
			int(cumUsage.CacheCreationInputTokens), int(cumUsage.CacheReadInputTokens))
		if cfg.BudgetUSD > 0 && currentCost > cfg.BudgetUSD {
			runErr = fmt.Errorf("%w: $%.4f > $%.2f after turn %d",
				ErrBudgetExceeded, currentCost, cfg.BudgetUSD, turn+1)
			conversation = append(conversation, msg.ToParam())
			break
		}

		conversation = append(conversation, msg.ToParam())

		if msg.StopReason != "tool_use" {
			break
		}

		// Dispatch tool_use blocks; assemble tool_result blocks.
		var results []anthropic.ContentBlockParamUnion
		for _, block := range msg.Content {
			if block.Type != "tool_use" {
				continue
			}
			tu := block.AsToolUse()
			tool, ok := toolMap[tu.Name]

			// Fire tool_use_start NOW (post-stream) so the renderer sees
			// header → output → result as one atomic block per tool.
			if cfg.StreamHandler != nil {
				cfg.StreamHandler(StreamEvent{
					Type:      StreamEventToolUseStart,
					ToolID:    tu.ID,
					ToolName:  tu.Name,
					ToolInput: string(tu.Input),
				})
			}

			var (
				output  string
				isError bool
			)
			var toolDurationMs int64
			if !ok {
				output = fmt.Sprintf("Error: tool %q not registered", tu.Name)
				isError = true
			} else {
				toolStart := time.Now()
				output, isError = tool.Execute(ctx, tu.Input)
				toolDurationMs = time.Since(toolStart).Milliseconds()
			}
			if tw != nil {
				tw.writeToolResult(turn, tu.ID, tu.Name, output, isError)
			}
			if cfg.StreamHandler != nil {
				cfg.StreamHandler(StreamEvent{
					Type:       StreamEventToolResult,
					ToolID:     tu.ID,
					ToolName:   tu.Name,
					ToolOutput: output,
					IsError:    isError,
					DurationMs: toolDurationMs,
				})
			}
			results = append(results, anthropic.NewToolResultBlock(tu.ID, output, isError))
		}
		if len(results) == 0 {
			// Defensive: stop_reason said tool_use but no blocks present.
			break
		}
		conversation = append(conversation, anthropic.NewUserMessage(results...))
	}

	finishedAt := time.Now().UTC()

	res.Stdout = accText.String()
	res.InputTokens = int(cumUsage.InputTokens)
	res.OutputTokens = int(cumUsage.OutputTokens)
	res.CacheReadIn = int(cumUsage.CacheReadInputTokens)
	res.CacheWriteIn = int(cumUsage.CacheCreationInputTokens)
	res.StopReason = string(finalStop)
	res.CostUSD = computeCost(model, res.InputTokens, res.OutputTokens, res.CacheWriteIn, res.CacheReadIn)

	if tw != nil {
		tw.writeAccounting(turnsRun, res, finishedAt)
	}

	if runErr != nil {
		res.Error = runErr
		res.ExitStatus = 1
		return res, runErr
	}
	return res, nil
}

func computeCost(model string, in, out, cacheWrite, cacheRead int) float64 {
	p, ok := pricing[model]
	if !ok {
		return 0
	}
	return (float64(in)*p.in +
		float64(out)*p.out +
		float64(cacheWrite)*p.cacheWrite +
		float64(cacheRead)*p.cacheRead) / 1_000_000
}

func transcriptPath(cfg Config) (string, error) {
	if cfg.TranscriptDir == "" {
		return "", nil
	}
	if err := os.MkdirAll(cfg.TranscriptDir, 0o755); err != nil {
		return "", err
	}
	name := cfg.RunID
	if name == "" {
		name = time.Now().UTC().Format("20060102T150405Z")
	}
	return filepath.Join(cfg.TranscriptDir, name+".jsonl"), nil
}

// transcriptWriter is a per-run JSONL writer that records every turn's
// request/response, every tool result, and a final accounting line. The file
// is opened at the start of Run and appended to incrementally so that even
// crashed/aborted runs leave a partial transcript on disk.
type transcriptWriter struct {
	path string
	f    *os.File
	enc  *json.Encoder
}

func openTranscript(cfg Config, model string, started time.Time) (*transcriptWriter, error) {
	path, err := transcriptPath(cfg)
	if err != nil || path == "" {
		return nil, err
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	tw := &transcriptWriter{path: path, f: f, enc: json.NewEncoder(f)}
	_ = tw.enc.Encode(map[string]any{
		"type":       "request",
		"started_at": started,
		"model":      model,
		"system":     cfg.System,
		"prompt":     cfg.Prompt,
		"max_tokens": cfg.MaxTokens,
		"tools":      toolNames(cfg.Tools),
		"max_turns":  cfg.MaxTurns,
	})
	return tw, nil
}

func (tw *transcriptWriter) writeResponse(turn int, msg *anthropic.Message, finished time.Time) {
	_ = tw.enc.Encode(map[string]any{
		"type":        "response",
		"turn":        turn,
		"finished_at": finished,
		"id":          msg.ID,
		"stop_reason": msg.StopReason,
		"content":     msg.Content,
		"usage":       msg.Usage,
	})
}

func (tw *transcriptWriter) writeToolResult(turn int, toolUseID, name, output string, isError bool) {
	_ = tw.enc.Encode(map[string]any{
		"type":        "tool_result",
		"turn":        turn,
		"tool_use_id": toolUseID,
		"tool_name":   name,
		"output":      output,
		"is_error":    isError,
	})
}

func (tw *transcriptWriter) writeAccounting(turns int, res Result, finished time.Time) {
	_ = tw.enc.Encode(map[string]any{
		"type":          "accounting",
		"turns":         turns,
		"finished_at":   finished,
		"input_tokens":  res.InputTokens,
		"output_tokens": res.OutputTokens,
		"cache_read":    res.CacheReadIn,
		"cache_write":   res.CacheWriteIn,
		"cost_usd":      res.CostUSD,
	})
}

func (tw *transcriptWriter) writeError(err error, finished time.Time) {
	_ = tw.enc.Encode(map[string]any{
		"type":        "error",
		"finished_at": finished,
		"error":       err.Error(),
	})
}

func (tw *transcriptWriter) close() {
	if tw == nil || tw.f == nil {
		return
	}
	_ = tw.f.Close()
}

func toolNames(tools []Tool) []string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name()
	}
	return names
}
