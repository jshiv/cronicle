package config

import (
	"fmt"
	"time"

	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
)

// Git is the struct which associates common data structures from the go-git library.
type Git struct {
	Worktree   *git.Worktree
	Repository *git.Repository
	Head       *plumbing.Reference
	Hash       *plumbing.Hash
	Commit     *object.Commit
}

// GetGit returns a git struct populated with git useful repo pointers
func GetGit(worktreePath string) Git {
	var g Git
	r, _ := git.PlainOpen(worktreePath)

	g.Repository = r

	h, _ := r.Head()
	g.Head = h

	wt, _ := r.Worktree()

	g.Worktree = wt

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
