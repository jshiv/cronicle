package cronicle

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	homedir "github.com/mitchellh/go-homedir"

	"github.com/go-git/go-git/v5"
	c "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// Git is the struct which associates common data structures from the go-git library.
type Git struct {
	Worktree      *git.Worktree
	Repository    *git.Repository
	Head          *plumbing.Reference
	Hash          *plumbing.Hash
	Commit        *object.Commit
	ReferenceName plumbing.ReferenceName
	authMethod    *transport.AuthMethod
}

// Auth authroizes a repository if from a local rsa key
func (repo *Repo) Auth() (transport.AuthMethod, error) {

	if repo.URL == "" {
		return nil, nil
	}

	if repo.DeployKey != "" {
		keyPath, err := homedir.Expand(repo.DeployKey)
		if err != nil {
			return nil, err
		}
		auth, err := ssh.NewPublicKeysFromFile("git", keyPath, "")
		auth.HostKeyCallback = gossh.InsecureIgnoreHostKey()
		return auth, err
	}

	// auth := http.BasicAuth{Username: repo.user, Password: repo.password}
	return nil, nil

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
func Clone(worktreeDir string, url string, auth *transport.AuthMethod) (Git, error) {

	if !DirExists(filepath.Join(worktreeDir, ".git")) {

		cloneOptions := git.CloneOptions{URL: url, Auth: *auth}

		_, err := git.PlainClone(worktreeDir, false, &cloneOptions)
		if err != nil {
			return Git{}, err
		}
	}

	var g Git
	g.authMethod = auth
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

	// When no branch was specified, resolve the remote's default branch
	// from `refs/remotes/origin/HEAD` (a symbolic ref set during Clone).
	// This picks `main` for repos where the remote default is main and
	// `master` for older repos — no guessing or hardcoded fallback per
	// platform convention. Fallback chain: remote HEAD → local HEAD →
	// "main" (the GitHub default since 2020) → bare "master" only if
	// nothing else resolves.
	if branch == "" && commit == "" {
		branch = g.defaultBranch()
	}

	var fetchOptions git.FetchOptions
	if g.authMethod == nil {
		fetchOptions = git.FetchOptions{
			RefSpecs: []c.RefSpec{"+refs/heads/*:refs/remotes/origin/*", "refs/*:refs/*"},
			Force:    true,
		}
	} else {
		fetchOptions = git.FetchOptions{
			RefSpecs: []c.RefSpec{"+refs/heads/*:refs/remotes/origin/*", "refs/*:refs/*"},
			Force:    true,
			Auth:     *g.authMethod,
		}
	}

	err := g.Repository.Fetch(&fetchOptions)
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
			Create: false, Force: true, Hash: h,
		}
	} else {
		b := plumbing.NewBranchReferenceName(branch)
		checkoutOptions = git.CheckoutOptions{
			Create: false, Force: true, Branch: b,
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

// defaultBranch returns the branch name to check out when neither branch
// nor commit is specified by the user. Fallback chain:
//
//  1. `refs/remotes/origin/HEAD` symbolic ref — only set when go-git
//     receives explicit info from the remote (HTTP smart protocol);
//     skipped for local file-protocol clones.
//  2. `main` if origin has it — modern GitHub default.
//  3. `master` if origin has it — legacy default for older repos.
//  4. Local HEAD's branch — covers init-without-remote setups.
//  5. Literal "main" as a last resort, since "master" is no longer
//     the GitHub convention.
//
// (2) and (3) check the remote-tracking ref so the answer reflects
// the remote's branches, not whatever the local HEAD happens to point
// at after a previous checkout.
func (g *Git) defaultBranch() string {
	if g == nil || g.Repository == nil {
		return "main"
	}

	const remoteHEAD = "refs/remotes/origin/HEAD"
	if ref, err := g.Repository.Reference(plumbing.ReferenceName(remoteHEAD), true); err == nil {
		full := ref.Name().String()
		const prefix = "refs/remotes/origin/"
		if strings.HasPrefix(full, prefix) {
			short := full[len(prefix):]
			if short != "" && short != "HEAD" {
				return short
			}
		}
	}

	for _, candidate := range []string{"main", "master"} {
		if _, err := g.Repository.Reference(
			plumbing.NewRemoteReferenceName("origin", candidate), false,
		); err == nil {
			return candidate
		}
	}

	if h, err := g.Repository.Head(); err == nil {
		if name := h.Name(); name.IsBranch() {
			return name.Short()
		}
	}
	return "main"
}
