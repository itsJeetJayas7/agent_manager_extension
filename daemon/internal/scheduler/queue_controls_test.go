package scheduler

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"agent-conductor/internal/logging"
	"agent-conductor/internal/model"
	"agent-conductor/internal/providers"
)

func TestReorderAllWaitingStatesPersistsAndAppends(t *testing.T) {
	for _, state := range queueWaitingStates() {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			mustPause(t, h, PauseRequest{Source: PauseSourceUser})
			var ids []string
			for _, prompt := range []string{"a", "b", "c"} {
				_, id, err := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: prompt, Provider: model.ProviderClaude, Cwd: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
			}
			r := h.run(ids[1])
			r.State = state
			if err := h.store.UpdateRun(r); err != nil {
				t.Fatal(err)
			}
			if err := h.conductor.ReorderQueue(ids[0], ids[1]); err != nil {
				t.Fatal(err)
			}
			if err := h.conductor.ReorderQueue(ids[1], ids[0]); err != nil {
				t.Fatal(err)
			}
			if h.run(ids[1]).State != state {
				t.Fatal("reorder changed run state")
			}
			h.restart()
			_, last, err := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "last", Provider: model.ProviderClaude, Cwd: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			runs, err := h.store.ListRunsByStates(queueWaitingStates()...)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, run := range runs {
				got = append(got, run.ID)
			}
			if !reflect.DeepEqual(got, []string{ids[1], ids[0], ids[2], last}) {
				t.Fatalf("wrong order after restart/append: %v", got)
			}
		})
	}
}

func TestReorderPreservesSequenceDependenciesAcrossOtherBatches(t *testing.T) {
	h := newHarness(t)
	mustPause(t, h, PauseRequest{Source: PauseSourceUser})
	_, other, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "other", Provider: model.ProviderClaude, Cwd: t.TempDir()})
	batch, err := h.conductor.ApplyBatch(sequenceSpec(t.TempDir(), "reorder"))
	if err != nil {
		t.Fatal(err)
	}
	runs, _ := h.store.ListRunsByBatch(batch)
	before := dispatchOrder(t, h)
	for _, move := range [][2]string{{runs[1].ID, runs[0].ID}, {runs[1].ID, other}, {runs[0].ID, ""}} {
		if err := h.conductor.ReorderQueue(move[0], move[1]); err == nil {
			t.Fatalf("accepted dependency inversion: %v", move)
		}
		if !reflect.DeepEqual(dispatchOrder(t, h), before) {
			t.Fatal("rejected move changed the queue")
		}
	}
	if err := h.conductor.ReorderQueue(other, ""); err != nil {
		t.Fatal(err)
	}
}

func TestTrustedFolderUpgradeRequeuesOnlyTrustFailures(t *testing.T) {
	h := newHarness(t)
	mustPause(t, h, PauseRequest{Source: PauseSourceUser})
	var ids []string
	for _, code := range []string{"WORKSPACE_UNTRUSTED", "AUTH"} {
		_, id, err := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: code, Provider: model.ProviderClaude, Cwd: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if err := h.store.TransitionRun(id, model.StateNeedsAttention, nil, func(r *model.Run) { r.ErrorCode = code }); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	key := h.run(ids[0]).SortKey
	h.restart()
	if r := h.run(ids[0]); r.State != model.StateQueued || r.ErrorCode != "" || r.SortKey != key {
		t.Fatalf("trust failure not recovered: %+v", r)
	}
	if h.run(ids[1]).State != model.StateNeedsAttention {
		t.Fatal("unrelated failure was retried")
	}
	if !h.conductor.PauseState().Paused {
		t.Fatal("upgrade removed user pause")
	}
}

func TestClaudeRunsInFolderWithoutTrustRecord(t *testing.T) {
	h := newHarness(t)
	mustPause(t, h, PauseRequest{Source: PauseSourceUser})
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
if [ "$1" = "--help" ]; then
  echo '--print --output-format stream-json'
  exit 0
fi
cat >/dev/null
echo '{"type":"system","subtype":"init","session_id":"trusted-test","model":"test"}'
echo '{"type":"result","subtype":"success","result":"<AC_RESULT>COMPLETE</AC_RESULT>","is_error":false}'
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AC_CLAUDE_BIN", bin)
	h.registry.Register(providers.NewClaude(logging.NewNop(), providers.NewHookBus(), "/bin/true", ""))
	_, id, err := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "trusted by queueing", Provider: model.ProviderClaude, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.conductor.ResumeQueue(); err != nil {
		t.Fatal(err)
	}
	if !h.waitState(id, 5*time.Second, model.StateCompleted) {
		t.Fatalf("run did not complete: %+v", h.run(id))
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !os.IsNotExist(err) {
		t.Fatal("headless run should not need to write Claude's global trust file")
	}
}
