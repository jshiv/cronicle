package cronicle

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// remoteMCPServer serves an MCP server with tools echo and wipe over
// Streamable HTTP and the older SSE transport, behind a bearer-token check.
func remoteMCPServer(t *testing.T, token string) (streamableURL, sseURL string) {
	t.Helper()
	newServer := func(*http.Request) *mcpsdk.Server {
		s := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test", Version: "1"}, nil)
		type in struct {
			Text string `json:"text"`
		}
		mcpsdk.AddTool(s, &mcpsdk.Tool{Name: "echo", Description: "echo text back"},
			func(_ context.Context, _ *mcpsdk.CallToolRequest, a in) (*mcpsdk.CallToolResult, any, error) {
				return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "echo: " + a.Text}}}, nil, nil
			})
		mcpsdk.AddTool(s, &mcpsdk.Tool{Name: "wipe", Description: "delete everything"},
			func(_ context.Context, _ *mcpsdk.CallToolRequest, _ in) (*mcpsdk.CallToolResult, any, error) {
				t.Error("wipe must not be callable when filtered out")
				return &mcpsdk.CallToolResult{}, nil, nil
			})
		return s
	}
	auth := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", auth(mcpsdk.NewStreamableHTTPHandler(newServer, nil)))
	mux.Handle("/sse", auth(mcpsdk.NewSSEHandler(newServer, nil)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/mcp", srv.URL + "/sse"
}

func TestRemoteMCP_StreamableAndSSE(t *testing.T) {
	streamable, sse := remoteMCPServer(t, "t0k")
	for _, m := range []MCP{
		{Name: "svc", URL: streamable},
		{Name: "svc", URL: sse, Transport: "sse"},
	} {
		m.Headers = map[string]string{"Authorization": "Bearer t0k"}
		m.Tools = []string{"echo"}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		handles, tools, err := LaunchMCPServers(ctx, []MCP{m}, nil, nil)
		if err != nil {
			cancel()
			t.Fatalf("%s: launch: %v", m.URL, err)
		}
		if len(tools) != 1 || tools[0].Name() != "svc__echo" {
			t.Errorf("%s: tools filter should leave only svc__echo, got %d", m.URL, len(tools))
		}
		out, isErr := tools[0].Execute(ctx, json.RawMessage(`{"text":"hi"}`))
		if isErr || !strings.Contains(out, "echo: hi") {
			t.Errorf("%s: call: %q %v", m.URL, out, isErr)
		}
		for _, h := range handles {
			_ = h.Close()
		}
		cancel()
	}
}

func TestRemoteMCP_Failures(t *testing.T) {
	streamable, _ := remoteMCPServer(t, "t0k")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, _, err := LaunchMCPServers(ctx, []MCP{{Name: "svc", URL: streamable,
		Headers: map[string]string{"Authorization": "Bearer wrong"}}}, nil, nil); err == nil {
		t.Error("a rejected token must fail the launch")
	}
	// A listed tool the server doesn't offer (renamed, misspelled) is
	// skipped with a warning naming what it does offer; the run goes on.
	var block strings.Builder
	handles, tools, err := LaunchMCPServers(ctx, []MCP{{Name: "svc", URL: streamable,
		Headers: map[string]string{"Authorization": "Bearer t0k"}, Tools: []string{"echoo", "echo"}}}, nil, &block)
	if err != nil {
		t.Fatalf("a missing tool must not fail the launch: %v", err)
	}
	for _, h := range handles {
		_ = h.Close()
	}
	if len(tools) != 1 || tools[0].Name() != "svc__echo" {
		t.Errorf("the tools that do exist should still be kept, got %d", len(tools))
	}
	if !strings.Contains(block.String(), "skipping echoo, not offered by the server (it offers: echo, wipe)") {
		t.Errorf("the block should say which tool was skipped and what's offered: %q", block.String())
	}

	// None of the listed tools exist: the block is out of date, and the
	// agent would get nothing from this server. That fails.
	_, _, err = LaunchMCPServers(ctx, []MCP{{Name: "svc", URL: streamable,
		Headers: map[string]string{"Authorization": "Bearer t0k"}, Tools: []string{"echoo", "wiipe"}}}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "none of the tools listed (echoo, wiipe) are offered by the server (it offers: echo, wipe)") {
		t.Errorf("no matching tools should fail, naming what's offered: %v", err)
	}
}

func TestMCP_HeaderSecrets(t *testing.T) {
	cleanup := setupSecretStore(t, map[string]string{"SVC_TOKEN": "t0k"})
	defer cleanup()
	task := Task{Name: "a", Agent: &Agent{MCPs: []MCP{{Name: "svc", URL: "https://x/${path}",
		Headers: map[string]string{"Authorization": "Bearer $secret.SVC_TOKEN"}}}}, Path: "p"}
	mcps, err := task.resolveMCPs(strings.NewReplacer("${path}", "mcp"))
	if err != nil || mcps[0].Headers["Authorization"] != "Bearer t0k" || mcps[0].URL != "https://x/mcp" {
		t.Errorf("resolved: %+v %v", mcps, err)
	}
	if task.Agent.MCPs[0].Headers["Authorization"] != "Bearer $secret.SVC_TOKEN" {
		t.Error("resolving must not write the secret back into the config")
	}
	task.Agent.MCPs[0].Headers["Authorization"] = "Bearer $secret.MISSING"
	if _, err := task.resolveMCPs(strings.NewReplacer()); err == nil || !strings.Contains(err.Error(), "MISSING") {
		t.Errorf("a missing header secret must fail, by name: %v", err)
	}
}

func TestMCP_SharedDefinitionsAndValidation(t *testing.T) {
	conf, err := parseConfig(t, `
mcp "github" {
  url     = "https://api.example.com/mcp/"
  headers = { Authorization = "Bearer $secret.GITHUB_TOKEN" }
  tools   = ["get_issue"]
}
schedule "s" {
  mcp "github" {
    url = "https://schedule.example.com/mcp/"
  }
  mcp "fs" {
    command = ["npx", "-y", "@modelcontextprotocol/server-filesystem", "${path}"]
  }
  agent "a" {
    prompt      = "hi"
    mcp_servers = ["github", "fs"]
    mcp "local" {
      command = ["./server"]
    }
  }
}`)
	if err != nil {
		t.Fatal(err)
	}
	got := conf.Schedules[0].TaskMap()["a"].Agent
	var names []string
	for _, m := range got.MCPs {
		names = append(names, m.Name+"="+m.URL+strings.Join(m.Command, " "))
	}
	want := "local=./server github=https://schedule.example.com/mcp/ fs=npx -y @modelcontextprotocol/server-filesystem ${path}"
	if strings.Join(names, " ") != want {
		t.Errorf("resolved servers:\n got %s\nwant %s (a schedule's definition wins over the top level)", strings.Join(names, " "), want)
	}

	for body, want := range map[string]string{
		`agent "a" {
		  prompt = "hi"
		  mcp_servers = ["nope"]
		}`: `mcp_servers names nope, but no mcp block`,
		`mcp "x" {
		  command = ["s"]
		}
		agent "a" {
		  prompt = "hi"
		  mcp_servers = ["x"]
		  mcp "x" {
		    command = ["t"]
		  }
		}`: `defined twice`,
		`mcp "x" {
		  command = ["s"]
		  url = "https://h/"
		}`: `command or url, not both`,
		`mcp "x" {
		}`: `set command`,
		`mcp "x" {
		  url = "https://h/"
		  env = ["A"]
		}`: `takes headers instead`,
		`mcp "x" {
		  url = "https://h/"
		  transport = "ws"
		}`: `transport must be`,
		`mcp "x" {
		  command = ["s"]
		  headers = { A = "b" }
		}`: `apply to a url server`,
		`mcp "x" {
		  url = "ftp://h/"
		}`: `url must start with`,
	} {
		if _, err := parseConfig(t, "schedule \"s\" {\n"+body+"\n}"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want error %q, got %v\n%s", want, err, body)
		}
	}
}

// TestMCP_ItemTextNeverExpandsSecrets: a for_each item's value goes into
// a header after secrets resolve, so an item containing "$secret.NAME"
// reaches the server as that literal text, never as the secret.
func TestMCP_ItemTextNeverExpandsSecrets(t *testing.T) {
	cleanup := setupSecretStore(t, map[string]string{"SVC_TOKEN": "t0k"})
	defer cleanup()
	tmpl := Task{Name: "a", ForEach: &ForEach{}, Agent: &Agent{MCPs: []MCP{{Name: "svc",
		URL: "https://${each.value.host}/mcp",
		Headers: map[string]string{
			"Authorization": "Bearer $secret.SVC_TOKEN",
			"X-Title":       "${each.value.title}",
		}}}}}
	inst, err := tmpl.instance(ForEachItem{Key: "1",
		Value: json.RawMessage(`{"host":"evil.example","title":"$secret.SVC_TOKEN"}`)})
	if err != nil {
		t.Fatal(err)
	}
	mcps, err := inst.resolveMCPs(strings.NewReplacer())
	if err != nil {
		t.Fatal(err)
	}
	h := mcps[0].Headers
	if h["X-Title"] != "$secret.SVC_TOKEN" {
		t.Errorf("item text was expanded into a secret: %q", h["X-Title"])
	}
	if h["Authorization"] != "Bearer t0k" || mcps[0].URL != "https://evil.example/mcp" {
		t.Errorf("the config's own secret and the item's url should still resolve: %+v", mcps[0])
	}
}
