package cronicle

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	homedir "github.com/mitchellh/go-homedir"

	"github.com/go-git/go-git/v5"
	c "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
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

// Auth returns the transport.AuthMethod implied by the repo block.
// Branches, in order:
//
//  1. URL empty → no auth (local-path clone or no remote).
//  2. DeployKey set → SSH public-key auth from the file at DeployKey.
//  3. Password non-empty → HTTP Basic auth. Username defaults to "x"
//     (git's HTTP Basic challenge requires SOME username; modern hosts
//     ignore the value). The Password field is a parsed HCL string,
//     so HCL like `password = "${env.CRONICLE_TOKEN}"` has already
//     resolved to the env-var value by the time this runs. An empty
//     Password skips this branch (anonymous), letting laptops without
//     the env var still clone public repos cleanly.
//  4. Otherwise → nil (anonymous).
//
// The Basic-auth branch is host-agnostic: GitHub, GitLab, Gitea, the
// cronicle-hosted git server all accept "any-username + token-in-
// password-slot" over HTTPS, so one shape covers every common case
// without hardcoding a hostname or env-var name.
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
		// NewPublicKeysFromFile returns (nil, err) when the key file is
		// missing or unreadable. Checking err BEFORE touching auth fields
		// avoids a nil-pointer SIGSEGV — the original bug here let a
		// missing key path crash the process.
		if err != nil {
			return nil, fmt.Errorf("load ssh deploy key %q: %w", keyPath, err)
		}
		auth.HostKeyCallback = gossh.InsecureIgnoreHostKey()
		return auth, nil
	}

	if repo.Password != "" {
		user := repo.Username
		if user == "" {
			user = "x"
		}
		return &http.BasicAuth{Username: user, Password: repo.Password}, nil
	}

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
//
//Logs each step (clone / skip / open) so operators can see git activity
//in the pretty/json/text stdout streams — previously these operations
//were silent and the only signal that cloning happened was the
//directory appearing on disk.
func Clone(worktreeDir string, url string, auth *transport.AuthMethod) (Git, error) {

	var effectiveAuth transport.AuthMethod
	if auth != nil && *auth != nil {
		effectiveAuth = *auth
	}

	if !DirExists(filepath.Join(worktreeDir, ".git")) {
		slog.Info("git clone", "url", url, "path", worktreeDir)
		cloneOptions := git.CloneOptions{URL: url, Auth: effectiveAuth}
		// In pretty mode, route go-git's progress sideband straight to
		// stdout so the user sees the live "Counting objects" / "Receiving
		// objects" lines (the same UX as the host `git` CLI). In file/Loki
		// modes we deliberately omit Progress — those sinks already get
		// the structured "git clone" / "git clone complete" slog records,
		// and the per-percent overwrite lines (carriage-return updates)
		// just produce noise in line-oriented log files.
		if IsStreamingPretty() {
			cloneOptions.Progress = os.Stdout
		}

		_, err := git.PlainClone(worktreeDir, false, &cloneOptions)
		if err != nil {
			slog.Error("git clone failed", "url", url, "path", worktreeDir, "error", err.Error())
			return Git{}, err
		}
		slog.Info("git clone complete", "url", url, "path", worktreeDir)
	} else {
		slog.Info("git open", "path", worktreeDir, "note", "worktree already exists, skipping clone")
	}

	// Stash the effective auth (caller-supplied OR auto-filled cronicle-
	// hosted bearer) so Checkout's fetch uses the same credentials the
	// Clone used. Without this, Checkout would fall back to the caller's
	// nil auth and the fetch against a cronicle-hosted repo would 401.
	var stored transport.AuthMethod
	if effectiveAuth != nil {
		stored = effectiveAuth
	} else if auth != nil {
		stored = *auth
	}
	var g Git
	g.authMethod = &stored
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

	slog.Info("git fetch", "refs", "+refs/heads/*:refs/remotes/origin/*")
	err := g.Repository.Fetch(&fetchOptions)
	if err != nil {
		switch err {
		case git.NoErrAlreadyUpToDate:
			slog.Info("git fetch: already up to date")
		default:
			slog.Error("git fetch failed", "error", err.Error())
			return err
		}
	}

	var checkoutOptions git.CheckoutOptions
	var checkoutTarget string
	if commit != "" {
		h := plumbing.NewHash(commit)
		checkoutOptions = git.CheckoutOptions{
			Create: false, Force: true, Hash: h,
		}
		checkoutTarget = "commit=" + commit
	} else {
		b := plumbing.NewBranchReferenceName(branch)
		checkoutOptions = git.CheckoutOptions{
			Create: false, Force: true, Branch: b,
		}
		checkoutTarget = "branch=" + branch
	}

	slog.Info("git checkout", "target", checkoutTarget)
	if err := g.Worktree.Checkout(&checkoutOptions); err != nil {
		slog.Error("git checkout failed", "target", checkoutTarget, "error", err.Error())
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
