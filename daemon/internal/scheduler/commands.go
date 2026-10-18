package scheduler

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"sort"
	"strings"
	"time"

	"agent-conductor/internal/db"
	"agent-conductor/internal/model"
	"agent-conductor/internal/providers"
)

func quotaRowFromWindow(provider string, w providers.QuotaWindow) db.QuotaWindowRow {
	res := ""
	if w.HasReset {
		res = w.ResetsAt.UTC().Format(time.RFC3339)
	}
	return db.QuotaWindowRow{
		Provider: provider, WindowID: w.ID, Name: w.Name,
		UsedPercent: w.UsedPercent, ResetsAt: res, Exhausted: w.Exhausted,
		Source: w.Source, Confidence: w.Confidence,
	}
}

// EnqueueRunInput describes a single queued run (implicit batch).
type EnqueueRunInput struct {
	Prompt        string `json:"prompt"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Effort        string `json:"effort"`
	Permission    string `json:"permission"`
	AttachSecrets bool   `json:"attachSecrets"`
	Staged        bool   `json:"staged"`
	Cwd           string `json:"cwd"`
	RunNext       bool   `json:"runNext"`
	Title         string `json:"title"`
}

// EnqueueRun creates an implicit single-run batch and queues it.
func (c *Conductor) EnqueueRun(in EnqueueRunInput) (batchID, runID string, err error) {
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	if strings.TrimSpace(in.Prompt) == "" {
		return "", "", fmt.Errorf("prompt is required")
	}
	if in.Provider == "" {
		return "", "", fmt.Errorf("provider is required")
	}
	if _, ok := c.registry.Get(in.Provider); !ok {
		return "", "", fmt.Errorf("unknown provider %q", in.Provider)
	}
	if in.Permission == "" {
		in.Permission = "bypass"
	}
	cipher, err := c.vault.Seal(in.Prompt)
	if err != nil {
		return "", "", err
	}
	batch := &model.Batch{
		ID: newID("b"), Title: model.PreviewRedacted(in.Prompt, 60), Status: model.BatchQueued,
		BaseCwd: in.Cwd, CreatedAt: c.Now(), Implicit: true,
	}
	row := &model.Row{ID: newID("row"), BatchID: batch.ID, Position: 0, Mode: model.RowLinear, Status: model.RowPending}
	run := &model.Run{
		ID: newID("run"), BatchID: batch.ID, RowID: row.ID,
		Title:    model.PreviewRedacted(in.Prompt, 60),
		Provider: in.Provider, Model: in.Model, Effort: in.Effort, Permission: in.Permission,
		AttachSecrets: in.AttachSecrets, Staged: in.Staged,
		State: model.StateQueued, Stage: stageFor(in.Staged),
		PromptCipherStore: cipher, PromptHash: model.HashPrompt(in.Prompt),
		PromptPreview: model.PreviewRedacted(in.Prompt, 120),
		CreatedAt:     c.Now(), QueuedAt: c.Now(), Cwd: in.Cwd,
	}
	// Run Next means "ahead of everything still waiting". It used to assign
	// time.Now().UnixNano(), which dwarfs the small sequential keys and
	// therefore sent the run to the BACK of the queue.
	var sortKey int64
	if in.RunNext {
		sortKey, err = c.store.FrontSortKey()
	} else {
		sortKey, err = c.store.NextSortKey()
	}
	if err != nil {
		return "", "", err
	}
	run.SortKey = sortKey
	if err := c.store.CreateBatch(batch); err != nil {
		return "", "", err
	}
	if err := c.store.CreateRow(row); err != nil {
		return "", "", err
	}
	if err := c.store.CreateRun(run); err != nil {
		return "", "", err
	}
	if block, id := c.latestSecretBlock(); in.AttachSecrets && block != "" {
		run.SecretVersionID = fmt.Sprintf("%d", id)
		c.store.UpdateRun(run)
	}
	c.store.AddRunEvent(run.ID, model.EventQueued, quoteJSON("enqueued"))
	c.emitBatch(batch.ID)
	c.emitRun(run.ID)
	c.Poke()
	return batch.ID, run.ID, nil
}

func stageFor(staged bool) model.Stage {
	if staged {
		return model.StagePlan
	}
	return model.StageNone
}

// BatchSpecInput defines an explicit batch from the planner.
type BatchSpecInput struct {
	ImportKey string         `json:"importKey,omitempty"`
	Title     string         `json:"title"`
	Cwd       string         `json:"cwd"`
	Rows      []RowSpecInput `json:"rows"`
}

// RowSpecInput is one row; Parallel rows contain multiple runs.
type RowSpecInput struct {
	Mode string         `json:"mode"` // LINEAR | PARALLEL
	Runs []RunSpecInput `json:"runs"`
}

// RunSpecInput is one card.
type RunSpecInput struct {
	Prompt        string `json:"prompt"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Effort        string `json:"effort"`
	Permission    string `json:"permission"`
	AttachSecrets bool   `json:"attachSecrets"`
	Staged        bool   `json:"staged"`
	Title         string `json:"title"`
}

// PrepareBatch validates a batch spec (worktree prechecks for parallel).
func (c *Conductor) PrepareBatch(spec BatchSpecInput) error {
	if len(spec.Rows) == 0 {
		return fmt.Errorf("a batch needs at least one row")
	}
	if spec.ImportKey != "" {
		if len(spec.Rows) > 500 {
			return fmt.Errorf("a prompt sequence supports at most 500 steps")
		}
		for _, row := range spec.Rows {
			if row.Mode != "LINEAR" {
				return fmt.Errorf("prompt sequence steps must be LINEAR")
			}
		}
	}
	hasParallel := false
	for _, r := range spec.Rows {
		if strings.ToUpper(r.Mode) == "PARALLEL" {
			if len(r.Runs) < 2 {
				return fmt.Errorf("a PARALLEL row needs at least two runs")
			}
			hasParallel = true
		} else if len(r.Runs) != 1 {
			return fmt.Errorf("a LINEAR row must contain exactly one run")
		}
		for _, run := range r.Runs {
			if strings.TrimSpace(run.Prompt) == "" {
				return fmt.Errorf("every run needs a prompt")
			}
			if _, ok := c.registry.Get(run.Provider); !ok {
				return fmt.Errorf("unknown provider %q", run.Provider)
			}
		}
	}
	if hasParallel {
		if err := c.wt.Precheck(spec.Cwd); err != nil {
			return err
		}
	}
	return nil
}

// ApplyBatch persists a batch spec and queues it.
func (c *Conductor) ApplyBatch(spec BatchSpecInput) (batchID string, err error) {
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	// A preview carries a stable key. Retrying after a lost response or daemon
	// restart must return the original sequence, never enqueue it twice.
	id := newID("b")
	if spec.ImportKey != "" {
		if len(spec.ImportKey) > 128 {
			return "", fmt.Errorf("import key is too long")
		}
		id = "b_import_" + model.HashPrompt(spec.ImportKey)
		if existing, err := c.store.GetBatch(id); err != nil {
			return "", err
		} else if existing != nil {
			return id, nil
		}
	}
	if err := c.PrepareBatch(spec); err != nil {
		return "", err
	}
	title := spec.Title
	if title == "" {
		title = "Batch"
	}
	branch, _ := c.wt.Branch(spec.Cwd)
	commit, _ := c.wt.Commit(spec.Cwd)
	batch := &model.Batch{
		ID: id, Title: title, Status: model.BatchQueued,
		BaseCwd: spec.Cwd, BaseBranch: branch, BaseCommit: commit,
		CreatedAt: c.Now(),
	}
	sortKey, err := c.store.NextSortKey()
	if err != nil {
		return "", err
	}
	var rows []*model.Row
	var runs []*model.Run
	for i, rs := range spec.Rows {
		if len(rs.Runs) == 0 {
			// A row with no runs can never satisfy rowDone/rowAllStarted, so it
			// would stall the batch forever.
			return "", fmt.Errorf("row %d has no runs", i+1)
		}
		mode := model.RowLinear
		if strings.ToUpper(rs.Mode) == "PARALLEL" {
			mode = model.RowParallel
		}
		row := &model.Row{ID: newID("row"), BatchID: batch.ID, Position: i, Mode: mode, Status: model.RowPending}
		rows = append(rows, row)
		for ci, rr := range rs.Runs {
			cipher, err := c.vault.Seal(rr.Prompt)
			if err != nil {
				return "", err
			}
			permission := rr.Permission
			if permission == "" {
				permission = "bypass"
			}
			runs = append(runs, &model.Run{
				ID: newID("run"), BatchID: batch.ID, RowID: row.ID, CardIndex: ci,
				Title: rr.Title, Provider: rr.Provider, Model: rr.Model, Effort: rr.Effort,
				Permission: permission, AttachSecrets: rr.AttachSecrets, Staged: rr.Staged,
				State: model.StateQueued, Stage: stageFor(rr.Staged),
				PromptCipherStore: cipher, PromptHash: model.HashPrompt(rr.Prompt),
				PromptPreview: model.PreviewRedacted(rr.Prompt, 120),
				CreatedAt:     c.Now(), QueuedAt: c.Now(), SortKey: sortKey, Cwd: spec.Cwd,
			})
			sortKey += db.SortKeyGap
		}
	}
	// One transaction: a failure part-way used to leave a half-created batch.
	if err := c.store.CreateBatchAtomic(batch, rows, runs, func(string) (model.RunEventType, string) {
		return model.EventQueued, quoteJSON("batch applied")
	}); err != nil {
		// Concurrent retries may both observe a missing batch. The primary key
		// and atomic transaction let only one create it.
		if spec.ImportKey != "" {
			if existing, lookupErr := c.store.GetBatch(id); lookupErr == nil && existing != nil {
				return id, nil
			}
		}
		return "", err
	}
	c.emitBatch(batch.ID)
	c.Poke()
	return batch.ID, nil
}

// CancelRun requests provider-aware cancellation.
func (c *Conductor) CancelRun(runID string) error {
	r, err := c.store.GetRun(runID)
	if err != nil || r == nil {
		return fmt.Errorf("run not found")
	}
	if r.State.Terminal() {
		return fmt.Errorf("run already terminal: %s", r.State)
	}
	c.store.AddRunEvent(runID, model.EventCancelRequested, quoteJSON("user"))
	c.mu.Lock()
	cancel := c.cancels[runID]
	c.mu.Unlock()

	// A run that was never dispatched has no provider turn in flight, so
	// nothing would ever emit TurnInterrupted to move it out of
	// CANCEL_REQUESTED — it sat there forever holding a slot, and two cancels
	// with the default MaxTotal of 2 killed the queue until a daemon restart.
	if cancel == nil && (!dispatched(r.State) || r.State == model.StatePaused) {
		// A process kept between turns (or opened in Claude Code) must not
		// outlive the run it serves.
		if adapter, ok := c.registry.Get(r.Provider); ok {
			_ = adapter.Cancel(runID)
		}
		if err := c.store.TransitionRun(runID, model.StateCancelled, nil, func(x *model.Run) {
			x.FinalOutcome = "CANCELLED"
			x.FinishedAt = c.Now()
		}); err != nil {
			return err
		}
		c.reviveBatch(r.BatchID)
		c.emitRun(runID)
		c.Poke()
		return nil
	}

	// Persist the intent before the adapter can acknowledge it.
	if err := c.store.TransitionRun(runID, model.StateCancelRequested, nil, func(x *model.Run) {
		x.CancelRequestedAt = c.Now()
	}); err != nil {
		return err
	}
	if adapter, ok := c.registry.Get(r.Provider); ok {
		adapter.Cancel(runID)
	}
	if cancel != nil {
		cancel()
	}
	if cancel == nil && r.State == model.StateVerifying {
		c.transition(runID, model.StateCancelled, nil, func(x *model.Run) { x.FinishedAt = c.Now() })
	}
	c.emitRun(runID)
	c.Poke()
	return nil
}

// dispatched reports whether a run has an execution in flight with the
// provider. Waiting states have not reached an adapter yet.
func dispatched(st model.RunState) bool {
	switch st {
	case model.StateQueued, model.StateWaitingForSlot, model.StateWaitingForExternal,
		model.StateWaitingForQuota, model.StateDraft:
		return false
	}
	return true
}

// RetryRun requeues a failed/attention run from its original prompt.
func (c *Conductor) RetryRun(runID string) error {
	r, err := c.store.GetRun(runID)
	if err != nil || r == nil {
		return fmt.Errorf("run not found")
	}
	if r.State.Active() {
		return fmt.Errorf("run is already active")
	}
	if !c.transition(runID, model.StateQueued, &model.RunEvent{Type: model.EventQueued, Timestamp: c.Now()}, func(x *model.Run) {
		x.Stage = stageFor(r.Staged)
		x.ErrorCode, x.ErrorMsg = "", ""
		x.ProviderSessionID = "" // fresh native session on retry
		x.ResolvedModel, x.PendingPromptCipher = "", ""
		x.RetryCount, x.CompletionRetries, x.VerifyCycles, x.AuditCycles = 0, 0, 0, 0
		x.NextEligibleAt = nil
		x.FinishedAt = time.Time{}
		x.FinalOutcome = ""
		x.MeaningfulProgress = false
	}) {
		return fmt.Errorf("cannot retry run in %s", r.State)
	}
	c.store.UpdateRowStatus(r.RowID, model.RowRunning)
	c.reviveBatch(r.BatchID)
	c.emitRun(runID)
	c.Poke()
	return nil
}

// reviveBatch returns an ATTENTION batch to RUNNING so the scheduler can
// advance it after a retry/skip decision.
func (c *Conductor) reviveBatch(batchID string) {
	b, err := c.store.GetBatch(batchID)
	if err != nil || b == nil {
		return
	}
	if b.Status == model.BatchAttention || b.Status == model.BatchDone {
		b.Status = model.BatchRunning
		c.store.UpdateBatch(b)
		c.emitBatch(b.ID)
	}
}

// ContinueRun resumes the same native session with a user/interrupt-driven
// continuation (also used for NEEDS_ATTENTION recovery).
func (c *Conductor) ContinueRun(runID string, message string) error {
	r, err := c.store.GetRun(runID)
	if err != nil || r == nil {
		return fmt.Errorf("run not found")
	}
	if r.ProviderSessionID == "" {
		return c.RetryRun(runID)
	}
	if r.State.Active() {
		return fmt.Errorf("run is already active")
	}
	if message == "" {
		message = model.ContinueAfterQuota
	}
	return c.queueContinuation(runID, message, "continuation requested", nil)
}

// SkipRun marks a run skipped (explicit user action only).
func (c *Conductor) SkipRun(runID string) error {
	r, err := c.store.GetRun(runID)
	if err != nil || r == nil {
		return fmt.Errorf("run not found")
	}
	if err := c.store.TransitionRun(runID, model.StateSkipped, nil, func(x *model.Run) {
		x.FinishedAt = c.Now()
	}); err != nil {
		return err
	}
	c.reviveBatch(r.BatchID)
	c.emitRun(runID)
	c.Poke()
	return nil
}

// UpdateRunInput is a true patch: an omitted field is left alone. The previous
// shape reused EnqueueRunInput and unconditionally assigned Model/Effort/
// Permission, so a partial update that omitted `model` blanked it.
type UpdateRunInput struct {
	Prompt        *string `json:"prompt"`
	Provider      *string `json:"provider"`
	Model         *string `json:"model"`
	Effort        *string `json:"effort"`
	Permission    *string `json:"permission"`
	AttachSecrets *bool   `json:"attachSecrets"`
	Staged        *bool   `json:"staged"`
	Title         *string `json:"title"`
}

// UpdateRun edits a queued (not yet started) run's scheduling fields.
// Prompt edits are rejected after dispatch: original prompts are immutable.
func (c *Conductor) UpdateRun(runID string, in UpdateRunInput) error {
	r, err := c.store.GetRun(runID)
	if err != nil || r == nil {
		return fmt.Errorf("run not found")
	}
	if r.State != model.StateQueued && r.State != model.StateWaitingForSlot && r.State != model.StateWaitingForExternal {
		return fmt.Errorf("only queued, not-yet-started runs can be edited (state %s); use Continue or Cancel+Retry", r.State)
	}
	if in.Prompt != nil && strings.TrimSpace(*in.Prompt) != "" {
		cipher, err := c.vault.Seal(*in.Prompt)
		if err != nil {
			return err
		}
		r.PromptCipherStore = cipher
		r.PromptHash = model.HashPrompt(*in.Prompt)
		r.PromptPreview = model.PreviewRedacted(*in.Prompt, 120)
	}
	if in.Provider != nil && *in.Provider != "" {
		if _, ok := c.registry.Get(*in.Provider); !ok {
			return fmt.Errorf("unknown provider %q", *in.Provider)
		}
		r.Provider = *in.Provider
	}
	if in.Model != nil {
		r.Model = *in.Model
	}
	if in.Effort != nil {
		r.Effort = *in.Effort
	}
	if in.Permission != nil && *in.Permission != "" {
		r.Permission = *in.Permission
	}
	if in.Title != nil {
		r.Title = *in.Title
	}
	if in.AttachSecrets != nil {
		r.AttachSecrets = *in.AttachSecrets
	}
	if in.Staged != nil {
		r.Staged = *in.Staged
	}
	if r.Staged && r.Stage == model.StageNone {
		r.Stage = model.StagePlan
	}
	if !r.Staged {
		r.Stage = model.StageNone
	}
	return c.store.UpdateRun(r)
}

// ReorderQueue moves a queued run within FIFO bounds (never ahead of its
// batch row dependencies).
func (c *Conductor) ReorderQueue(runID string, beforeRunID string) error {
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	if err := c.store.ReorderQueue(runID, beforeRunID); err != nil {
		return err
	}
	// Reordering can renumber every waiting run. All connected windows must
	// refresh the complete order, including clients other than the editor.
	c.emit("queueReordered", map[string]string{"runId": runID})
	c.Poke()
	return nil
}

// PauseScheduler / ResumeScheduler are thin aliases kept so an older
// extension build keeps working against a newer daemon. New callers use
// PauseQueue/ResumeQueue, which carry scope, timer and reason.
func (c *Conductor) PauseScheduler() error {
	_, err := c.PauseQueue(PauseRequest{Scope: PauseQueueOnly, Source: PauseSourceUser})
	return err
}

func (c *Conductor) ResumeScheduler() error {
	_, err := c.ResumeQueue()
	return err
}

// UpdateSecretBlock stores a new encrypted version of the shared testing &
// sudo block. Latest-at-dispatch semantics: queued runs pick up this version
// when they start.
func (c *Conductor) UpdateSecretBlock(text string) (int64, error) {
	if strings.TrimSpace(text) == "" {
		return 0, fmt.Errorf("secret block text empty; use DeleteSecretBlock")
	}
	c.log.Redactor().Add(text)
	cipher, err := c.vault.Seal(text)
	if err != nil {
		return 0, err
	}
	id, err := c.store.AddSecretVersion(cipher, model.HashPrompt(text), "")
	if err != nil {
		return 0, err
	}
	c.emit("settingsUpdated", nil)
	return id, nil
}

// RevealSecretBlock decrypts the latest block for explicit user reveal.
func (c *Conductor) RevealSecretBlock() (string, int64, error) {
	v, err := c.store.LatestSecretVersion()
	if err != nil {
		return "", 0, err
	}
	if v == nil {
		return "", 0, fmt.Errorf("no secret block stored")
	}
	plain, err := c.vault.Open(v.Cipher())
	return plain, v.ID, err
}

// DeleteSecretBlock permanently removes the stored block.
//
// This used to set an unread `secretBlock.deleted` flag: the plaintext stayed
// in the database, RevealSecretBlock still revealed it, and queued runs still
// shipped it to the provider. Pressing Delete on a sudo password did nothing.
func (c *Conductor) DeleteSecretBlock() error {
	n, err := c.store.DeleteAllSecretVersions()
	if err != nil {
		return err
	}
	// Stop redacting a value that no longer exists, and clear the stale flag
	// from installations that ran the old implementation.
	c.log.Redactor().Clear()
	_ = c.store.SetSetting("secretBlock.deleted", false)
	c.log.Infof("secret block deleted (%d version(s) removed)", n)
	c.emit("settingsUpdated", nil)
	return nil
}

// RunAttachesSecrets reports whether a run's effective prompt carries the
// shared block. Previously the API substring-matched the rendered prompt for
// a literal header, which is both fragile and a weaker guarantee.
func (c *Conductor) RunAttachesSecrets(runID string) bool {
	r, err := c.store.GetRun(runID)
	if err != nil || r == nil || !r.AttachSecrets {
		return false
	}
	block, _ := c.latestSecretBlock()
	return block != ""
}

// HasSecretBlock reports existence without decrypting anything.
func (c *Conductor) HasSecretBlock() bool {
	ok, err := c.store.HasSecretBlock()
	return err == nil && ok
}

// ClearStaleExternalSession removes one external busy observation.
func (c *Conductor) ClearStaleExternalSession(provider, nativeID string) error {
	if err := c.store.DeleteProviderSession(provider, nativeID); err != nil {
		return err
	}
	c.emit("externalSessionUpdated", provider)
	c.Poke()
	return nil
}

// ApplyBatchToBranch performs the guarded final merge (explicit user action).
func (c *Conductor) ApplyBatchToBranch(batchID string) (string, error) {
	b, err := c.store.GetBatch(batchID)
	if err != nil || b == nil {
		return "", fmt.Errorf("batch not found")
	}
	if b.IntegrationBranch == "" {
		return "", fmt.Errorf("batch has no integration branch")
	}
	out, err := c.wt.Apply(worktreesApplyOptions(b))
	if err != nil {
		return "", err
	}
	b.Applied = true
	c.store.UpdateBatch(b)
	c.emitBatch(b.ID)
	return out, nil
}

// CleanupWorktrees removes a completed batch's worktrees and branches.
func (c *Conductor) CleanupWorktrees(batchID string) error {
	wts, err := c.store.ListWorktrees(batchID)
	if err != nil {
		return err
	}
	b, _ := c.store.GetBatch(batchID)
	var dirs, branches []string
	for _, w := range wts {
		dirs = append(dirs, w.Path)
		branches = append(branches, w.Branch)
	}
	if b != nil && b.BaseCwd != "" {
		if err := c.wt.Cleanup(b.BaseCwd, dirs, branches); err != nil {
			return err
		}
	}
	for _, w := range wts {
		c.store.UpdateWorktreeStatus(w.Path, "CLEANED")
	}
	c.emit("worktreeUpdated", batchID)
	return nil
}

// CopyBatchReport builds the deterministic markdown report.
func (c *Conductor) CopyBatchReport(batchID string) (string, error) {
	b, err := c.store.GetBatch(batchID)
	if err != nil || b == nil {
		return "", fmt.Errorf("batch not found")
	}
	rows, _ := c.store.ListRows(batchID)
	runs, _ := c.store.ListRunsByBatch(batchID)
	byRow := map[string][]*model.Run{}
	for _, r := range runs {
		byRow[r.RowID] = append(byRow[r.RowID], r)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Batch: %s\n\n", b.Title)
	completed := 0
	for _, r := range runs {
		if r.State == model.StateCompleted {
			completed++
		}
	}
	fmt.Fprintf(&sb, "Status: %d/%d complete\n\n", completed, len(runs))
	n := 1
	for _, row := range rows {
		for _, r := range byRow[row.ID] {
			fmt.Fprintf(&sb, "## Run %d — %s\n", n, titleCase(r.Provider))
			fmt.Fprintf(&sb, "- Model: %s (effort %s)\n", r.Model, r.Effort)
			fmt.Fprintf(&sb, "- Outcome: %s\n", outcomeLabel(r))
			if !r.StartedAt.IsZero() && !r.FinishedAt.IsZero() {
				fmt.Fprintf(&sb, "- Duration: %s (active %s, quota wait %s)\n",
					r.FinishedAt.Sub(r.StartedAt).Round(time.Second),
					time.Duration(r.ActiveNS).Round(time.Second),
					time.Duration(r.QuotaWaitNS).Round(time.Second))
			}
			if r.ContinuationCount > 0 {
				fmt.Fprintf(&sb, "- Continuations: %d\n", r.ContinuationCount)
			}
			if r.ProviderSessionID != "" {
				fmt.Fprintf(&sb, "- Provider session: %s\n", r.ProviderSessionID)
			}
			if r.Branch != "" {
				fmt.Fprintf(&sb, "- Branch: %s\n", r.Branch)
			}
			sb.WriteString("\n")
			n++
		}
	}
	fmt.Fprintf(&sb, "## Changes\n\n- Base: %s @ %s\n- Integration branch: %s\n", b.BaseBranch, b.BaseCommit[:min(8, len(b.BaseCommit))], b.IntegrationBranch)
	return sb.String(), nil
}

// titleCase upper-cases the first letter. r.Provider[:1] panicked on an empty
// provider string.
func titleCase(s string) string {
	if s == "" {
		return "Unknown"
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func outcomeLabel(r *model.Run) string {
	if r.FinalOutcome != "" {
		return r.FinalOutcome
	}
	return string(r.State)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// EffectivePromptForRun builds the current effective prompt for explicit user
// reveal (contains secrets when attached — UI must warn).
func (c *Conductor) EffectivePromptForRun(runID string) (string, error) {
	r, err := c.store.GetRun(runID)
	if err != nil || r == nil {
		return "", fmt.Errorf("run not found")
	}
	userPrompt, err := c.decryptPrompt(r)
	if err != nil {
		return "", err
	}
	var block string
	if r.AttachSecrets {
		block, _ = c.latestSecretBlock()
	}
	return model.EffectivePrompt(userPrompt, r.AttachSecrets, block, r.Staged && r.Stage == model.StagePlan), nil
}

var idEncoding = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

// newID returns a collision-free identifier.
//
// The previous implementation concatenated two clock readings, so two calls in
// the same tick produced identical ids — and ApplyBatch creates rows and runs
// in a tight loop, violating the PRIMARY KEY and aborting batch creation
// half-way. The time prefix is kept only so ids sort roughly by creation.
func newID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; if it ever does, a
		// time-based id is still better than a duplicate.
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return fmt.Sprintf("%s_%s", prefix, idEncoding.EncodeToString(b[:]))
}

// ---- usage & diffs (§7.6, §7.7) ----

// RunUsage is one run's measured cost in time and continuations.
type RunUsage struct {
	RunID         string `json:"runId"`
	Title         string `json:"title"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	State         string `json:"state"`
	ActiveMs      int64  `json:"activeMs"`
	QuotaWaitMs   int64  `json:"quotaWaitMs"`
	Continuations int    `json:"continuations"`
	FinishedAt    string `json:"finishedAt,omitempty"`
}

// UsageSummary aggregates what was actually measured.
//
// Deliberately no dollar figures: the daemon does not know token prices or
// which plan a run billed against, and inventing a number would be worse than
// showing none.
type UsageSummary struct {
	Since         string           `json:"since"`
	Runs          int              `json:"runs"`
	Completed     int              `json:"completed"`
	Failed        int              `json:"failed"`
	ActiveMs      int64            `json:"activeMs"`
	QuotaWaitMs   int64            `json:"quotaWaitMs"`
	Continuations int              `json:"continuations"`
	ByProvider    map[string]int64 `json:"byProviderActiveMs"`
	Recent        []RunUsage       `json:"recent"`
	ByDay         map[string]int64 `json:"byDayActiveMs"`
	Note          string           `json:"note"`
}

// UsageSince summarises runs created in the given window.
func (c *Conductor) UsageSince(window time.Duration) (*UsageSummary, error) {
	runs, err := c.store.ListRunsByStates(allStatesList()...)
	if err != nil {
		return nil, err
	}
	cutoff := c.Now().Add(-window)
	out := &UsageSummary{
		Since:      cutoff.UTC().Format(time.RFC3339),
		ByProvider: map[string]int64{},
		Recent:     []RunUsage{},
		ByDay:      map[string]int64{},
		Note:       "Measured wall-clock time only. Agent Conductor does not estimate spend.",
	}
	for _, r := range runs {
		if r.CreatedAt.Before(cutoff) {
			continue
		}
		activeMs := r.ActiveNS / int64(time.Millisecond)
		out.Runs++
		out.ActiveMs += activeMs
		out.QuotaWaitMs += r.QuotaWaitNS / int64(time.Millisecond)
		out.Continuations += r.ContinuationCount
		out.ByProvider[r.Provider] += activeMs
		out.ByDay[r.CreatedAt.UTC().Format("2006-01-02")] += activeMs
		switch r.State {
		case model.StateCompleted:
			out.Completed++
		case model.StateFailed, model.StateNeedsAttention, model.StateOrphaned:
			out.Failed++
		}
		u := RunUsage{
			RunID: r.ID, Title: r.Title, Provider: r.Provider, Model: r.Model,
			State: string(r.State), ActiveMs: activeMs,
			QuotaWaitMs: r.QuotaWaitNS / int64(time.Millisecond), Continuations: r.ContinuationCount,
		}
		if !r.FinishedAt.IsZero() {
			u.FinishedAt = r.FinishedAt.UTC().Format(time.RFC3339)
		}
		out.Recent = append(out.Recent, u)
	}
	sort.Slice(out.Recent, func(i, j int) bool { return out.Recent[i].ActiveMs > out.Recent[j].ActiveMs })
	if len(out.Recent) > 10 {
		out.Recent = out.Recent[:10]
	}
	return out, nil
}

// RunDiff describes what a run changed on disk.
type RunDiff struct {
	RunID   string `json:"runId"`
	Summary string `json:"summary"`
	Files   int    `json:"files"`
	Branch  string `json:"branch,omitempty"`
	Path    string `json:"path,omitempty"`
}

// RunDiffStat summarises a run's worktree against the batch's base.
//
// worktrees.DiffStat existed and was never called from anywhere.
func (c *Conductor) RunDiffStat(runID string) (*RunDiff, error) {
	r, err := c.store.GetRun(runID)
	if err != nil || r == nil {
		return nil, fmt.Errorf("run not found")
	}
	if r.WorktreePath == "" {
		return nil, fmt.Errorf("this run did not use an isolated worktree")
	}
	b, err := c.store.GetBatch(r.BatchID)
	if err != nil || b == nil {
		return nil, fmt.Errorf("batch not found")
	}
	base := b.BaseCommit
	if base == "" {
		base = b.BaseBranch
	}
	if base == "" {
		return nil, fmt.Errorf("the batch recorded no base to compare against")
	}
	summary, files, err := c.wt.DiffStat(r.WorktreePath, base)
	if err != nil {
		return nil, err
	}
	return &RunDiff{RunID: runID, Summary: summary, Files: files, Branch: r.Branch, Path: r.WorktreePath}, nil
}

// RequeueRun creates a NEW run from a past run's original prompt (§7.3).
//
// Distinct from RetryRun, which revives the same run in place: this leaves the
// original history intact, which is what "run that again" means for a run that
// already finished.
func (c *Conductor) RequeueRun(runID string, runNext bool) (string, string, error) {
	r, err := c.store.GetRun(runID)
	if err != nil || r == nil {
		return "", "", fmt.Errorf("run not found")
	}
	prompt, err := c.decryptPrompt(r)
	if err != nil {
		return "", "", fmt.Errorf("the original prompt is no longer readable: %w", err)
	}
	return c.EnqueueRun(EnqueueRunInput{
		Prompt:        prompt,
		Provider:      r.Provider,
		Model:         r.Model,
		Effort:        r.Effort,
		Permission:    r.Permission,
		AttachSecrets: r.AttachSecrets,
		Staged:        r.Staged,
		Cwd:           r.Cwd,
		RunNext:       runNext,
		Title:         r.Title,
	})
}
