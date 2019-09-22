package config

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/fatih/color"

	gogit "gopkg.in/src-d/go-git.v4"
)

//Init initializes a default croniclePath with a .git repository,
//Basic schedule as code in a Cronicle.hcl file and a repos folder.
func Init(croniclePath string) {
	absCroniclePath, err := filepath.Abs(croniclePath)
	if err != nil {
		panic(err)
	}
	slantyedCyan := color.New(color.FgCyan, color.Italic).SprintFunc()
	// errors.New("could not extract repos from " + slantedRed("Config"))
	fmt.Println("Init Cronicle: " + slantyedCyan(absCroniclePath))
	os.MkdirAll(path.Join(absCroniclePath, "repos"), 0777)
	_, err = gogit.PlainInit(absCroniclePath, false)
	if err != nil {
		fmt.Printf("\x1b[31;1m%s\x1b[0m\n", fmt.Sprintf("git: %s", err))
	}
	cronicleFile := path.Join(absCroniclePath, "Cronicle.hcl")
	if fileExists(cronicleFile) {
		conf, _ := GetConfig(cronicleFile)
		hcl := GetHcl(*conf)
		fmt.Printf("%s", slantyedCyan(string(hcl.Bytes())))
		CloneRepos(absCroniclePath, conf)
	} else {
		MarshallHcl(Default(), cronicleFile)
		Commit(absCroniclePath, "Cronicle Initial Commit")
	}

}

// GetRepos collects the set of repos associated to a given config
func GetRepos(conf *Config) map[string]bool {
	repos := map[string]bool{}
	for _, repo := range conf.Repos {
		fmt.Println(repo)
		repos[repo] = true
	}
	for _, sched := range conf.Schedules {
		if sched.Repo != "" {
			repos[sched.Repo] = true
		}
		for _, task := range sched.Tasks {
			if task.Repo != "" {
				repos[task.Repo] = true
			}
		}
	}

	return repos
}

//CloneRepos clones all repositories configured in Cronicle.hcl
// TODO: append {schedulename}.{taskname} to each directory in repos to avoid concurancy issues
func CloneRepos(croniclePath string, conf *Config) {
	repos := GetRepos(conf)
	for repo := range repos {
		fullRepoDir, _ := LocalRepoDir(croniclePath, repo)
		Clone(repo, fullRepoDir)
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

// GetConfig returns the Config specified by the given Cronicle.hcl file
// Including any Cronicle files specified by in the repos directory.
func GetConfig(cronicleFile string) (*Config, error) {
	cronicleFileAbs, err := filepath.Abs(cronicleFile)
	if err != nil {
		fmt.Println(err)
	}
	croniclePath := filepath.Dir(cronicleFileAbs)

	conf, err := ParseFile(cronicleFileAbs)
	if err != nil {
		fmt.Println(err)
	}

	// Assign the path for each task or schedule repo
	for sdx, schedule := range conf.Schedules {
		for tdx, task := range schedule.Tasks {
			if task.Repo != "" {
				p, _ := LocalRepoDir(croniclePath, task.Repo)
				conf.Schedules[sdx].Tasks[tdx].Path = p
				conf.Schedules[sdx].Tasks[tdx].Git = GetGit(p)
			} else if schedule.Repo != "" {
				p, _ := LocalRepoDir(croniclePath, schedule.Repo)
				conf.Schedules[sdx].Tasks[tdx].Path = p
				conf.Schedules[sdx].Tasks[tdx].Git = GetGit(p)
			} else {
				conf.Schedules[sdx].Tasks[tdx].Path = croniclePath
				conf.Schedules[sdx].Tasks[tdx].Path = croniclePath
				conf.Schedules[sdx].Tasks[tdx].Git = GetGit(croniclePath)
			}
		}
	}

	// Collect any sub level Cronicle files if they exist
	// then append all schedules to conf.Schedules
	// Explicitly ignore any information that is not in a schedule.
	repos := GetRepos(conf)
	for repo := range repos {
		repoPath, _ := LocalRepoDir(croniclePath, repo)
		fmt.Println("sub repo path:  " + repoPath)
		repoCronicleFile := filepath.Join(repoPath, "Cronicle.hcl")
		fmt.Println("sub repo file:  " + repoCronicleFile)
		fmt.Println(fileExists(repoCronicleFile))
		if fileExists(repoCronicleFile) {
			repoConf, _ := GetConfig(repoCronicleFile)
			for _, repoSched := range repoConf.Schedules {
				conf.Schedules = append(conf.Schedules, repoSched)
			}
		}

	}

	return conf, nil
}

// fileExists checks if a file exists and is not a directory before we
// try using it to prevent further errors.
func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}
