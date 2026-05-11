package cronicle_test

import (
	"path/filepath"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/hashicorp/hcl/v2/hclparse"

	"github.com/jshiv/cronicle/internal/cronicle"
)

var _ = Describe("Agent", func() {

	It("parses an agent block from HCL (legacy nested form)", func() {
		abs, _ := filepath.Abs("./test/agent.hcl")
		conf, diags := cronicle.ParseFile(abs, hclparse.NewParser())
		Expect(diags.HasErrors()).To(BeFalse())

		Expect(conf.Schedules).To(HaveLen(1))
		task := conf.Schedules[0].Tasks[0]
		Expect(task.Agent).NotTo(BeNil())
		Expect(task.Agent.Prompt).To(Equal("Summarize what changed today: ${date}"))
		Expect(task.Agent.Model).To(Equal("claude-opus-4-7"))
		Expect(task.Agent.System).To(Equal("You are a concise release-notes writer."))
		Expect(task.Agent.MaxTokens).To(Equal(2048))
		Expect(task.Agent.BudgetUSD).To(Equal(0.50))
		Expect(task.Command).To(BeNil())
	})

	// First-class form: `agent "name" {}` at schedule level instead of
	// nested inside a task. ParseFile folds these into Schedule.Tasks
	// after decode, so the resulting struct is indistinguishable from
	// the legacy form.
	It("parses a first-class agent block from HCL (sugar form)", func() {
		abs, _ := filepath.Abs("./test/agent_block.hcl")
		conf, diags := cronicle.ParseFile(abs, hclparse.NewParser())
		Expect(diags.HasErrors()).To(BeFalse())

		Expect(conf.Schedules).To(HaveLen(1))
		Expect(conf.Schedules[0].Tasks).To(HaveLen(1))
		// AgentTasks must be cleared post-fold so downstream code only
		// walks Tasks.
		Expect(conf.Schedules[0].AgentTasks).To(BeEmpty())

		task := conf.Schedules[0].Tasks[0]
		Expect(task.Name).To(Equal("summarize"))
		Expect(task.Agent).NotTo(BeNil())
		Expect(task.Agent.Prompt).To(Equal("Summarize what changed today: ${date}"))
		Expect(task.Agent.Model).To(Equal("claude-opus-4-7"))
		Expect(task.Agent.System).To(Equal("You are a concise release-notes writer."))
		Expect(task.Agent.MaxTokens).To(Equal(2048))
		Expect(task.Agent.BudgetUSD).To(Equal(0.50))
		Expect(task.Command).To(BeNil())
	})

	// Mixed config: `task "shell"` and `agent "review"` in the same
	// schedule. Both forms must coexist; the agent block normalizes into
	// the task list alongside the shell task.
	It("supports task and agent blocks side by side in one schedule", func() {
		atask := cronicle.AgentTask{
			Name:   "review",
			Prompt: "review the PR",
			Model:  "claude-opus-4-7",
		}
		task := atask.ToTask()
		Expect(task.Name).To(Equal("review"))
		Expect(task.Agent).NotTo(BeNil())
		Expect(task.Agent.Prompt).To(Equal("review the PR"))
		Expect(task.Agent.Model).To(Equal("claude-opus-4-7"))
		Expect(task.Command).To(BeNil())
		// task-shared fields untouched (defaults)
		Expect(task.Depends).To(BeNil())
		Expect(task.Repo).To(BeNil())
	})

	It("Validate rejects a task with both command and agent", func() {
		task := cronicle.Task{
			Name:    "mixed",
			Command: []string{"/bin/echo", "hi"},
			Agent:   &cronicle.Agent{Prompt: "do the thing"},
		}
		Expect(task.Validate()).To(Equal(cronicle.ErrCommandAndAgentBothGiven))
	})

	It("Validate rejects an agent with neither prompt nor skills", func() {
		task := cronicle.Task{
			Name:  "blank",
			Agent: &cronicle.Agent{},
		}
		Expect(task.Validate()).To(Equal(cronicle.ErrAgentNeedsPromptOrSkills))
	})

	It("Validate accepts an agent-only task", func() {
		task := cronicle.Task{
			Name:  "ok",
			Agent: &cronicle.Agent{Prompt: "hello"},
		}
		Expect(task.Validate()).To(BeNil())
	})

	It("Validate accepts a skills-only agent task (no prompt)", func() {
		task := cronicle.Task{
			Name:  "skill-only",
			Agent: &cronicle.Agent{Skills: []string{"skills/x/SKILL.md"}},
		}
		Expect(task.Validate()).To(BeNil())
	})

	It("Validate rejects skill paths that escape the workspace", func() {
		task := cronicle.Task{
			Name: "bad-skill",
			Agent: &cronicle.Agent{
				Prompt: "x",
				Skills: []string{"../escape/SKILL.md"},
			},
		}
		err := task.Validate()
		Expect(err).NotTo(BeNil())
		Expect(err.Error()).To(ContainSubstring("escapes task workspace"))
	})
})
