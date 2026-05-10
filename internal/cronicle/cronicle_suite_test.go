package cronicle_test

import (
	"os"
	"testing"

	"github.com/gernest/kemi"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"path/filepath"

	"github.com/jshiv/cronicle/internal/cronicle"
)

func TestConfig(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Config Suite")
}

var croniclePath string
var taskPath string
var testRepoPath string
var testSSHAddr string
var testSSHStop func()

var _ = BeforeSuite(func() {
	croniclePath, _ = filepath.Abs("./testconfig/")
	taskPath, _ = filepath.Abs("./test_task/")
	cronicle.Init(croniclePath, "", "", cronicle.Default())

	p, _ := filepath.Abs("./test_repo/")
	testRepoPath = filepath.Join(p, ".git")
	kemi.Unpack("test_repo.tar.gz", "./")

	serveRoot, _ := filepath.Abs("./")
	keyPath, _ := filepath.Abs("./test/test_deploy_key.pub")
	addr, stop, err := startTestSSHServer(serveRoot, keyPath)
	Expect(err).To(BeNil())
	testSSHAddr = addr
	testSSHStop = stop
})

var _ = AfterSuite(func() {
	if testSSHStop != nil {
		testSSHStop()
	}
	os.RemoveAll("./testconfig")
	os.RemoveAll("./test_repo/")
	os.RemoveAll("./test_task/")

})
