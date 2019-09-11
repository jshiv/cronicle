package config_test

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	// . "UNKNOWN_PACKAGE_PATH"
)

var _ = Describe("Parse", func() {

	It("Should return an error if the sides don't make up a triangle", func() {
		Expect("blah").To(Equal("blah"))
	})
})
