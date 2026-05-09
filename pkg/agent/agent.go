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
}

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

// Run sends a single-turn message to Claude and returns the parsed result.
// The transcript (request + response + accounting) is written to
// cfg.TranscriptDir as JSONL when set.
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

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: maxTokens,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(cfg.Prompt)),
		},
	}
	if cfg.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: cfg.System}}
	}

	startedAt := time.Now().UTC()
	msg, err := client.Messages.New(ctx, params)
	finishedAt := time.Now().UTC()
	if err != nil {
		res.Error = err
		res.ExitStatus = 1
		res.Stderr = err.Error()
		writeTranscriptOnError(cfg, model, startedAt, finishedAt, err)
		return res, err
	}

	var sb strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	res.Stdout = sb.String()
	res.InputTokens = int(msg.Usage.InputTokens)
	res.OutputTokens = int(msg.Usage.OutputTokens)
	res.CacheReadIn = int(msg.Usage.CacheReadInputTokens)
	res.CacheWriteIn = int(msg.Usage.CacheCreationInputTokens)
	res.StopReason = string(msg.StopReason)
	res.CostUSD = computeCost(model, res.InputTokens, res.OutputTokens, res.CacheWriteIn, res.CacheReadIn)

	if path, werr := writeTranscript(cfg, model, startedAt, finishedAt, msg, res); werr == nil {
		res.TranscriptPath = path
	}

	if cfg.BudgetUSD > 0 && res.CostUSD > cfg.BudgetUSD {
		res.Error = fmt.Errorf("%w: $%.4f > $%.2f", ErrBudgetExceeded, res.CostUSD, cfg.BudgetUSD)
		res.ExitStatus = 1
		return res, res.Error
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

func writeTranscript(cfg Config, model string, started, finished time.Time, msg *anthropic.Message, res Result) (string, error) {
	path, err := transcriptPath(cfg)
	if err != nil || path == "" {
		return "", err
	}
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	_ = enc.Encode(map[string]any{
		"type":       "request",
		"started_at": started,
		"model":      model,
		"system":     cfg.System,
		"prompt":     cfg.Prompt,
		"max_tokens": cfg.MaxTokens,
	})
	_ = enc.Encode(map[string]any{
		"type":        "response",
		"finished_at": finished,
		"id":          msg.ID,
		"stop_reason": msg.StopReason,
		"content":     msg.Content,
		"usage":       msg.Usage,
	})
	_ = enc.Encode(map[string]any{
		"type":          "accounting",
		"input_tokens":  res.InputTokens,
		"output_tokens": res.OutputTokens,
		"cache_read":    res.CacheReadIn,
		"cache_write":   res.CacheWriteIn,
		"cost_usd":      res.CostUSD,
	})
	return path, nil
}

func writeTranscriptOnError(cfg Config, model string, started, finished time.Time, runErr error) {
	path, err := transcriptPath(cfg)
	if err != nil || path == "" {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	_ = enc.Encode(map[string]any{
		"type":       "request",
		"started_at": started,
		"model":      model,
		"system":     cfg.System,
		"prompt":     cfg.Prompt,
		"max_tokens": cfg.MaxTokens,
	})
	_ = enc.Encode(map[string]any{
		"type":        "error",
		"finished_at": finished,
		"error":       runErr.Error(),
	})
}
