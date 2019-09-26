package config_test

import (
	"os"
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"path/filepath"

	"github.com/jshiv/cronicle/internal/config"
)

var croniclePath string

func TestConfig(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Config Suite")
}

var _ = BeforeSuite(func() {
	croniclePath, _ = filepath.Abs("./testconfig/")
	config.Init(croniclePath)
})

var _ = AfterSuite(func() {
	os.RemoveAll("./testconfig")
})
