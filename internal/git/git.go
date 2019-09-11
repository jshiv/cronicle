package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
)

// Init returns a new repository using the .git folder, if the fixture
// is tagged as worktree the filesystem from fixture is used, otherwise a new
// memfs filesystem is used as worktree.
func Init(path string) *git.Repository {

	repo, err := git.PlainOpen(path) //
	if err != nil {
		//this feels wrong :-(
		repo, initErr := git.PlainInit(path, false)
		if initErr != nil {
			panic(initErr)
		}
		return repo
	}
	return repo

}

// CheckIfError should be used to naively panics if an error is not nil.
func CheckIfError(err error) {
	if err == nil {
		return
	}

	fmt.Printf("\x1b[31;1m%s\x1b[0m\n", fmt.Sprintf("error: %s", err))
	os.Exit(1)
}

// Info should be used to describe the example commands that are about to run.
func Info(format string, args ...interface{}) {
	fmt.Printf("\x1b[34;1m%s\x1b[0m\n", fmt.Sprintf(format, args...))
}

func Clone(gitURL string, dir string) {
	// Clones the repository into the given dir, just as a normal git clone does
	_, err := git.PlainClone(dir, false, &git.CloneOptions{
		URL: gitURL,
	})

	if err != nil {
		log.Fatal(err)
	}
}

func Commit(worktreeDir string, msg string) {
	// Opens an already existing repository.
	r, err := git.PlainOpen(worktreeDir)
	CheckIfError(err)

	w, err := r.Worktree()
	CheckIfError(err)

	Info("git add .")
	_, err = w.Add(".")

	// We can verify the current status of the worktree using the method Status.
	Info("git status --porcelain")
	status, err := w.Status()
	CheckIfError(err)

	fmt.Println(status)

	Info("git commit -m with message")
	commit, err := w.Commit(msg, &git.CommitOptions{
		Author: &object.Signature{
			Name: "Cronicle user",
			When: time.Now(),
		},
	})

	Info("git show -s")
	obj, err := r.CommitObject(commit)
	CheckIfError(err)

	fmt.Println(obj)
}

func GitLog(worktreeDir string) {
	// Opens an already existing repository.
	r, err := git.PlainOpen(worktreeDir)
	CheckIfError(err)

	// w, err := r.Worktree()
	// CheckIfError(err)

	// Gets the HEAD history from HEAD, just like this command:
	Info("git log")

	// ... retrieves the branch pointed by HEAD
	ref, err := r.Head()
	CheckIfError(err)

	cIter, err := r.Log(&git.LogOptions{From: ref.Hash()})
	CheckIfError(err)

	// ... just iterates over the commits, printing it
	err = cIter.ForEach(func(c *object.Commit) error {
		fmt.Println(c)  // commit as struct https://godoc.org/gopkg.in/src-d/go-git.v4/plumbing/object#Commit 
		return nil
	})
	CheckIfError(err)
}

func main() {
	// Clone("https://github.com/src-d/go-git.git", "/tmp/foo")
	// Commit("/Users/jessicas/work/cronicle", "example go-git commit")
	GitLog("/Users/jessicas/work/cronicle")
}

// // RunExample of how to:
// // - Clone a repository into memory
// // - Get the HEAD reference
// // - Using the HEAD reference, obtain the commit this reference is pointing to
// // - Using the commit, obtain its history and print it
// func RunExample() {
// 	// Clones the given repository, creating the remote, the local branches
// 	// and fetching the objects, everything in memory:
// 	Info("git clone https://github.com/src-d/go-siva")
// 	r, err := git.Clone(memory.NewStorage(), nil, &git.CloneOptions{
// 		URL: "https://github.com/src-d/go-siva",
// 	})
// 	CheckIfError(err)

// 	// Gets the HEAD history from HEAD, just like this command:
// 	Info("git log")

// 	// ... retrieves the branch pointed by HEAD
// 	ref, err := r.Head()
// 	CheckIfError(err)

// 	// ... retrieves the commit history
// 	cIter, err := r.Log(&git.LogOptions{From: ref.Hash()})
// 	CheckIfError(err)

// 	// ... just iterates over the commits, printing it
// 	err = cIter.ForEach(func(c *object.Commit) error {
// 		fmt.Println(c)

// 		return nil
// 	})
// 	CheckIfError(err)
// }
