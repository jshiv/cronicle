package cronicle_test

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/hashicorp/hcl/v2/hclsimple"

	"github.com/jshiv/cronicle/internal/cronicle"
)

var _ = Describe("Agent", func() {

	It("parses an agent block from HCL", func() {
		var conf cronicle.Config
		err := hclsimple.DecodeFile("./test/agent.hcl", &cronicle.CommandEvalContext, &conf)
		Expect(err).To(BeNil())

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
