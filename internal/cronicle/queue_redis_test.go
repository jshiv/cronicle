package cronicle_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/jshiv/cronicle/internal/cronicle"
)

// In-process verification that an agent task with skills + MCP survives
// the Schedule JSON round-trip used by cronicle's distributed-mode
// transport. The actual vice→Redis send/receive path uses MakeViceTransport
// and a redis client; here we run the JSON marshal/unmarshal directly,
// which is what the worker side does after receiving bytes off the queue
// (see internal/cronicle/cron.go: ConsumeSchedule). The contract this test
// pins: every field set on the producer must be readable on the consumer.
func TestScheduleJSONRoundTripPreservesAgentFields(t *testing.T) {
	original := cronicle.Schedule{
		Name: "audit",
		Cron: "@every 30s",
		Now:  time.Now().UTC().Truncate(time.Second),
		Tasks: []cronicle.Task{
			{
				Name: "compose",
				Agent: &cronicle.Agent{
					Prompt:    "summarize today",
					Model:     "claude-haiku-4-5",
					System:    "Be concise.",
					MaxTokens: 1500,
					BudgetUSD: 0.05,
					Tools:     []string{"bash", "text_editor"},
					Skills: []string{
						"skills/morning-brief/SKILL.md",
						"skills/report-writer/SKILL.md",
					},
					MCPs: []cronicle.MCP{
						{
							Name:    "fs",
							Command: []string{"npx", "-y", "@modelcontextprotocol/server-filesystem", "/tmp"},
							Env:     []string{"PATH"},
						},
						{
							Name:    "github",
							Command: []string{"npx", "-y", "@modelcontextprotocol/server-github"},
							Env:     []string{"GITHUB_TOKEN"},
						},
					},
					MaxTurns:  12,
					Wallclock: "2m",
				},
				Env: []string{"FOO=bar"},
			},
		},
	}

	// Producer side: serialize.
	wire := original.JSON()

	// Consumer side: deserialize, exactly as ConsumeSchedule does.
	var got cronicle.Schedule
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	gotTask := got.Tasks[0]
	wantTask := original.Tasks[0]

	if gotTask.Name != wantTask.Name {
		t.Fatalf("Name: got %q, want %q", gotTask.Name, wantTask.Name)
	}
	if gotTask.Agent == nil {
		t.Fatalf("Agent block lost in transit")
	}
	if gotTask.Agent.Prompt != wantTask.Agent.Prompt ||
		gotTask.Agent.Model != wantTask.Agent.Model ||
		gotTask.Agent.System != wantTask.Agent.System ||
		gotTask.Agent.MaxTokens != wantTask.Agent.MaxTokens ||
		gotTask.Agent.BudgetUSD != wantTask.Agent.BudgetUSD ||
		gotTask.Agent.MaxTurns != wantTask.Agent.MaxTurns ||
		gotTask.Agent.Wallclock != wantTask.Agent.Wallclock {
		t.Fatalf("agent scalar fields diverged: got %+v", gotTask.Agent)
	}
	if len(gotTask.Agent.Tools) != 2 || gotTask.Agent.Tools[0] != "bash" {
		t.Fatalf("Tools: got %v", gotTask.Agent.Tools)
	}
	if len(gotTask.Agent.Skills) != 2 ||
		gotTask.Agent.Skills[0] != "skills/morning-brief/SKILL.md" {
		t.Fatalf("Skills: got %v", gotTask.Agent.Skills)
	}
	if len(gotTask.Agent.MCPs) != 2 {
		t.Fatalf("MCPs len: got %d, want 2", len(gotTask.Agent.MCPs))
	}
	mcp := gotTask.Agent.MCPs[0]
	if mcp.Name != "fs" || len(mcp.Command) != 4 || mcp.Command[0] != "npx" {
		t.Fatalf("MCP[0]: got %+v", mcp)
	}
	if len(gotTask.Env) != 1 || gotTask.Env[0] != "FOO=bar" {
		t.Fatalf("Env: got %v", gotTask.Env)
	}
}

// End-to-end sanity over an in-process Redis (miniredis): cronicle's vice
// transport pushes JSON to Redis; a separate consumer goroutine pulls it
// off, unmarshals, and we assert the agent block survived. This exercises
// the actual transport (vice + go-redis) — not just JSON — so a regression
// in the queue plumbing would surface here.
func TestRedisQueueRoundTripWithMiniredis(t *testing.T) {
	mr := miniredis.RunT(t) // auto-cleanup
	defer mr.Close()

	transport := cronicle.MakeViceTransport("redis", mr.Addr())
	defer transport.Stop()

	const queueName = "cronicle-test"
	send := transport.Send(queueName)
	recv := transport.Receive(queueName)

	original := cronicle.Schedule{
		Name: "wire-check",
		Tasks: []cronicle.Task{
			{
				Name: "agent",
				Agent: &cronicle.Agent{
					Prompt: "hi",
					Skills: []string{"a.md", "b.md"},
					MCPs:   []cronicle.MCP{{Name: "fs", Command: []string{"npx", "-y", "x"}}},
				},
			},
		},
	}

	send <- original.JSON()

	select {
	case msg := <-recv:
		var got cronicle.Schedule
		if err := json.Unmarshal(msg, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Name != "wire-check" {
			t.Fatalf("Name lost: %q", got.Name)
		}
		if got.Tasks[0].Agent == nil {
			t.Fatalf("Agent lost")
		}
		if len(got.Tasks[0].Agent.Skills) != 2 {
			t.Fatalf("Skills lost: %v", got.Tasks[0].Agent.Skills)
		}
		if len(got.Tasks[0].Agent.MCPs) != 1 || got.Tasks[0].Agent.MCPs[0].Name != "fs" {
			t.Fatalf("MCPs lost: %+v", got.Tasks[0].Agent.MCPs)
		}
	case err := <-transport.ErrChan():
		t.Fatalf("transport error: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for queued schedule")
	}
}
