package cronicle

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jshiv/cronicle/pkg/agent"
	"github.com/jshiv/cronicle/pkg/exec"
)

// MaxToolOutputBytes caps how much bash output we feed back to the model in
// a single tool_result. Outputs exceeding this are middle-truncated (head
// + tail preserved, middle dropped with a marker). Matches Claude Code's
// 30K default — keeps the next-turn context bounded against runaway commands.
const MaxToolOutputBytes = 30 * 1024

// ansiCSI matches ANSI CSI escape sequences (color codes, cursor moves).
// Stripped from bash output before truncation so the byte count reflects
// real content, not control bytes the model can't use anyway.
var ansiCSI = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

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
//
// The combined output is ANSI-stripped and middle-truncated to
// MaxToolOutputBytes so a runaway command can't pollute the next turn's
// context. Cancellation propagates: when ctx is done (typically because the
// agent's wallclock fired), the bash process group is signalled SIGTERM and
// then SIGKILL.
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
	res := exec.ExecuteWithStreamContext(ctx, cmd, b.Workspace, b.Env, b.StdoutW, b.StderrW)

	stdout := stripANSI(res.Stdout)
	stderr := stripANSI(res.Stderr)

	var sb strings.Builder
	if res.ExitStatus != 0 {
		fmt.Fprintf(&sb, "exit_code: %d\n", res.ExitStatus)
	}
	if stdout != "" {
		sb.WriteString(stdout)
	}
	if stderr != "" {
		sb.WriteString("\n--- stderr ---\n")
		sb.WriteString(stderr)
	}
	if sb.Len() == 0 {
		sb.WriteString("(no output)")
	}
	return truncateOutput(sb.String(), MaxToolOutputBytes), res.ExitStatus != 0
}

// stripANSI removes ANSI CSI escape sequences from s.
func stripANSI(s string) string {
	if !strings.ContainsRune(s, 0x1b) {
		return s
	}
	return ansiCSI.ReplaceAllString(s, "")
}

// truncateOutput returns s unchanged when it fits in max bytes; otherwise it
// keeps the first and last halves of max with a marker noting how many
// bytes were dropped from the middle. Middle-keep matters for shell
// commands where both the start (the command framing) and the end (the
// final lines, exit context) are usually the most informative.
func truncateOutput(s string, max int) string {
	if len(s) <= max {
		return s
	}
	dropped := len(s) - max
	half := max / 2
	return fmt.Sprintf("%s\n\n... [%d bytes truncated] ...\n\n%s",
		s[:half], dropped, s[len(s)-half:])
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
