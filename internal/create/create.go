package create

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/jshiv/cronicle/internal/config"
	"github.com/jshiv/cronicle/internal/git"

	gogit "gopkg.in/src-d/go-git.v4"
)

//Init initializes a default croniclePath with a .git repository,
//Basic schedule as code in a Cronicle.hcl file and a repos folder.
func Init(croniclePath string) {
	absCroniclePath, err := filepath.Abs(croniclePath)
	if err != nil {
		panic(err)
	}
	fmt.Println(absCroniclePath)
	os.MkdirAll(path.Join(absCroniclePath, "repos"), 0777)
	_, err = gogit.PlainInit(absCroniclePath, false)
	if err != nil {
		fmt.Printf("\x1b[31;1m%s\x1b[0m\n", fmt.Sprintf("git: %s", err))
	}
	cronicleFile := path.Join(absCroniclePath, "Cronicle.hcl")
	var conf *config.Config
	// Does the Cronicle.hcl file exist?
	if _, err := os.Stat(cronicleFile); err == nil {
		var parseErr error
		conf, parseErr = config.ParseFile(cronicleFile)
		if parseErr != nil {
			panic(parseErr)
		}
		CloneRepos(absCroniclePath, conf)
		// If not, create it from config.Default() and commit
	} else if os.IsNotExist(err) {
		// path/to/whatever does *not* exist
		config.MarshallHcl(config.Default(), cronicleFile)
		git.Commit(absCroniclePath, "Cronicle Initial Commit")

	} else {
		panic(errors.New("cronicle.hcl does not exist and was not created"))
	}
	// config.
	fmt.Println(conf)

}

//CloneRepos clones all repositories configured in Cronicle.hcl
func CloneRepos(croniclePath string, conf *config.Config) {
	repos := map[string]bool{}
	for _, sched := range conf.Schedules {
		schedRepo := sched.Repo
		if schedRepo != "" {
			repos[schedRepo] = true
		}
		for _, task := range sched.Tasks {
			taskRepo := task.Repo
			if taskRepo != "" {
				repos[taskRepo] = true
			}
		}
	}
	for repo := range repos {
		fullRepoDir , _ := LocalRepoDir(croniclePath, repo)
		git.Clone(repo, fullRepoDir)
	}
}

//LocalRepoDir takes a Cronicle.hcl path and a github repo URL and converts
//it to the local clone of that repo
func LocalRepoDir(croniclePath string, repo string) (string, error) {
	reposDir := path.Join(croniclePath, "repos")
	repoClean := strings.Replace(strings.Replace(repo, "github.com/", "", 1), "https:", "", 1)
	localRepoDir := path.Join(reposDir, repoClean)
	return localRepoDir, nil
}
