package cronicle

import (
	"fmt"
	"path/filepath"
	"time"

	"gopkg.in/src-d/go-git.v4"
	c "gopkg.in/src-d/go-git.v4/config"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/object"

	"io/ioutil"
	"os"
	"path"
	"strings"

	"golang.org/x/crypto/ssh"
	"gopkg.in/src-d/go-git.v4/plumbing/transport"
	"gopkg.in/src-d/go-git.v4/plumbing/transport/http"
	gitssh "gopkg.in/src-d/go-git.v4/plumbing/transport/ssh"
)

// Git is the struct which associates common data structures from the go-git library.
type Git struct {
	Worktree      *git.Worktree
	Repository    *git.Repository
	Head          *plumbing.Reference
	Hash          *plumbing.Hash
	Commit        *object.Commit
	ReferenceName plumbing.ReferenceName
	authMethod    transport.AuthMethod
}

//Open populates a git struct for the given worktreePath
func (g *Git) Open(worktreePath string) error {
	r, err := git.PlainOpen(worktreePath)
	if err != nil {
		return err
	}

	g.Repository = r

	if r != nil {
		h, err := r.Head()
		if err != nil {
			return err
		}
		g.Head = h

		wt, err := r.Worktree()
		if err != nil {
			return err
		}
		g.Worktree = wt

		//Set head and Head and Commit state after opening worktree
		g.Head, err = g.Repository.Head()
		if err != nil {
			return err
		}
		g.Commit, err = g.Repository.CommitObject(g.Head.Hash())
		if err != nil {
			return err
		}
	}

	return nil
}

//Commit does a git commit on the repository at worktree
func Commit(worktreeDir string, msg string) {
	// Opens an already existing repository.
	r, _ := git.PlainOpen(worktreeDir)

	w, _ := r.Worktree()

	_, _ = w.Add(".")

	// We can verify the current status of the worktree using the method Status.
	status, _ := w.Status()

	fmt.Println(status)

	commit, _ := w.Commit(msg, &git.CommitOptions{
		Author: &object.Signature{
			Name: "Cronicle user",
			When: time.Now(),
		},
	})

	obj, _ := r.CommitObject(commit)

	fmt.Println(obj)
}

//Clone checks for the existance of worktreeDir/.git and clones if it does not exist
//then executes Git = GetGit(worktreeDir)
func Clone(worktreeDir string, repo string) (Git, error) {
	if !DirExists(filepath.Join(worktreeDir, ".git")) {

		// var cloneOptions git.CloneOptions
		// if sshKey != "" {
		// 	auth, err := sshKeyFromFile(sshKeyPath)
		// 	cloneOptions = git.CloneOptions{URL: repo, Auth: auth}
		// } else {
		// 	cloneOptions = git.CloneOptions{URL: repo}
		// }
		cloneOptions := git.CloneOptions{URL: repo}

		_, err := git.PlainClone(worktreeDir, false, &cloneOptions)
		if err != nil {
			return Git{}, err
		}
	}

	var g Git
	if err := g.Open(worktreeDir); err != nil {
		return g, err
	}

	return g, nil
}

//Checkout does a git fetch for task.Repo and does a git checkout for the
//given task.Branch or task.Commit.
//Note: Only one can be given, branch or commit.
//Checkout requires task.Repo to be given
func (g *Git) Checkout(branch string, commit string) error {

	if branch != "" && commit != "" {
		return ErrBranchAndCommitGiven
	}

	// var branch string
	if branch == "" {
		branch = "master"
	}

	err := g.Repository.Fetch(&git.FetchOptions{
		RefSpecs: []c.RefSpec{"refs/*:refs/*", "HEAD:refs/heads/HEAD"},
	})
	if err != nil {
		switch err {
		case git.NoErrAlreadyUpToDate:
		default:
			return err
		}
	}

	var checkoutOptions git.CheckoutOptions
	if commit != "" {
		h := plumbing.NewHash(commit)
		checkoutOptions = git.CheckoutOptions{
			Create: false, Force: false, Hash: h,
		}
	} else {
		b := plumbing.NewBranchReferenceName(branch)
		checkoutOptions = git.CheckoutOptions{
			Create: false, Force: false, Branch: b,
		}
	}

	if err := g.Worktree.Checkout(&checkoutOptions); err != nil {
		return err
	}

	//Set head and commit state after checkout branch/commit
	g.Head, err = g.Repository.Head()
	if err != nil {
		return err
	}
	g.Commit, err = g.Repository.CommitObject(g.Head.Hash())
	if err != nil {
		return err
	}

	return nil
}

//CleanGit nulls non-serlizable properties of a task
//task.Git = Git{}
func (task *Task) CleanGit() {
	task.Git = Git{}
}

func usernamePassword(username, password string) (transport.AuthMethod, error) {
	return &http.BasicAuth{
		Username: username,
		Password: password,
	}, nil
}

func sshKeyFromFile(fp string) (transport.AuthMethod, error) {
	if strings.HasPrefix(fp, "~/") {
		home, _ := os.UserHomeDir()
		fp = path.Join(home, fp[2:])
	}
	key, err := ioutil.ReadFile(fp)
	if err != nil {
		return nil, err
	}
	return sshKey(key)
}

func sshKey(key []byte) (transport.AuthMethod, error) {
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, err
	}

	return &gitssh.PublicKeys{
		User:   "git",
		Signer: signer,
	}, nil
}
