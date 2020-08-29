package config

import (
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/fatih/color"

	"net/url"

	"github.com/hashicorp/hcl/v2/hclsimple"
	"gopkg.in/src-d/go-git.v4"
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
	_, err = git.PlainInit(absCroniclePath, false)
	if err != nil {
		fmt.Printf("\x1b[31;1m%s\x1b[0m\n", fmt.Sprintf("git: %s", err))
	}
	cronicleFile := path.Join(absCroniclePath, "Cronicle.hcl")
	if fileExists(cronicleFile) {
		conf, _ := GetConfig(cronicleFile)
		hcl := conf.Hcl()
		fmt.Printf("%s", slantyedCyan(string(hcl.Bytes)))
		// CloneRepos(absCroniclePath, conf)
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

//LocalRepoDir takes a Cronicle.hcl path and a github repo URL and converts
//it to the local clone of that repo
func LocalRepoDir(croniclePath string, repo string) (string, error) {
	reposDir := path.Join(croniclePath, "repos")
	u, err := url.Parse(repo)
	if err != nil {
		return "", err
	}
	localRepoDir := path.Join(reposDir, u.Path)

	return localRepoDir, nil
}

//SetGit sets assigns the
func (task *Task) SetGit() {
	if !DirExists(filepath.Join(task.Path, ".git")) {

		_, err := git.PlainClone(task.Path, false, &git.CloneOptions{URL: task.Repo})
		if err != nil {
			fmt.Println(err)
		}
	}
	task.Git = GetGit(task.Path)
}

//CleanGit nulls non-serlizable properties of a task
//task.Git = Git{}
func (task Task) CleanGit() Task {
	t := task
	t.Git = Git{}
	return t
}

//CleanGit nulls non-serlizable properties of a schedule
//task.Git = Git{}
func (schedule Schedule) CleanGit() Schedule {
	sch := schedule
	for i, task := range schedule.Tasks {
		sch.Tasks[i] = task.CleanGit()
	}
	return sch
}

//SetConfig populates task repo path, runs git clone for any sub repos,
//and assigns Git meta data to the task
func SetConfig(conf *Config, croniclePath string) error {
	// Assign the path for each task or schedule repo
	conf.Validate()
	for sdx, schedule := range conf.Schedules {
		for tdx, task := range schedule.Tasks {
			err := task.Validate()
			if err != nil {
				return err
			}
			var path string
			var taskPath string
			var repo string

			// If the task is associated to a repo
			if task.Repo != "" {
				repo = task.Repo
				// If a Schedule is associated to a repo, all sub tasks are by default associated
			} else if schedule.Repo != "" {
				repo = schedule.Repo
				// Else the repo is the cronicle repo
			} else {
				//TODO: make remote cronicle repo rathar than ""
				repo = ""
			}
			// If the task is associated to a repo, put it in the repos directory
			if task.Repo != "" {
				path, _ = LocalRepoDir(croniclePath, task.Repo)
				// If a Schedule is associated to a repo, all sub tasks are by default associated
			} else if schedule.Repo != "" {
				path, _ = LocalRepoDir(croniclePath, schedule.Repo)
				// Else the path is the root croniclePath
			} else {
				path = croniclePath
			}

			// If the given task is associatated to a repo, clone the task to an independent path
			if repo != "" {
				taskPath = filepath.Join(path, schedule.Name, task.Name)
				// Else the task is associated to the root croniclePath
			} else {
				taskPath = croniclePath
			}
			conf.Schedules[sdx].Tasks[tdx].Path = taskPath
			conf.Schedules[sdx].Tasks[tdx].Repo = repo

			// // Clone the repo if there is no .git directory in taskPath
			// if !DirExists(filepath.Join(taskPath, ".git")) {

			// 	_, err := git.PlainClone(taskPath, false, &git.CloneOptions{URL: repo})
			// 	if err != nil {
			// 		return err
			// 	}
			// }
			// conf.Schedules[sdx].Tasks[tdx].Git = GetGit(taskPath)
			conf.Schedules[sdx].Tasks[tdx].SetGit()
			conf.Schedules[sdx].Tasks[tdx].CleanGit()
			conf.Schedules[sdx].Tasks[tdx].ScheduleName = schedule.Name
		}
	}
	return nil
}

// GetConfig returns the Config specified by the given Cronicle.hcl file
// Including any Cronicle files specified by in the repos directory.
func GetConfig(cronicleFile string) (*Config, error) {
	cronicleFileAbs, err := filepath.Abs(cronicleFile)
	if err != nil {
		return nil, err
	}
	croniclePath := filepath.Dir(cronicleFileAbs)

	var conf Config
	err = hclsimple.DecodeFile(cronicleFileAbs, &CommandEvalContext, &conf)
	// conf, err := ParseFile(cronicleFileAbs)
	if err != nil {
		return nil, err
	}

	if err := SetConfig(&conf, croniclePath); err != nil {
		return &conf, err
	}

	// Collect any sub level Cronicle files if they exist
	// then append all schedules to conf.Schedules
	// Explicitly ignore any information that is not in a schedule.
	repos := GetRepos(&conf)
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

	return &conf, nil
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

// DirExists checks that a directory exists.
func DirExists(dirname string) bool {
	info, err := os.Stat(dirname)
	if os.IsNotExist(err) {
		return false
	}
	return info.IsDir()
}
