package git

import (
	"gopkg.in/src-d/go-git.v4"
)

// Init returns a new repository using the .git folder, if the fixture
// is tagged as worktree the filesystem from fixture is used, otherwise a new
// memfs filesystem is used as worktree.
func Init(path string) *git.Repository {

	repo, err := git.PlainOpen(path)//
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

