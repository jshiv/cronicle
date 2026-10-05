package cronicle

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/jshiv/cronicle/pkg/agent"
)

func TestOpenAIConfigForms(t *testing.T) {
	for _, block := range []string{
		`agent "check" {
 provider = "openai"
   prompt = "hi"
  }`,
		`task "check" {
 agent {
 provider = "openai"
   prompt = "hi"
  }
 }`,
	} {
		p := filepath.Join(t.TempDir(), "cronicle.hcl")
		os.WriteFile(p, []byte("schedule \"test\" {\ncron = \"0 2 * * *\"\n"+block+"\n}"), 0600)
		conf, diags := ParseFile(p, hclparse.NewParser())
		if diags.HasErrors() {
			t.Fatal(diags.Error())
		}
		task := conf.Schedules[0].Tasks[0]
		b, _ := json.Marshal(task)
		var queued Task
		json.Unmarshal(b, &queued)
		if queued.Agent.Provider != "openai" {
			t.Fatalf("provider lost: %s", b)
		}
		if err := queued.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}
func TestAgentProviderValidation(t *testing.T) {
	for _, a := range []Agent{{Provider: "typo", Prompt: "hi"}, {Provider: "openai", Prompt: "hi", Tools: []string{"web_search"}}, {Provider: "openai", Prompt: "hi", Tools: []string{"web_fetch"}}} {
		task := Task{Name: "test", Agent: &a}
		if task.Validate() == nil {
			t.Errorf("accepted unsupported config: %+v", a)
		}
	}
}
func TestProviderAPIKeyIsolation(t *testing.T) {
	for _, provider := range []string{"", "anthropic", "openai"} {
		env := []string{"OPENAI_API_KEY=openai-secret", "ANTHROPIC_API_KEY=anthropic-secret", "OPENAI_API_KEY=duplicate", "KEEP=value"}
		key, rest := splitAPIKey(env, provider)
		want := "anthropic-secret"
		if provider == "openai" {
			want = "openai-secret"
		}
		if key != want || !reflect.DeepEqual(rest, []string{"KEEP=value"}) {
			t.Fatalf("provider=%q key=%q rest=%v", provider, key, rest)
		}
	}
}

// Opt-in integration test: exercises HCL dispatch, the real SDK, tool execution,
// key isolation, and a second model turn. Never runs in the ordinary test suite.
func TestOpenAILive(t *testing.T) {
	if os.Getenv("CRONICLE_OPENAI_LIVE_TEST") != "1" {
		t.Skip("set CRONICLE_OPENAI_LIVE_TEST=1 to make a small paid API call")
	}
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Fatal("OPENAI_API_KEY is required")
	}
	workspace := t.TempDir()
	task := Task{Name: "smoke", ScheduleName: "openai-smoke", Path: workspace,
		Env: []string{"OPENAI_API_KEY=" + key},
		Agent: &Agent{Provider: "openai", Model: "gpt-4.1-mini", Tools: []string{"bash"}, MaxTokens: 128, MaxTurns: 3, Wallclock: "45s", BudgetUSD: 0.01,
			Prompt: `Use bash exactly once to execute: test -z "$OPENAI_API_KEY" && test -z "$ANTHROPIC_API_KEY" && printf 'OPENAI_TOOL_OK' > result.txt. Then reply with only DONE.`},
	}
	result, err := task.Execute(time.Now())
	if err != nil || result.Error != nil || result.ExitStatus != 0 {
		t.Fatalf("agent failed: %v %v %s", err, result.Error, result.Stderr)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "result.txt"))
	if err != nil || string(data) != "OPENAI_TOOL_OK" {
		t.Fatalf("tool did not write expected result: %q %v", data, err)
	}
	t.Logf("OpenAI tool cycle succeeded: %s", result.Stdout)
}

func TestOpenAIDefaultToolsAndSkill(t *testing.T) {
	workspace := t.TempDir()
	skill, err := NewSkillTool([]*Skill{{Name: "example", Description: "Example skill", Body: "skill instructions"}})
	if err != nil {
		t.Fatal(err)
	}
	tools := append(buildAgentTools(nil, workspace, nil, nil, "", nil), skill)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		var output []any
		if requests == 1 {
			defs := map[string]map[string]any{}
			for _, raw := range body["tools"].([]any) {
				def := raw.(map[string]any)
				defs[def["name"].(string)] = def
			}
			for _, name := range []string{"bash", "str_replace_based_edit_tool", "git", "load_skill"} {
				def := defs[name]
				if def == nil || def["type"] != "function" {
					t.Errorf("missing function %s", name)
					continue
				}
				schema := def["parameters"].(map[string]any)
				if schema["type"] != "object" || schema["properties"] == nil {
					t.Errorf("bad schema for %s: %v", name, schema)
				}
			}
			output = []any{
				map[string]any{"type": "function_call", "id": "fc_editor", "call_id": "editor", "name": "str_replace_based_edit_tool", "arguments": `{"command":"create","path":"result.txt","file_text":"created"}`},
				map[string]any{"type": "function_call", "id": "fc_skill", "call_id": "skill", "name": "load_skill", "arguments": `{"name":"example"}`},
			}
		} else {
			encoded, _ := json.Marshal(body["input"])
			if !strings.Contains(string(encoded), "skill instructions") {
				t.Errorf("skill was not returned: %s", encoded)
			}
			output = []any{}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		event := map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_test", "status": "completed", "output": output}}
		data, _ := json.Marshal(event)
		fmt.Fprintf(w, "data: %s\n\n", data)
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL+"/")
	t.Setenv("OPENAI_API_KEY", "test-key")
	_, err = agent.Run(context.Background(), agent.Config{Provider: "openai", Prompt: "hi", Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "result.txt"))
	if err != nil || string(data) != "created" {
		t.Fatalf("editor failed: %q %v", data, err)
	}
	if !reflect.DeepEqual(skill.Loaded(), []string{"example"}) {
		t.Fatalf("skill not loaded: %v", skill.Loaded())
	}
}
