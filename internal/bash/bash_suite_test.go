package bash_test

import (
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestBash(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Bash Suite")
}
