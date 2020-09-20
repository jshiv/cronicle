package config

import (
	"fmt"
	"path/filepath"
	"time"

	"gopkg.in/src-d/go-git.v4"
	c "gopkg.in/src-d/go-git.v4/config"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
)

// Git is the struct which associates common data structures from the go-git library.
type Git struct {
	Worktree      *git.Worktree
	Repository    *git.Repository
	Head          *plumbing.Reference
	Hash          *plumbing.Hash
	Commit        *object.Commit
	ReferenceName plumbing.ReferenceName
}

//Open populates a git struct for the given worktreePath
func (g *Git) Open(worktreePath string) error {
	r, err := git.PlainOpen(worktreePath)
	if err != nil {
		return err
	}

	g.Repository = r

	if r != nil {
		if h, err := r.Head(); err != nil {
			return err
		} else {
			g.Head = h
		}

		if wt, err := r.Worktree(); err != nil {
			return err
		} else {
			g.Worktree = wt
		}
	}

	return nil
}

// GetGit returns a git struct populated with git useful repo pointers
func GetGit(worktreePath string) Git {
	var g Git
	r, err := git.PlainOpen(worktreePath)
	if err != nil {
		fmt.Println(err)
	}

	g.Repository = r

	if r != nil {
		if h, err := r.Head(); err != nil {
			fmt.Println("=================")
			fmt.Println(err)
			fmt.Println("=================")

		} else {
			g.Head = h
		}

		if wt, err := r.Worktree(); err != nil {
			fmt.Println(err)
		} else {
			g.Worktree = wt
		}
	}

	return g

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

//Clone checks for the existance of task.Path/.git and clones if it does not exist
//then executes task.Git = GetGit(task.Path)
func (task *Task) Clone() error {
	if !DirExists(filepath.Join(task.Path, ".git")) {

		_, err := git.PlainClone(task.Path, false, &git.CloneOptions{URL: task.Repo})
		if err != nil {
			return err
		}
	}

	if err := task.Git.Open(task.Path); err != nil {
		return err
	}

	if task.Branch != "" {
		task.Git.ReferenceName = plumbing.NewBranchReferenceName(task.Branch)
	} else {
		task.Git.ReferenceName = plumbing.HEAD
	}

	return nil
}

//Checkout does a git fetch for task.Repo and does a git checkout for the
//given task.Branch or task.Commit.
//Note: Only one can be given, branch or commit.
//Checkout requires task.Repo to be given
func (task *Task) Checkout() error {

	if task.Repo == "" {
		return ErrRepoNotGiven
	}

	if err := task.Validate(); err != nil {
		return err
	}

	var branch string
	if task.Branch != "" {
		branch = task.Branch
	} else {
		branch = "master"
	}

	var commit string
	if task.Commit != "" {
		commit = task.Commit
	}

	err := task.Git.Repository.Fetch(&git.FetchOptions{
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

	if err := task.Git.Worktree.Checkout(&checkoutOptions); err != nil {
		return err
	}

	//Set head and commit state after checkout branch/commit
	task.Git.Head, err = task.Git.Repository.Head()
	if err != nil {
		return err
	}
	task.Git.Commit, err = task.Git.Repository.CommitObject(task.Git.Head.Hash())
	if err != nil {
		return err
	}

	return nil
}

//SetGit sets executes GetGit after a plain clone if a repo is given
func (task *Task) SetGit() {
	if !DirExists(filepath.Join(task.Path, ".git")) {

		_, err := git.PlainClone(task.Path, false, &git.CloneOptions{URL: task.Repo})
		if err != nil {
			fmt.Println(err)
		}
	}
	task.Git = GetGit(task.Path)

	if task.Branch != "" {
		task.Git.ReferenceName = plumbing.NewBranchReferenceName(task.Branch)
	} else {
		task.Git.ReferenceName = plumbing.HEAD
	}
}

//CleanGit nulls non-serlizable properties of a task
//task.Git = Git{}
func (task *Task) CleanGit() {
	task.Git = Git{}
}
