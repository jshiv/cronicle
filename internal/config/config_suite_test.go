package config_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/gernest/kemi"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"path/filepath"

	"github.com/jshiv/cronicle/internal/config"
)

func TestConfig(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Config Suite")
}

var croniclePath string
var taskPath string
var testRepoPath string

var _ = BeforeSuite(func() {
	croniclePath, _ = filepath.Abs("./testconfig/")
	taskPath, _ = filepath.Abs("./test_task/")
	config.Init(croniclePath)

	p, _ := filepath.Abs("./test_repo/")
	testRepoPath = filepath.Join(p, ".git")
	fmt.Println(testRepoPath)
	kemi.Unpack("test_repo.tar.gz", "./")

})

var _ = AfterSuite(func() {
	os.RemoveAll("./testconfig")
	os.RemoveAll("./test_repo/")
	os.RemoveAll("./test_task/")

})
