package create

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/jshiv/cronicle/internal/config"
	"github.com/jshiv/cronicle/internal/git"

	gogit "gopkg.in/src-d/go-git.v4"
)

//Init initializes a default croniclePath with a .git repository,
//Basic schedule as code in a Cronicle.hcl file and a repos folder.
func Init(croniclePath string) {
	abs, err := filepath.Abs(croniclePath)
	if err != nil {
		panic(err)
	}
	fmt.Println(abs)
	os.MkdirAll(path.Join(abs, "repos"), 0777)
	_, err = gogit.PlainInit(abs, false)
	if err != nil {
		fmt.Printf("\x1b[31;1m%s\x1b[0m\n", fmt.Sprintf("git: %s", err))
	}
	cronicleFile := path.Join(abs, "Cronicle.hcl")
	var conf *config.Config
	// Does the Cronicle.hcl file exist?
	if _, err := os.Stat(cronicleFile); err == nil {
		var parseErr error
		conf, parseErr = config.ParseFile(cronicleFile)
		if err != nil {
			panic(parseErr)
		}
	} else if os.IsNotExist(err) {
		// path/to/whatever does *not* exist
		config.MarshallHcl(config.Default(), cronicleFile)
		git.Commit(abs, "Cronicle Initial Commit")

	} else {
		panic(errors.New("cronicle.hcl does not exist and was not created"))
	}
	// config.
	fmt.Println(conf)

	// bash.LogStdout(bashReturn)
	// bashReturn = bash.Bash([]string{"ls -la /tmp/testrepo/"})
	// bash.LogStdout(bashReturn)
	// cfg, _ := config.ParseFile("./Cronicle.hcl")
	// fmt.Println(cfg)
}
