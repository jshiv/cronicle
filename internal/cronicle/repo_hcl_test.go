package cronicle_test

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/hashicorp/hcl/v2/hclsimple"

	"github.com/jshiv/cronicle/internal/cronicle"
)

// Locks in the fix for the Repo HCL tag swap that crossed Branch/Commit.
// Before the fix, `branch = "main"` populated Repo.Commit and `commit = "abc"`
// populated Repo.Branch. Validate also called Checkout(Branch, Commit) so
// the swap was double-wrong but mostly invisible because git can resolve
// "main" as either a branch or a commit-ish.
var _ = Describe("Repo HCL tags", func() {
	It("`branch` populates Repo.Branch", func() {
		var conf cronicle.Config
		err := hclsimple.DecodeFile("./test/repo.hcl", &cronicle.CommandEvalContext, &conf)
		Expect(err).To(BeNil())

		var withBranch *cronicle.Schedule
		for i := range conf.Schedules {
			if conf.Schedules[i].Name == "with_branch" {
				withBranch = &conf.Schedules[i]
			}
		}
		Expect(withBranch).NotTo(BeNil())
		Expect(withBranch.Tasks[0].Repo).NotTo(BeNil())
		Expect(withBranch.Tasks[0].Repo.Branch).To(Equal("main"))
		Expect(withBranch.Tasks[0].Repo.Commit).To(Equal(""))
	})

	It("`commit` populates Repo.Commit", func() {
		var conf cronicle.Config
		err := hclsimple.DecodeFile("./test/repo.hcl", &cronicle.CommandEvalContext, &conf)
		Expect(err).To(BeNil())

		var withCommit *cronicle.Schedule
		for i := range conf.Schedules {
			if conf.Schedules[i].Name == "with_commit" {
				withCommit = &conf.Schedules[i]
			}
		}
		Expect(withCommit).NotTo(BeNil())
		Expect(withCommit.Tasks[0].Repo).NotTo(BeNil())
		Expect(withCommit.Tasks[0].Repo.Commit).To(Equal("abc1234"))
		Expect(withCommit.Tasks[0].Repo.Branch).To(Equal(""))
	})
})
