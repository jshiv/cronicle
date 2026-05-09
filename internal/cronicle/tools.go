package cronicle

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jshiv/cronicle/pkg/agent"
	"github.com/jshiv/cronicle/pkg/exec"
)

// BashTool implements agent.Tool by wrapping pkg/exec.ExecuteWithStream so
// that bash invoked by an agent runs through the exact same execution path
// as a shell task: cwd=workspace, env, stream-aware writers. Output flows
// to the same stdoutW used by the agent's pretty-mode renderer when set,
// otherwise it's just captured into the result string.
type BashTool struct {
	Workspace string
	Env       []string
	StdoutW   io.Writer // optional; for live pretty-mode streaming of bash output
	StderrW   io.Writer // optional
}

// Name is "bash" (matches Anthropic-defined tool name).
func (b *BashTool) Name() string { return "bash" }

// Definition returns the SDK's bash_20250124 tool definition.
func (b *BashTool) Definition() anthropic.ToolUnionParam {
	return anthropic.ToolUnionParam{
		OfBashTool20250124: &anthropic.ToolBash20250124Param{},
	}
}

// Execute runs the requested command and returns a tool_result string the
// model can read on the next turn. Format:
//
//	exit_code: <n>          (only when non-zero)
//	<stdout>
//	--- stderr ---           (only when stderr non-empty)
//	<stderr>
func (b *BashTool) Execute(ctx context.Context, input json.RawMessage) (string, bool) {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Sprintf("Error: invalid bash input: %v", err), true
	}
	if args.Command == "" {
		return "Error: empty command", true
	}

	cmd := []string{"/bin/sh", "-c", args.Command}
	res := exec.ExecuteWithStream(cmd, b.Workspace, b.Env, b.StdoutW, b.StderrW)

	var sb strings.Builder
	if res.ExitStatus != 0 {
		fmt.Fprintf(&sb, "exit_code: %d\n", res.ExitStatus)
	}
	if res.Stdout != "" {
		sb.WriteString(res.Stdout)
	}
	if res.Stderr != "" {
		sb.WriteString("\n--- stderr ---\n")
		sb.WriteString(res.Stderr)
	}
	if sb.Len() == 0 {
		// No output at all but exit=0 — still tell the model the command ran.
		sb.WriteString("(no output)")
	}
	return sb.String(), res.ExitStatus != 0
}

// buildAgentTools converts the HCL `tools` field into a slice of agent.Tool
// implementations bound to the given workspace and stream writer (when in
// pretty streaming mode). Unknown tools are filtered out (Validate has
// already rejected them at parse time, so this is defensive).
func buildAgentTools(names []string, workspace string, env []string, w io.Writer) []agent.Tool {
	if len(names) == 0 {
		return nil
	}
	out := make([]agent.Tool, 0, len(names))
	for _, name := range names {
		switch name {
		case "bash":
			out = append(out, &BashTool{
				Workspace: workspace,
				Env:       env,
				StdoutW:   w,
				StderrW:   w,
			})
		}
	}
	return out
}
