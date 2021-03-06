package cronicle

import (
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	url "github.com/whilp/git-urls"

	"github.com/fatih/color"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"
	log "github.com/sirupsen/logrus"
	gossh "golang.org/x/crypto/ssh"
)

//Init initializes a default croniclePath with a .git repository,
//Basic schedule as code in a cronicle.hcl file and a repos folder.
func Init(croniclePath string, cloneRepo string, deployKey string, defaultConf Config) {

	absCroniclePath, err := filepath.Abs(croniclePath)
	if err != nil {
		log.Error(err)
	}

	//if remote is given, clone it to the cronicle path
	if cloneRepo != "" {
		var cloneOptions git.CloneOptions
		if deployKey != "" {
			auth, err := ssh.NewPublicKeysFromFile("git", deployKey, "")
			auth.HostKeyCallback = gossh.InsecureIgnoreHostKey()
			if err != nil {
				log.Fatal(err)
			}
			cloneOptions = git.CloneOptions{URL: cloneRepo, Auth: auth}
		} else {
			cloneOptions = git.CloneOptions{URL: cloneRepo}
		}

		_, err = git.PlainClone(absCroniclePath, false, &cloneOptions)
		if err != nil {
			log.Fatal(err)
		}
	}

	slantyedCyan := color.New(color.FgCyan, color.Italic).SprintFunc()
	// errors.New("could not extract repos from " + slantedRed("Config"))
	os.MkdirAll(path.Join(absCroniclePath, path.Join(".cronicle", "repos")), 0777)
	cronicleFile := path.Join(absCroniclePath, "cronicle.hcl")
	fmt.Println("Init Cronicle: " + slantyedCyan(cronicleFile))

	if fileExists(cronicleFile) {
		conf, err := GetConfig(cronicleFile)
		if err != nil {
			os.Exit(1)
		}
		hcl := conf.Hcl()
		fmt.Printf("%s", slantyedCyan(string(hcl.Bytes)))
		// CloneRepos(absCroniclePath, conf)
	} else {
		MarshallHcl(defaultConf, cronicleFile)
		f, err := os.OpenFile(path.Join(absCroniclePath, ".gitignore"),
			os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Println(err)
		}
		defer f.Close()
		if _, err := f.WriteString(".cronicle\n"); err != nil {
			log.Println(err)
		}
	}

}

// GetRepos collects the set of repos associated to a given config
func GetRepos(conf *Config) map[string]bool {
	repos := map[string]bool{}
	for _, repo := range conf.Repos {
		repos[repo] = true
	}
	for _, sched := range conf.Schedules {
		if sched.Repo != nil {
			repos[sched.Repo.URL] = true
		}
		for _, task := range sched.Tasks {
			if task.Repo != nil {
				repos[task.Repo.URL] = true
			}
		}
	}

	return repos
}

//LocalRepoDir takes a cronicle.hcl path and a github repo URL and converts
//it to the local clone of that repo
func LocalRepoDir(croniclePath string, repoURL string) (string, error) {
	dotCronicleRepos := path.Join(".cronicle", "repos")
	reposDir := path.Join(croniclePath, dotCronicleRepos)
	u, err := url.Parse(repoURL)
	if err != nil {
		return "", err
	}
	localRepoDir := path.Join(reposDir, u.Path)

	return localRepoDir, nil
}

//CleanGit nulls non-serlizable properties of a schedule
//task.Git = Git{}
func (schedule *Schedule) CleanGit() {
	for i := range schedule.Tasks {
		schedule.Tasks[i].CleanGit()
	}
}

//Init populates task repo path, runs git clone for any sub repos,
//and assigns Git meta data to the task
func (conf *Config) Init(croniclePath string) error {
	// Assign the path for each task or schedule repo
	conf.PropigateTaskProperties(croniclePath)
	if err := conf.Validate(); err != nil {
		return err
	}

	//If conf.Repo is a given repo, clone and fetch
	if conf.Repo != nil {
		auth, err := conf.Repo.Auth()
		if err != nil {
			return err
		}
		g, err := Clone(croniclePath, conf.Repo.URL, &auth)
		if err != nil {
			return err
		}
		if err := g.Checkout(conf.Repo.Branch, conf.Repo.Commit); err != nil {
			return err
		}
	}

	for _, schedule := range conf.Schedules {
		for _, task := range schedule.Tasks {
			if err := task.Validate(); err != nil {
				return err
			}
			if task.Repo != nil {
				auth, err := task.Repo.Auth()
				if err != nil {
					return err
				}
				if _, err := Clone(task.Path, task.Repo.URL, &auth); err != nil {
					// if _, err := Clone(task.Path, task.Repo.URL, task.Repo.DeployKey); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// GetConfig returns the Config specified by the given cronicle.hcl file
// Including any Cronicle files specified by in the repos directory.
func GetConfig(cronicleFile string) (*Config, error) {
	cronicleFileAbs, err := filepath.Abs(cronicleFile)
	if err != nil {
		return nil, err
	}
	croniclePath := filepath.Dir(cronicleFileAbs)

	parser := hclparse.NewParser()
	wr := hcl.NewDiagnosticTextWriter(
		os.Stdout,      // writer to send messages to
		parser.Files(), // the parser's file cache, for source snippets
		78,             // wrapping width
		true,           // generate colored/highlighted output
	)
	conf, diags := ParseFile(cronicleFileAbs, parser)

	if diags.HasErrors() {
		wr.WriteDiagnostics(diags)
		return conf, fmt.Errorf("cronicle.hcl parse: %w", diags)
	}

	err = conf.Init(croniclePath)
	if err != nil {
		return conf, err
	}
	// conf.PropigateTaskProperties(croniclePath)

	// if err := SetConfig(&conf, croniclePath); err != nil {
	// 	return &conf, err
	// }

	// Collect any sub level Cronicle files if they exist
	// then append all schedules to conf.Schedules
	// Explicitly ignore any information that is not in a schedule.
	repos := GetRepos(conf)
	for repo := range repos {
		repoPath, _ := LocalRepoDir(croniclePath, repo)
		repoCronicleFile := filepath.Join(repoPath, "cronicle.hcl")
		if fileExists(repoCronicleFile) {
			repoConf, err := GetConfig(repoCronicleFile)
			if err != nil {
				return conf, err
			}
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

// DirExists checks that a directory exists.
func DirExists(dirname string) bool {
	info, err := os.Stat(dirname)
	if os.IsNotExist(err) {
		return false
	}
	return info.IsDir()
}
