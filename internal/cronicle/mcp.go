// MCP (Model Context Protocol) integration: launch MCP servers as
// subprocesses, list their tools, and wrap them as agent.Tool so the model
// can invoke them through cronicle's existing dispatch loop.
//
// Tools are namespaced as `<server-name>__<tool-name>` so multiple servers
// can be configured on a single agent without name collisions. cronicle
// translates between MCP's JSON-Schema input definitions and Anthropic's
// ToolInputSchemaParam by round-tripping through JSON — the wire formats
// are compatible.
//
// Each server runs for the lifetime of one agent run: it's spawned at the
// start of execAgent and torn down (stdin closed, then SIGTERM after a
// grace period via the SDK's CommandTransport.TerminateDuration) when the
// run completes.

package cronicle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jshiv/cronicle/pkg/agent"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPNameSep is the separator between server name and tool name in the
// namespaced form the model sees (e.g. `github__create_issue`). Two
// underscores avoids confusion with tools whose own names contain a single
// underscore (most do).
const MCPNameSep = "__"

// mcpClientName/mcpClientVersion identify cronicle to the MCP server in
// the initialize handshake. The MCP standard recommends a stable client
// name and a semver-ish version.
const (
	mcpClientName    = "cronicle"
	mcpClientVersion = "0.5.0"
)

// MCPHandle is the live state of a launched MCP server: the SDK session
// (used for tool calls and shutdown), the server name (for logs), and the
// tools it advertised at startup. Held by execAgent for the duration of a
// run so it can defer Close.
type MCPHandle struct {
	Name    string
	Session *mcpsdk.ClientSession
	Tools   []*mcpsdk.Tool
}

// Close shuts down the underlying session, which closes stdin on the
// subprocess and then SIGTERMs it after the SDK's grace period.
func (h *MCPHandle) Close() error {
	if h == nil || h.Session == nil {
		return nil
	}
	return h.Session.Close()
}

// LaunchMCPServers spawns each configured MCP server, performs the
// initialize handshake, lists its tools, and returns wrapped agent.Tool
// values plus handles for shutdown. If any server fails to start or list,
// already-launched handles are closed and the error is returned — partial
// MCP state would be confusing for the agent ("which tools are real?")
// and lossy for accounting.
//
// taskEnv is the task's HCL `env` (already in KEY=VALUE form). For each
// MCP, only env vars listed in mcp.env are forwarded (looked up in the
// caller's environment). PATH is always inherited so `npx` / `python` /
// the server binary itself can be located.
func LaunchMCPServers(ctx context.Context, mcps []MCP, taskEnv []string, w io.Writer) ([]*MCPHandle, []agent.Tool, error) {
	if len(mcps) == 0 {
		return nil, nil, nil
	}
	var (
		handles []*MCPHandle
		tools   []agent.Tool
	)
	for _, m := range mcps {
		h, ts, err := launchOne(ctx, m, taskEnv, w)
		if err != nil {
			for _, prior := range handles {
				_ = prior.Close()
			}
			return nil, nil, fmt.Errorf("mcp %q: %w", m.Name, err)
		}
		handles = append(handles, h)
		tools = append(tools, ts...)
	}
	return handles, tools, nil
}

func launchOne(ctx context.Context, m MCP, taskEnv []string, w io.Writer) (*MCPHandle, []agent.Tool, error) {
	// CommandContext (vs plain Command) ensures a panic between launch
	// and the deferred Close still tears down the subprocess when ctx
	// cancels (wallclock, parent ctx, etc.). Under normal flow Close
	// handles teardown via stdin-close + SIGTERM grace period.
	cmd := exec.CommandContext(ctx, m.Command[0], m.Command[1:]...)
	cmd.Env = mcpProcessEnv(m.Env, taskEnv)
	// Server stderr routing (audit M14): in pretty mode the agent's
	// writer renders MCP output in-context with the rest of the run.
	// Without a pretty writer we previously dumped raw bytes to
	// os.Stderr, which (a) bypassed the structured log file and Loki,
	// (b) let third-party MCP servers emit arbitrary ANSI escapes
	// directly to the operator's terminal, and (c) let any secrets that
	// surfaced in MCP errors land in raw stderr instead of the bounded,
	// structured logging chain.
	//
	// Now route nil-writer stderr through mcpStderrSlogWriter which
	// emits one slog.Warn record per line, tagged with the MCP server
	// name. Operators get structured, queryable MCP errors; raw bytes
	// don't escape.
	if w != nil {
		cmd.Stderr = w
	} else {
		cmd.Stderr = newMCPStderrSlogWriter(m.Name)
	}

	transport := &mcpsdk.CommandTransport{Command: cmd}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{
		Name:    mcpClientName,
		Version: mcpClientVersion,
	}, nil)

	// Bound the handshake — a hung server should fail fast rather than
	// stall the whole run.
	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	session, err := client.Connect(connectCtx, transport, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("connect: %w", err)
	}
	listCtx, cancel2 := context.WithTimeout(ctx, 30*time.Second)
	defer cancel2()
	listRes, err := session.ListTools(listCtx, nil)
	if err != nil {
		_ = session.Close()
		return nil, nil, fmt.Errorf("list_tools: %w", err)
	}

	wrappers := make([]agent.Tool, 0, len(listRes.Tools))
	for _, t := range listRes.Tools {
		wrap, err := newMCPToolWrapper(m.Name, t, session)
		if err != nil {
			_ = session.Close()
			return nil, nil, fmt.Errorf("tool %q: %w", t.Name, err)
		}
		wrappers = append(wrappers, wrap)
	}

	return &MCPHandle{
		Name:    m.Name,
		Session: session,
		Tools:   listRes.Tools,
	}, wrappers, nil
}

// mcpProcessEnv builds the env slice for an MCP subprocess: PATH from the
// parent (so `npx`/`python`/binaries resolve), each name listed in mcp.env
// looked up from cronicle's process env, plus the task's HCL env passed
// through verbatim. We never forward the entire parent env wholesale —
// that's how secrets leak into untrusted server processes.
func mcpProcessEnv(envNames, taskEnv []string) []string {
	out := []string{"PATH=" + os.Getenv("PATH")}
	for _, name := range envNames {
		if v, ok := os.LookupEnv(name); ok {
			out = append(out, name+"="+v)
		}
	}
	out = append(out, taskEnv...)
	return out
}

// mcpToolWrapper adapts a single MCP tool to cronicle's agent.Tool
// interface. The Anthropic-facing name is namespaced (`<server>__<tool>`);
// Execute strips the prefix back off and dispatches to session.CallTool.
type mcpToolWrapper struct {
	server   string
	mcpName  string // un-namespaced, as the server knows it
	apiName  string // namespaced, as the model sees it
	desc     string
	schema   anthropic.ToolInputSchemaParam
	session  *mcpsdk.ClientSession
}

func newMCPToolWrapper(server string, t *mcpsdk.Tool, session *mcpsdk.ClientSession) (*mcpToolWrapper, error) {
	apiName := server + MCPNameSep + t.Name
	if len(apiName) > 128 {
		return nil, fmt.Errorf("namespaced name %q exceeds 128 chars", apiName)
	}
	schema, err := mcpSchemaToAnthropic(t.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("input_schema: %w", err)
	}
	desc := t.Description
	if desc == "" {
		desc = fmt.Sprintf("MCP tool %q from server %q", t.Name, server)
	}
	return &mcpToolWrapper{
		server:  server,
		mcpName: t.Name,
		apiName: apiName,
		desc:    desc,
		schema:  schema,
		session: session,
	}, nil
}

func (m *mcpToolWrapper) Name() string { return m.apiName }

func (m *mcpToolWrapper) Definition() anthropic.ToolUnionParam {
	return anthropic.ToolUnionParam{
		OfTool: &anthropic.ToolParam{
			Name:        m.apiName,
			Type:        anthropic.ToolTypeCustom,
			Description: anthropic.String(m.desc),
			InputSchema: m.schema,
		},
	}
}

func (m *mcpToolWrapper) Execute(ctx context.Context, input json.RawMessage) (string, bool) {
	var args any
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return fmt.Sprintf("Error: invalid arguments JSON: %v", err), true
		}
	}
	res, err := m.session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      m.mcpName,
		Arguments: args,
	})
	if err != nil {
		return fmt.Sprintf("Error: mcp %s.%s: %v", m.server, m.mcpName, err), true
	}
	out := mcpContentToString(res.Content)
	return out, res.IsError
}

// mcpContentToString flattens the MCP CallToolResult content list into a
// single string for the model's tool_result. Text content is concatenated
// verbatim; non-text content (images, embedded resources) is summarized
// with a placeholder so the model knows something was returned but doesn't
// need to handle the binary payload. Future work: pass through image
// content as image blocks once we have a tool_result block builder that
// supports them.
func mcpContentToString(contents []mcpsdk.Content) string {
	if len(contents) == 0 {
		return "(no content)"
	}
	var b strings.Builder
	for i, c := range contents {
		if i > 0 {
			b.WriteByte('\n')
		}
		switch v := c.(type) {
		case *mcpsdk.TextContent:
			b.WriteString(v.Text)
		default:
			fmt.Fprintf(&b, "[non-text content of type %T omitted]", c)
		}
	}
	return b.String()
}

// mcpSchemaToAnthropic converts an MCP tool's InputSchema (any value that
// JSON-marshals to a JSON Schema object) to an
// [anthropic.ToolInputSchemaParam]. Round-trips via JSON — the wire formats
// are compatible. Invalid/missing schemas degrade to an empty
// `{type: object}` schema so the tool is still callable with no args
// rather than rejected outright.
//
// `additionalProperties` and other JSON Schema keywords beyond
// properties+required are passed through ExtraFields so non-trivial
// schemas (anyOf, default, format, etc.) survive verbatim — this is the
// difference between "agent can call the tool" and "agent calls the tool
// and the server gets the input it actually expects".
func mcpSchemaToAnthropic(in any) (anthropic.ToolInputSchemaParam, error) {
	out := anthropic.ToolInputSchemaParam{}
	if in == nil {
		return out, nil
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return out, err
	}
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		return out, err
	}
	if props, ok := asMap["properties"]; ok {
		out.Properties = props
	}
	if req, ok := asMap["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				out.Required = append(out.Required, s)
			}
		}
	}
	// Forward any additional schema keywords (additionalProperties,
	// description, $defs, etc.) so the server sees its own schema.
	// `type` and `properties`/`required` are already handled above.
	for k, v := range asMap {
		switch k {
		case "type", "properties", "required":
			continue
		}
		if out.ExtraFields == nil {
			out.ExtraFields = map[string]any{}
		}
		out.ExtraFields[k] = v
	}
	return out, nil
}

// mcpStderrSlogWriter buffers MCP server stderr bytes by newline and
// emits one slog.Warn record per line, tagged with the MCP server
// name. Replaces the previous os.Stderr passthrough so:
//
//   - MCP errors land in the file log (cronicle.jsonl → Loki) and
//     LiveSink → SSE, queryable and bounded.
//   - Untrusted ANSI escapes from third-party servers can't reach the
//     operator's terminal directly.
//   - Large outputs are bounded the same way lineEmitter bounds shell
//     stdout (force-flush on cap exceeded).
type mcpStderrSlogWriter struct {
	name string
	mu   sync.Mutex
	buf  []byte
}

// mcpStderrMaxBufBytes caps the unflushed buffer. Mirrors the
// lineEmitter cap for the same reason — a no-newline stream
// (binary blob, malformed output) used to be able to grow without
// bound.
const mcpStderrMaxBufBytes = 1 << 20 // 1 MiB

func newMCPStderrSlogWriter(name string) *mcpStderrSlogWriter {
	return &mcpStderrSlogWriter{name: name}
}

func (w *mcpStderrSlogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		w.buf = w.buf[i+1:]
		if line != "" {
			slog.Warn(line,
				"entry_type", "mcp_stderr",
				"mcp_server", w.name,
			)
		}
	}
	if len(w.buf) > mcpStderrMaxBufBytes {
		slog.Warn(string(w.buf)+" [truncated: no newline]",
			"entry_type", "mcp_stderr",
			"mcp_server", w.name,
		)
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

// MCPServerNames returns the names of the launched servers, sorted for
// stable slog output.
func MCPServerNames(handles []*MCPHandle) []string {
	if len(handles) == 0 {
		return nil
	}
	out := make([]string, 0, len(handles))
	for _, h := range handles {
		out = append(out, h.Name)
	}
	sort.Strings(out)
	return out
}
