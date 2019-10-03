package cron_test

import (
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"path/filepath"

	"github.com/jshiv/cronicle/internal/config"
)

func TestCron(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Cron Suite")
}

var croniclePath string

var _ = BeforeSuite(func() {
	croniclePath, _ = filepath.Abs("./testconfig/")
	config.Init(croniclePath)
})

// var _ = AfterSuite(func() {
// 	os.RemoveAll("./testconfig")
// })
