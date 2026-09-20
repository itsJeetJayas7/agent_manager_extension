// Package worktrees implements git-based parallel isolation: integration
// branch + per-card worktrees, mechanical commits, deterministic merges,
// conflict detection, and explicit final application.
package worktrees

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Agent Conductor", "GIT_AUTHOR_EMAIL=conductor@localhost",
		"GIT_COMMITTER_NAME=Agent Conductor", "GIT_COMMITTER_EMAIL=conductor@localhost",
		"GIT_CONFIG_NOSYSTEM=1",
	)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
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
// git repo, clean tree, no merge in progress.
func (m *Manager) Precheck(repo string) error {
	if out, err := git(repo, "rev-parse", "--is-inside-work-tree"); err != nil || out != "true" {
		return &PrecheckError{Reason: "Parallel execution requires a Git repository. " + out}
	}
	if out, err := git(repo, "status", "--porcelain"); err != nil {
		return &PrecheckError{Reason: "git status failed: " + out}
	} else if out != "" {
		return &PrecheckError{Reason: "Parallel execution requires a clean Git worktree. Commit or stash your current changes first."}
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "MERGE_HEAD")); err == nil {
		return &PrecheckError{Reason: "Parallel execution requires no unresolved merge. Resolve the merge first."}
	}
	return nil
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
func (m *Manager) Layout(batchNum int, batchID, repo string) BatchLayout {
	base := filepath.Join(m.root, RepoID(repo), batchID)
	return BatchLayout{
		IntegrationBranch: fmt.Sprintf("conductor/b%d/integration", batchNum),
		IntegrationDir:    filepath.Join(base, "integration"),
		CardBranchPrefix:  fmt.Sprintf("conductor/b%d", batchNum),
	}
}

// CreateIntegration creates the integration branch + worktree from base commit.
func (m *Manager) CreateIntegration(repo, branch, dir, baseCommit string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if _, err := git(repo, "branch", "-f", branch, baseCommit); err != nil {
		return fmt.Errorf("branch %s: %w", branch, err)
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
	if _, err := git(repo, "branch", "-f", branch, baseBranch); err != nil {
		return fmt.Errorf("branch %s: %w", branch, err)
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
	out, err := git(o.Repo, "merge", "--no-ff", "--no-edit", o.IntegrationBranch)
	if err != nil {
		git(o.Repo, "merge", "--abort")
		return "", fmt.Errorf("apply merge failed (aborted, your branch untouched): %s", out)
	}
	return out, nil
}

// CopyMergeCommand returns the manual merge command for the user.
func CopyMergeCommand(integrationBranch string) string {
	return fmt.Sprintf("git merge --no-ff --no-edit %s", integrationBranch)
}

// Cleanup removes the batch worktrees and their branches after success.
func (m *Manager) Cleanup(repo string, dirs []string, branches []string) error {
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if out, err := git(repo, "worktree", "remove", "--force", d); err != nil {
			_ = out
		}
	}
	for _, b := range branches {
		if b == "" {
			continue
		}
		git(repo, "branch", "-D", b)
	}
	return nil
}

// RepoRootOf resolves the repo root containing dir.
func RepoRootOf(dir string) (string, error) {
	out, err := git(dir, "rev-parse", "--show-toplevel")
	return out, err
}
