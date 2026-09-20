package worktrees

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "main")
	os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644)
	run("add", "-A")
	run("commit", "-m", "init")
	return dir
}

func wt(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(filepath.Join(t.TempDir(), "wts"))
	return m
}

func TestPrecheckCleanVsDirty(t *testing.T) {
	repo := initRepo(t)
	m := wt(t)
	if err := m.Precheck(repo); err != nil {
		t.Fatalf("clean repo should pass: %v", err)
	}
	os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("x"), 0o644)
	err := m.Precheck(repo)
	if err == nil {
		t.Fatal("dirty repo must be blocked")
	}
	if !strings.Contains(err.Error(), "clean Git worktree") {
		t.Fatalf("wrong message: %v", err)
	}
}

func TestParallelIsolationAndIntegration(t *testing.T) {
	repo := initRepo(t)
	m := wt(t)
	base, _ := m.Commit(repo)
	layout := m.Layout(14, "b14", repo)
	if err := m.CreateIntegration(repo, layout.IntegrationBranch, layout.IntegrationDir, base); err != nil {
		t.Fatal(err)
	}
	// two card worktrees
	cards := []string{
		filepath.Join(filepath.Dir(layout.IntegrationDir), "card-r2c0"),
		filepath.Join(filepath.Dir(layout.IntegrationDir), "card-r2c1"),
	}
	branches := []string{layout.CardBranchPrefix + "/r2c0", layout.CardBranchPrefix + "/r2c1"}
	for i, c := range cards {
		if err := m.CardWorktree(repo, branches[i], c, layout.IntegrationBranch); err != nil {
			t.Fatal(err)
		}
	}
	// isolated edits: no shared checkout
	os.WriteFile(filepath.Join(cards[0], "a.txt"), []byte("from card0\n"), 0o644)
	os.WriteFile(filepath.Join(cards[1], "b.txt"), []byte("from card1\n"), 0o644)
	if _, err := os.Stat(filepath.Join(repo, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("card edit leaked into original checkout")
	}
	committed, err := m.CommitAll(cards[0], "Agent Conductor: B14 R02 C00")
	if err != nil || !committed {
		t.Fatalf("commit card0: committed=%v err=%v", committed, err)
	}
	committed, err = m.CommitAll(cards[1], "Agent Conductor: B14 R02 C01")
	if err != nil || !committed {
		t.Fatalf("commit card1: committed=%v err=%v", committed, err)
	}
	// merge both into integration conflict-free
	for _, b := range branches {
		if err := m.MergeBranch(layout.IntegrationDir, b); err != nil {
			t.Fatalf("merge %s: %v", b, err)
		}
	}
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(layout.IntegrationDir, f)); err != nil {
			t.Fatalf("integration missing %s", f)
		}
	}
	// original branch untouched
	if _, err := os.Stat(filepath.Join(repo, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("original checkout mutated before explicit apply")
	}
}

func TestConflictDetection(t *testing.T) {
	repo := initRepo(t)
	m := wt(t)
	base, _ := m.Commit(repo)
	layout := m.Layout(15, "b15", repo)
	if err := m.CreateIntegration(repo, layout.IntegrationBranch, layout.IntegrationDir, base); err != nil {
		t.Fatal(err)
	}
	c0 := filepath.Join(filepath.Dir(layout.IntegrationDir), "card0")
	c1 := filepath.Join(filepath.Dir(layout.IntegrationDir), "card1")
	b0 := layout.CardBranchPrefix + "/r1c0"
	b1 := layout.CardBranchPrefix + "/r1c1"
	m.CardWorktree(repo, b0, c0, layout.IntegrationBranch)
	m.CardWorktree(repo, b1, c1, layout.IntegrationBranch)
	// same file, conflicting content
	os.WriteFile(filepath.Join(c0, "base.txt"), []byte("card0 version\n"), 0o644)
	os.WriteFile(filepath.Join(c1, "base.txt"), []byte("card1 version\n"), 0o644)
	m.CommitAll(c0, "c0")
	m.CommitAll(c1, "c1")
	if err := m.MergeBranch(layout.IntegrationDir, b0); err != nil {
		t.Fatalf("first merge should succeed: %v", err)
	}
	err := m.MergeBranch(layout.IntegrationDir, b1)
	mc, ok := err.(*MergeConflictError)
	if !ok {
		t.Fatalf("expected conflict error, got %v", err)
	}
	if len(mc.Files) == 0 || mc.Files[0] != "base.txt" {
		t.Fatalf("conflict files wrong: %v", mc.Files)
	}
	// worktrees preserved
	for _, d := range []string{c0, c1, layout.IntegrationDir} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Fatalf("worktree %s must be preserved after conflict", d)
		}
	}
}

func TestDiffStat(t *testing.T) {
	repo := initRepo(t)
	m := wt(t)
	base, _ := m.Commit(repo)
	layout := m.Layout(16, "b16", repo)
	m.CreateIntegration(repo, layout.IntegrationBranch, layout.IntegrationDir, base)
	os.WriteFile(filepath.Join(layout.IntegrationDir, "new.txt"), []byte("x"), 0o644)
	m.CommitAll(layout.IntegrationDir, "x")
	summary, files, err := m.DiffStat(layout.IntegrationDir, base)
	if err != nil {
		t.Fatal(err)
	}
	if files != 1 || !strings.Contains(summary, "new.txt") && !strings.Contains(summary, "1 file") {
		t.Fatalf("diff stat wrong: %q %d", summary, files)
	}
}

func TestApplyGuards(t *testing.T) {
	repo := initRepo(t)
	m := wt(t)
	base, _ := m.Commit(repo)
	layout := m.Layout(17, "b17", repo)
	m.CreateIntegration(repo, layout.IntegrationBranch, layout.IntegrationDir, base)
	os.WriteFile(filepath.Join(layout.IntegrationDir, "applied.txt"), []byte("y"), 0o644)
	m.CommitAll(layout.IntegrationDir, "y")
	o := ApplyOptions{Repo: repo, OriginalBranch: "main", OriginalCommit: base, IntegrationBranch: layout.IntegrationBranch}
	if err := m.ApplyCheck(o); err != nil {
		t.Fatalf("apply check: %v", err)
	}
	if _, err := m.Apply(o); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "applied.txt")); err != nil {
		t.Fatal("apply did not merge file")
	}
	// moved base commit must be rejected
	os.WriteFile(filepath.Join(repo, "extra.txt"), []byte("z"), 0o644)
	exec.Command("git", "-C", repo, "add", "-A").Run()
	exec.Command("git", "-C", repo, "-c", "user.name=T", "-c", "user.email=t@t", "commit", "-m", "moved").Run()
	if err := m.ApplyCheck(o); err == nil {
		t.Fatal("apply must be blocked when original branch moved")
	}
}

func TestCleanup(t *testing.T) {
	repo := initRepo(t)
	m := wt(t)
	base, _ := m.Commit(repo)
	layout := m.Layout(18, "b18", repo)
	m.CreateIntegration(repo, layout.IntegrationBranch, layout.IntegrationDir, base)
	c0 := filepath.Join(filepath.Dir(layout.IntegrationDir), "card0")
	b0 := layout.CardBranchPrefix + "/r1c0"
	m.CardWorktree(repo, b0, c0, layout.IntegrationBranch)
	if err := m.Cleanup(repo, []string{c0, layout.IntegrationDir}, []string{b0, layout.IntegrationBranch}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c0); !os.IsNotExist(err) {
		t.Fatal("card worktree not removed")
	}
}
