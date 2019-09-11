package run_test

import (
	"github.com/jshiv/cronicle/internal/run"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Run", func() {
	It("Should return an error if the sides don't make up a triangle", func() {

		got := run.Dummy("what")
		Expect(got).To(Equal("what"))
	})
})
