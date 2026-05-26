// Built-in git tool for agents. Wraps go-git so agent tasks can read git
// state and produce commits without the git CLI being installed on the
// host — preserving cronicle's "single binary on a bare machine" property.
//
// Subcommands:
//   status — list tracked changes (porcelain-style summary)
//   log    — recent commits (oneline by default)
//   diff   — unified diff: working tree vs HEAD, or two refs
//   branch — create + switch to a new branch from HEAD
//   commit — stage all + commit with a message
//   push   — push current branch using the same auth as the task's repo
//
// Push reuses the task.Repo auth method (same SSH key used for the clone),
// so an agent that opened a PR-prep flow doesn't need separate credentials.
// Read-only operations don't need auth.

package cronicle

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/go-git/go-git/v6"
	gitconfig "github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// GitTool exposes a small subset of git operations to the agent via go-git.
// Workspace must be the path to a git working tree (typically task.Path
// after cronicle.Clone). Auth is optional and only consumed by push.
//
// Author/Email default to "cronicle agent" / "cronicle@local" when a commit
// is made without an explicit author override; matches the convention used
// by cronicle.Commit elsewhere in the package.
type GitTool struct {
	Workspace string
	Auth      []client.Option
}

func (g *GitTool) Name() string { return "git" }

// Definition declares git as a custom Anthropic tool with a single string
// `command` plus a free-form `args` object that varies per subcommand. The
// description is directive — the model picks the subcommand from the
// description, not from a separate enum, so descriptions of each
// subcommand are inline.
func (g *GitTool) Definition() anthropic.ToolUnionParam {
	return anthropic.ToolUnionParam{
		OfTool: &anthropic.ToolParam{
			Name:        "git",
			Type:        anthropic.ToolTypeCustom,
			Description: anthropic.String(gitToolDescription),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "One of: status, log, diff, branch, commit, push.",
					},
					"args": map[string]any{
						"type":        "object",
						"description": "Per-subcommand arguments. See tool description.",
					},
				},
				Required: []string{"command"},
			},
		},
	}
}

const gitToolDescription = `Run git operations against the task's working tree using cronicle's embedded go-git (no host git CLI needed).

Subcommands and their args:
  status                                     -> list tracked changes
  log    {limit?: int}                       -> recent commits, default 10
  diff   {from?: ref, to?: ref, path?: str}  -> unified diff. Defaults: from=HEAD, to=worktree
  branch {name: str}                         -> create+switch to a new branch from current HEAD
  commit {message: str, author?: str, email?: str}  -> stage all changes + commit
  push   {remote?: str, branch?: str}        -> push current branch. remote defaults to "origin"

Read-only ops (status/log/diff) work in any cronicle clone. push needs the task to have a repo block so the same auth as the original clone is reused.`

// Execute dispatches to the named subcommand. Errors return isErr=true with
// a one-line message; success returns the human-readable output the model
// would expect from the corresponding git CLI invocation.
func (g *GitTool) Execute(ctx context.Context, input json.RawMessage) (string, bool) {
	var args struct {
		Command string          `json:"command"`
		Args    json.RawMessage `json:"args,omitempty"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Sprintf("Error: invalid git input: %v", err), true
	}
	if args.Command == "" {
		return "Error: git command required (one of: status, log, diff, branch, commit, push)", true
	}
	repo, err := git.PlainOpen(g.Workspace)
	if err != nil {
		return fmt.Sprintf("Error: open git repo at %s: %v", g.Workspace, err), true
	}
	switch args.Command {
	case "status":
		return g.status(repo)
	case "log":
		return g.log(repo, args.Args)
	case "diff":
		return g.diff(repo, args.Args)
	case "branch":
		return g.branch(repo, args.Args)
	case "commit":
		return g.commit(repo, args.Args)
	case "push":
		return g.push(ctx, repo, args.Args)
	default:
		return fmt.Sprintf("Error: unknown git command %q", args.Command), true
	}
}

// status renders the worktree status in a porcelain-ish format. Empty
// status returns a clear "nothing to commit" so the agent can decide
// whether commit is appropriate.
func (g *GitTool) status(repo *git.Repository) (string, bool) {
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Sprintf("Error: worktree: %v", err), true
	}
	st, err := wt.Status()
	if err != nil {
		return fmt.Sprintf("Error: status: %v", err), true
	}
	if st.IsClean() {
		return "On working tree (clean): nothing to commit", false
	}
	// Sort paths for stable output across runs.
	paths := make([]string, 0, len(st))
	for p := range st {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var sb strings.Builder
	for _, p := range paths {
		s := st[p]
		sb.WriteString(fmt.Sprintf(" %s%s %s\n", string(s.Staging), string(s.Worktree), p))
	}
	return strings.TrimRight(sb.String(), "\n"), false
}

// log returns the most recent N commits in `<short-sha> <subject>` form.
// Default limit 10. The model can ask for more by setting limit explicitly,
// up to 200 — beyond that, large repos start producing tool_results that
// blow past the next-turn context budget.
func (g *GitTool) log(repo *git.Repository, raw json.RawMessage) (string, bool) {
	var a struct {
		Limit int `json:"limit"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &a)
	}
	if a.Limit <= 0 {
		a.Limit = 10
	}
	if a.Limit > 200 {
		a.Limit = 200
	}
	head, err := repo.Head()
	if err != nil {
		return fmt.Sprintf("Error: head: %v", err), true
	}
	iter, err := repo.Log(&git.LogOptions{From: head.Hash()})
	if err != nil {
		return fmt.Sprintf("Error: log: %v", err), true
	}
	var sb strings.Builder
	count := 0
	err = iter.ForEach(func(c *object.Commit) error {
		if count >= a.Limit {
			return storerStop
		}
		subject := strings.SplitN(strings.TrimSpace(c.Message), "\n", 2)[0]
		sb.WriteString(fmt.Sprintf("%s %s\n", c.Hash.String()[:7], subject))
		count++
		return nil
	})
	if err != nil && err != storerStop {
		return fmt.Sprintf("Error: log iter: %v", err), true
	}
	if count == 0 {
		return "(no commits)", false
	}
	return strings.TrimRight(sb.String(), "\n"), false
}

// storerStop is a sentinel for breaking out of go-git's ForEach iterators.
// go-git treats any non-nil non-special error as an iterator failure, so
// our own value distinguishes "intentional stop" from real errors.
var storerStop = fmt.Errorf("stop")

// diff produces a unified diff. With no args, it diffs HEAD vs working
// tree. With `from` and `to`, both as refs (branch name, hash, or
// HEAD~N — anything ResolveRevision accepts), it diffs those two trees.
// Optional `path` filters to a single file.
func (g *GitTool) diff(repo *git.Repository, raw json.RawMessage) (string, bool) {
	var a struct {
		From string `json:"from"`
		To   string `json:"to"`
		Path string `json:"path"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &a)
	}
	// Resolve "from". HEAD by default.
	if a.From == "" {
		a.From = "HEAD"
	}
	fromHash, err := repo.ResolveRevision(plumbing.Revision(a.From))
	if err != nil {
		return fmt.Sprintf("Error: resolve %q: %v", a.From, err), true
	}
	fromCommit, err := repo.CommitObject(*fromHash)
	if err != nil {
		return fmt.Sprintf("Error: commit %s: %v", fromHash, err), true
	}
	fromTree, err := fromCommit.Tree()
	if err != nil {
		return fmt.Sprintf("Error: tree: %v", err), true
	}

	// "to" can be another ref OR the working tree (default).
	if a.To == "" {
		// HEAD-vs-worktree diff: compose from worktree status (changed
		// files) + per-file content diff against the from tree.
		return g.diffWorktree(repo, fromTree, a.Path)
	}
	toHash, err := repo.ResolveRevision(plumbing.Revision(a.To))
	if err != nil {
		return fmt.Sprintf("Error: resolve %q: %v", a.To, err), true
	}
	toCommit, err := repo.CommitObject(*toHash)
	if err != nil {
		return fmt.Sprintf("Error: commit %s: %v", toHash, err), true
	}
	toTree, err := toCommit.Tree()
	if err != nil {
		return fmt.Sprintf("Error: tree: %v", err), true
	}
	patch, err := fromTree.Patch(toTree)
	if err != nil {
		return fmt.Sprintf("Error: patch: %v", err), true
	}
	out := patch.String()
	if a.Path != "" {
		out = filterPatch(out, a.Path)
	}
	out = truncateOutput(out, MaxToolOutputBytes)
	if out == "" {
		return "(no diff)", false
	}
	return out, false
}

// diffWorktree falls back to a simple "modified files" listing for
// HEAD-vs-worktree because go-git doesn't ship a unified-diff worktree
// patch helper. The agent typically follows up with text_editor or a
// from/to ref-pair diff for actual content.
func (g *GitTool) diffWorktree(repo *git.Repository, _ *object.Tree, path string) (string, bool) {
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Sprintf("Error: worktree: %v", err), true
	}
	st, err := wt.Status()
	if err != nil {
		return fmt.Sprintf("Error: status: %v", err), true
	}
	paths := make([]string, 0, len(st))
	for p := range st {
		if path != "" && p != path {
			continue
		}
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		return "(no working-tree changes)", false
	}
	sort.Strings(paths)
	var sb strings.Builder
	sb.WriteString("Working-tree changes (use diff with from/to refs for content):\n")
	for _, p := range paths {
		s := st[p]
		sb.WriteString(fmt.Sprintf(" %s%s %s\n", string(s.Staging), string(s.Worktree), p))
	}
	return strings.TrimRight(sb.String(), "\n"), false
}

// filterPatch keeps only the diff sections that touch a specific path,
// for the rare case where the patch is large and the agent only cares
// about one file. Cheap string scan; not perfect but adequate for the
// agent's reading purposes.
func filterPatch(s, path string) string {
	parts := strings.Split(s, "diff --git ")
	var keep []string
	for _, p := range parts {
		if strings.Contains(p, path) {
			keep = append(keep, p)
		}
	}
	if len(keep) == 0 {
		return ""
	}
	return "diff --git " + strings.Join(keep, "diff --git ")
}

// branch creates a new branch from the current HEAD and switches the
// worktree to it. Equivalent to `git checkout -b <name>`. Refuses if
// the branch already exists, mirroring git's safety on -b.
func (g *GitTool) branch(repo *git.Repository, raw json.RawMessage) (string, bool) {
	var a struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return fmt.Sprintf("Error: invalid branch args: %v", err), true
	}
	if a.Name == "" {
		return "Error: branch name required", true
	}
	head, err := repo.Head()
	if err != nil {
		return fmt.Sprintf("Error: head: %v", err), true
	}
	refName := plumbing.NewBranchReferenceName(a.Name)
	if _, err := repo.Reference(refName, false); err == nil {
		return fmt.Sprintf("Error: branch %q already exists", a.Name), true
	}
	ref := plumbing.NewHashReference(refName, head.Hash())
	if err := repo.Storer.SetReference(ref); err != nil {
		return fmt.Sprintf("Error: create ref: %v", err), true
	}
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Sprintf("Error: worktree: %v", err), true
	}
	// Keep:true preserves uncommitted changes through the switch, matching
	// what `git checkout -b <name>` does at the CLI when the new branch
	// shares HEAD with the current one (always true here, since we just
	// created the ref from current HEAD).
	if err := wt.Checkout(&git.CheckoutOptions{Branch: refName, Keep: true}); err != nil {
		return fmt.Sprintf("Error: checkout: %v", err), true
	}
	return fmt.Sprintf("Switched to a new branch %q (from %s)", a.Name, head.Hash().String()[:7]), false
}

// commit stages all worktree changes and creates a commit. Author/email
// default to "cronicle agent" / "cronicle@local" — these are signature
// fields on the commit object, separate from any human attribution. If
// the worktree is clean, returns an explicit "nothing to commit" rather
// than creating an empty commit.
func (g *GitTool) commit(repo *git.Repository, raw json.RawMessage) (string, bool) {
	var a struct {
		Message string `json:"message"`
		Author  string `json:"author"`
		Email   string `json:"email"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return fmt.Sprintf("Error: invalid commit args: %v", err), true
	}
	if a.Message == "" {
		return "Error: commit message required", true
	}
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Sprintf("Error: worktree: %v", err), true
	}
	st, err := wt.Status()
	if err != nil {
		return fmt.Sprintf("Error: status: %v", err), true
	}
	if st.IsClean() {
		return "Error: nothing to commit (working tree clean)", true
	}
	if _, err := wt.Add("."); err != nil {
		return fmt.Sprintf("Error: stage: %v", err), true
	}
	if a.Author == "" {
		a.Author = "cronicle agent"
	}
	if a.Email == "" {
		a.Email = "cronicle@local"
	}
	hash, err := wt.Commit(a.Message, &git.CommitOptions{
		Author: &object.Signature{
			Name:  a.Author,
			Email: a.Email,
			When:  time.Now(),
		},
	})
	if err != nil {
		return fmt.Sprintf("Error: commit: %v", err), true
	}
	return fmt.Sprintf("Committed %s on branch (use git push to publish)", hash.String()[:7]), false
}

// push pushes the current branch to the named remote (origin by default).
// Reuses the auth method captured at clone time — no separate credentials
// flow. If the agent ran on a task with no `repo` block (so no auth was
// captured), push will likely fail on private remotes.
func (g *GitTool) push(ctx context.Context, repo *git.Repository, raw json.RawMessage) (string, bool) {
	var a struct {
		Remote string `json:"remote"`
		Branch string `json:"branch"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &a)
	}
	if a.Remote == "" {
		a.Remote = "origin"
	}
	opts := &git.PushOptions{RemoteName: a.Remote, ClientOptions: g.Auth}
	if a.Branch != "" {
		// Push only the named branch with `refs/heads/<x>:refs/heads/<x>`.
		full := string(plumbing.NewBranchReferenceName(a.Branch))
		opts.RefSpecs = []gitconfig.RefSpec{gitconfig.RefSpec(full + ":" + full)}
	}
	if err := repo.PushContext(ctx, opts); err != nil {
		if err == git.NoErrAlreadyUpToDate {
			return "Already up to date", false
		}
		return fmt.Sprintf("Error: push: %v", err), true
	}
	return fmt.Sprintf("Pushed to %s", a.Remote), false
}
