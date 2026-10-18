package scheduler

import (
	"sync"
	"testing"
	"time"

	"agent-conductor/internal/model"
	"agent-conductor/internal/providers"
)

func sequenceSpec(cwd, key string) BatchSpecInput {
	return BatchSpecInput{ImportKey: key, Title: "Folder sequence", Cwd: cwd, Rows: []RowSpecInput{
		{Mode: "LINEAR", Runs: []RunSpecInput{{Title: "001-build.md", Prompt: "first task\r\n", Provider: model.ProviderCodex}}},
		{Mode: "LINEAR", Runs: []RunSpecInput{{Title: "002-test.md", Prompt: "second task", Provider: model.ProviderCodex}}},
	}}
}

func TestPromptSequenceImportIsIdempotentAcrossRestart(t *testing.T) {
	h := newHarness(t)
	mustPause(t, h, PauseRequest{Source: PauseSourceUser})
	spec := sequenceSpec(t.TempDir(), "preview-123")
	id, err := h.conductor.ApplyBatch(spec)
	if err != nil {
		t.Fatal(err)
	}
	h.restart()
	again, err := h.conductor.ApplyBatch(spec)
	if err != nil || again != id {
		t.Fatalf("retry = %s, %v; want %s", again, err, id)
	}
	batches, _ := h.store.ListBatches(100)
	runs, _ := h.store.ListRunsByBatch(id)
	if len(batches) != 1 || len(runs) != 2 {
		t.Fatalf("duplicate import: %d batches, %d runs", len(batches), len(runs))
	}
	for i, run := range runs {
		prompt, err := h.vault.Open(run.PromptCipherStore)
		if err != nil || prompt != spec.Rows[i].Runs[0].Prompt {
			t.Fatalf("prompt changed: %q, %v", prompt, err)
		}
		if run.Cwd != spec.Cwd || run.Title != spec.Rows[i].Runs[0].Title {
			t.Fatalf("wrong title or workspace: %+v", run)
		}
	}
}

func TestConcurrentPromptSequenceRetriesCreateOneBatch(t *testing.T) {
	h := newHarness(t)
	mustPause(t, h, PauseRequest{Source: PauseSourceUser})
	spec := sequenceSpec(t.TempDir(), "concurrent-preview")
	var wg sync.WaitGroup
	ids := make([]string, 8)
	errs := make([]error, 8)
	for i := range ids {
		wg.Add(1)
		go func(i int) { defer wg.Done(); ids[i], errs[i] = h.conductor.ApplyBatch(spec) }(i)
	}
	wg.Wait()
	for i := range ids {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("concurrent import %d = %s, %v", i, ids[i], errs[i])
		}
	}
	batches, _ := h.store.ListBatches(100)
	if len(batches) != 1 {
		t.Fatalf("created %d batches", len(batches))
	}
}

func TestPromptSequenceFailureHoldsLaterStepsUntilSkipped(t *testing.T) {
	h := newHarness(t)
	h.fakeCodex.SetScripts([]providers.Script{{Match: "first task", Duration: 20 * time.Millisecond, Blocked: true}})
	id, err := h.conductor.ApplyBatch(sequenceSpec(t.TempDir(), "blocked-preview"))
	if err != nil {
		t.Fatal(err)
	}
	runs, _ := h.store.ListRunsByBatch(id)
	if !h.waitState(runs[0].ID, 3*time.Second, model.StateBlocked) {
		t.Fatal("first step did not block")
	}
	if !h.tickUntil(time.Second, func() bool { b, _ := h.store.GetBatch(id); return b.Status == model.BatchAttention }) {
		t.Fatal("sequence did not need attention")
	}
	if r := h.run(runs[1].ID); r.ProviderSessionID != "" || r.State != model.StateQueued {
		t.Fatalf("dependent step started: %s", r.State)
	}
	if err := h.conductor.SkipRun(runs[0].ID); err != nil {
		t.Fatal(err)
	}
	if !h.waitState(runs[1].ID, 3*time.Second, model.StateCompleted) {
		t.Fatal("sequence did not advance after skip")
	}
}

func TestPromptSequenceInvalidLastStepCreatesNothing(t *testing.T) {
	h := newHarness(t)
	spec := sequenceSpec(t.TempDir(), "invalid-preview")
	spec.Rows[1].Runs[0].Prompt = " "
	if _, err := h.conductor.ApplyBatch(spec); err == nil {
		t.Fatal("accepted empty prompt")
	}
	batches, _ := h.store.ListBatches(100)
	if len(batches) != 0 {
		t.Fatal("partially imported invalid sequence")
	}
	spec.Rows = nil
	if _, err := h.conductor.ApplyBatch(spec); err == nil {
		t.Fatal("accepted empty sequence")
	}
}
