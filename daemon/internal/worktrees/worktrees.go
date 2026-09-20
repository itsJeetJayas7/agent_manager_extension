// Package worktrees implements git-based parallel isolation: integration
// branch + per-card worktrees, mechanical commits, deterministic merges,
// conflict detection, and explicit final application.
package worktrees

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Manager performs git operations for batches.
type Manager struct {
	root string // ~/.agent-conductor/worktrees
}

// NewManager creates the manager rooted at root.
func NewManager(root string) *Manager { return &Manager{root: root} }

// Root returns the worktree root directory.
func (m *Manager) Root() string { return m.root }

// PrecheckError explains why parallel execution is blocked.
type PrecheckError struct{ Reason string }

func (e *PrecheckError) Error() string { return e.Reason }

// gitEnvBlocklist names variables that silently redirect git away from the
// repository we asked for. A stray GIT_DIR in the daemon's environment (easy
// to acquire from a hook or a wrapper script) would corrupt every operation.
var gitEnvBlocklist = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_NAMESPACE",
	"GIT_CEILING_DIRECTORIES", "GIT_PREFIX",
}

func gitEnv() []string {
	out := make([]string, 0, len(os.Environ())+8)
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		blocked := false
		for _, b := range gitEnvBlocklist {
			if name == b {
				blocked = true
				break
			}
		}
		if !blocked {
			out = append(out, kv)
		}
	}
	return append(out,
		"GIT_AUTHOR_NAME=Agent Conductor", "GIT_AUTHOR_EMAIL=conductor@localhost",
		"GIT_COMMITTER_NAME=Agent Conductor", "GIT_COMMITTER_EMAIL=conductor@localhost",
		"GIT_CONFIG_NOSYSTEM=1",
	)
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	out := &bytes.Buffer{}
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

// RepoID derives a stable directory-safe id for a repo path.
func RepoID(repoPath string) string {
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		abs = repoPath
	}
	h := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(h[:8])
}

// Precheck verifies the repo supports parallel batch execution:
// git repo, no uncommitted tracked changes, no merge in progress.
//
// Untracked files are reported as a warning, not a blocker: build output, a
// local .env or a stray note used to make every parallel batch fail with a
// misleading "commit or stash your changes".
func (m *Manager) Precheck(repo string) error {
	_, err := m.PrecheckWithWarnings(repo)
	return err
}

// PrecheckWithWarnings additionally returns non-blocking observations.
func (m *Manager) PrecheckWithWarnings(repo string) (warnings []string, err error) {
	if out, gerr := git(repo, "rev-parse", "--is-inside-work-tree"); gerr != nil || out != "true" {
		return nil, &PrecheckError{Reason: "Parallel execution requires a Git repository. " + out}
	}
	out, gerr := git(repo, "status", "--porcelain", "--untracked-files=no")
	if gerr != nil {
		return nil, &PrecheckError{Reason: "git status failed: " + out}
	}
	if out != "" {
		return nil, &PrecheckError{Reason: "Parallel execution requires a clean Git worktree. Commit or stash your current changes first."}
	}
	// `.git` is a FILE inside a worktree and may live anywhere with a custom
	// --git-dir, so the path must come from git itself.
	if p, gerr := git(repo, "rev-parse", "--git-path", "MERGE_HEAD"); gerr == nil && p != "" {
		if !filepath.IsAbs(p) {
			p = filepath.Join(repo, p)
		}
		if _, statErr := os.Stat(p); statErr == nil {
			return nil, &PrecheckError{Reason: "Parallel execution requires no unresolved merge. Resolve the merge first."}
		}
	}
	if untracked, gerr := git(repo, "ls-files", "--others", "--exclude-standard"); gerr == nil && untracked != "" {
		n := len(strings.Split(untracked, "\n"))
		warnings = append(warnings, fmt.Sprintf("%d untracked file(s) will not be visible to parallel runs", n))
	}
	return warnings, nil
}

// Branch returns the current branch of repo.
func (m *Manager) Branch(repo string) (string, error) {
	out, err := git(repo, "rev-parse", "--abbrev-ref", "HEAD")
	return out, err
}

// Commit returns HEAD of repo.
func (m *Manager) Commit(repo string) (string, error) {
	out, err := git(repo, "rev-parse", "HEAD")
	return out, err
}

// BatchLayout describes the paths and branches for one batch.
type BatchLayout struct {
	IntegrationBranch string
	IntegrationDir    string
	CardBranchPrefix  string
}

// Layout computes naming for a batch.
//
// Branch names carry a short suffix derived from the batch id as well as the
// monotonic batch number. The number alone came from hashing the id into
// 1..89, so `conductor/b12/integration` was reused across batches and
// `git branch -f` destroyed the earlier batch's work without a word.
func (m *Manager) Layout(batchNum int, batchID, repo string) BatchLayout {
	base := filepath.Join(m.root, RepoID(repo), batchID)
	prefix := fmt.Sprintf("conductor/b%03d-%s", batchNum, shortID(batchID))
	return BatchLayout{
		IntegrationBranch: prefix + "/integration",
		IntegrationDir:    filepath.Join(base, "integration"),
		CardBranchPrefix:  prefix,
	}
}

// shortID is a stable 6-character, branch-name-safe tag for a batch id.
func shortID(batchID string) string {
	h := sha256.Sum256([]byte(batchID))
	return hex.EncodeToString(h[:3])
}

// createBranch creates a branch, failing loudly if the name is taken.
//
// `git branch -f` force-MOVES an existing branch. Combined with colliding
// names that silently destroyed a previous batch's integration branch and
// every commit on it.
func createBranch(repo, branch, start string) error {
	if _, err := git(repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		return fmt.Errorf("branch %s already exists; refusing to overwrite it", branch)
	}
	if out, err := git(repo, "branch", branch, start); err != nil {
		return fmt.Errorf("branch %s: %s", branch, out)
	}
	return nil
}

// CreateIntegration creates the integration branch + worktree from base commit.
func (m *Manager) CreateIntegration(repo, branch, dir, baseCommit string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if err := createBranch(repo, branch, baseCommit); err != nil {
		return err
	}
	if out, err := git(repo, "worktree", "add", "--checkout", dir, branch); err != nil {
		return fmt.Errorf("worktree add: %s", out)
	}
	return nil
}

// CardWorktree creates an isolated worktree for one parallel card.
func (m *Manager) CardWorktree(repo, branch, dir, baseBranch string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	integrationDir := filepath.Join(filepath.Dir(filepath.Clean(dir)), "integration")
	if err := createBranch(repo, branch, baseBranch); err != nil {
		return err
	}
	if out, err := git(repo, "worktree", "add", "--checkout", dir, branch); err != nil {
		return fmt.Errorf("worktree add: %s (%s)", out, integrationDir)
	}
	return nil
}

// IsDirty reports uncommitted changes in a worktree.
func (m *Manager) IsDirty(dir string) (bool, error) {
	out, err := git(dir, "status", "--porcelain")
	return out != "", err
}

// CommitAll makes a mechanical internal commit if there are changes.
// Returns true when a commit was created.
func (m *Manager) CommitAll(dir, message string) (bool, error) {
	dirty, err := m.IsDirty(dir)
	if err != nil {
		return false, err
	}
	if !dirty {
		return false, nil
	}
	if out, err := git(dir, "add", "-A"); err != nil {
		return false, fmt.Errorf("git add: %s", out)
	}
	if out, err := git(dir, "commit", "-m", message, "--no-verify"); err != nil {
		return false, fmt.Errorf("git commit: %s", out)
	}
	return true, nil
}

// DiffStat summarizes changes on a branch vs its base.
func (m *Manager) DiffStat(dir, base string) (summary string, files int, err error) {
	out, err := git(dir, "diff", "--stat", base+"...HEAD")
	if err != nil {
		return "", 0, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return "no changes", 0, nil
	}
	names, _ := git(dir, "diff", "--name-only", base+"...HEAD")
	n := 0
	for _, l := range strings.Split(names, "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	last := lines[len(lines)-1]
	return last, n, nil
}

// MergeConflictError reports conflicting files.
type MergeConflictError struct {
	Branch string
	Files  []string
	Output string
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("merge conflict merging %s: %s", e.Branch, strings.Join(e.Files, ", "))
}

// MergeBranch merges branch into the integration worktree's branch.
// Returns conflict error instead of proceeding on conflict.
func (m *Manager) MergeBranch(integrationDir, branch string) error {
	out, err := git(integrationDir, "merge", "--no-ff", "--no-edit", branch)
	if err != nil {
		if strings.Contains(out, "CONFLICT") || strings.Contains(out, "Merge conflicts") {
			filesOut, _ := git(integrationDir, "diff", "--name-only", "--diff-filter=U")
			files := []string{}
			for _, f := range strings.Split(filesOut, "\n") {
				if strings.TrimSpace(f) != "" {
					files = append(files, f)
				}
			}
			// abort the merge, preserve worktrees for manual resolution
			git(integrationDir, "merge", "--abort")
			return &MergeConflictError{Branch: branch, Files: files, Output: out}
		}
		return fmt.Errorf("merge %s: %s", branch, out)
	}
	return nil
}

// ApplyOptions guard final application to the user's original branch.
type ApplyOptions struct {
	Repo              string
	OriginalBranch    string
	OriginalCommit    string
	IntegrationBranch string
}

// ApplyCheck verifies the original branch hasn't moved and the tree is clean.
func (m *Manager) ApplyCheck(o ApplyOptions) error {
	if err := m.Precheck(o.Repo); err != nil {
		return err
	}
	cur, err := m.Branch(o.Repo)
	if err != nil {
		return err
	}
	if cur != o.OriginalBranch {
		return fmt.Errorf("original branch moved: expected %s, now on %s", o.OriginalBranch, cur)
	}
	head, err := m.Commit(o.Repo)
	if err != nil {
		return err
	}
	if head != o.OriginalCommit {
		return fmt.Errorf("original branch commit changed since batch start; verify before applying")
	}
	return nil
}

// Apply merges the integration branch into the original branch (explicit
// user action only).
func (m *Manager) Apply(o ApplyOptions) (string, error) {
	if err := m.ApplyCheck(o); err != nil {
		return "", err
	}
	// Take a safety ref first, so the user can always get back to exactly
	// where they were even after a merge they did not expect.
	backup := fmt.Sprintf("conductor/backup/%s", time.Now().UTC().Format("20060102-150405"))
	if err := createBranch(o.Repo, backup, o.OriginalCommit); err != nil {
		return "", fmt.Errorf("could not take a safety ref before applying: %w", err)
	}
	out, err := git(o.Repo, "merge", "--no-ff", "--no-edit", o.IntegrationBranch)
	if err != nil {
		git(o.Repo, "merge", "--abort")
		return "", fmt.Errorf("apply merge failed (aborted, your branch untouched; safety ref %s): %s", backup, out)
	}
	return out + "\n\nSafety ref: " + backup, nil
}

// CopyMergeCommand returns the manual merge command for the user.
func CopyMergeCommand(integrationBranch string) string {
	return fmt.Sprintf("git merge --no-ff --no-edit %s", integrationBranch)
}

// Cleanup removes the batch worktrees and their branches after success.
//
// Branches are deleted with `-d`, which refuses to drop unmerged commits.
// `-D` is used only for a branch provably merged into mergedInto, or when the
// caller passes force after an explicit user confirmation. The previous
// version always used `-D` and silently destroyed unmerged work.
func (m *Manager) Cleanup(repo string, dirs []string, branches []string) error {
	return m.CleanupWithPolicy(repo, dirs, branches, "", false)
}

// CleanupWithPolicy is Cleanup with an explicit safety policy.
func (m *Manager) CleanupWithPolicy(repo string, dirs, branches []string, mergedInto string, force bool) error {
	var kept []string
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if out, err := git(repo, "worktree", "remove", "--force", d); err != nil {
			_ = out
		}
	}
	git(repo, "worktree", "prune")
	for _, b := range branches {
		if b == "" {
			continue
		}
		if _, err := git(repo, "branch", "-d", b); err == nil {
			continue
		}
		if force || (mergedInto != "" && branchIsMerged(repo, b, mergedInto)) {
			if _, err := git(repo, "branch", "-D", b); err == nil {
				continue
			}
		}
		kept = append(kept, b)
	}
	if len(kept) > 0 {
		return fmt.Errorf("kept %d branch(es) with unmerged commits: %s", len(kept), strings.Join(kept, ", "))
	}
	return nil
}

// branchIsMerged reports whether every commit on branch is reachable from into.
func branchIsMerged(repo, branch, into string) bool {
	out, err := git(repo, "rev-list", "--count", into+".."+branch)
	return err == nil && out == "0"
}

// RepoRootOf resolves the repo root containing dir.
func RepoRootOf(dir string) (string, error) {
	out, err := git(dir, "rev-parse", "--show-toplevel")
	return out, err
}
