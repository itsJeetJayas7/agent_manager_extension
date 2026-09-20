package scheduler

import (
	"fmt"
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
	sortKey, err := c.store.NextSortKey()
	if err != nil {
		return "", "", err
	}
	if in.RunNext {
		// insert immediately after active runs: lowest sort key among queued.
		sortKey = time.Now().UnixNano() // still after active; scheduler dispatches FIFO
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
	c.emit("batchUpdated", batch.ID)
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
	Title string         `json:"title"`
	Cwd   string         `json:"cwd"`
	Rows  []RowSpecInput `json:"rows"`
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
		ID: newID("b"), Title: title, Status: model.BatchQueued,
		BaseCwd: spec.Cwd, BaseBranch: branch, BaseCommit: commit,
		CreatedAt: c.Now(),
	}
	if err := c.store.CreateBatch(batch); err != nil {
		return "", err
	}
	sortKey, _ := c.store.NextSortKey()
	for i, rs := range spec.Rows {
		mode := model.RowLinear
		if strings.ToUpper(rs.Mode) == "PARALLEL" {
			mode = model.RowParallel
		}
		row := &model.Row{ID: newID("row"), BatchID: batch.ID, Position: i, Mode: mode, Status: model.RowPending}
		if err := c.store.CreateRow(row); err != nil {
			return "", err
		}
		for ci, rr := range rs.Runs {
			cipher, err := c.vault.Seal(rr.Prompt)
			if err != nil {
				return "", err
			}
			permission := rr.Permission
			if permission == "" {
				permission = "bypass"
			}
			run := &model.Run{
				ID: newID("run"), BatchID: batch.ID, RowID: row.ID, CardIndex: ci,
				Title: rr.Title, Provider: rr.Provider, Model: rr.Model, Effort: rr.Effort,
				Permission: permission, AttachSecrets: rr.AttachSecrets, Staged: rr.Staged,
				State: model.StateQueued, Stage: stageFor(rr.Staged),
				PromptCipherStore: cipher, PromptHash: model.HashPrompt(rr.Prompt),
				PromptPreview: model.PreviewRedacted(rr.Prompt, 120),
				CreatedAt:     c.Now(), QueuedAt: c.Now(), SortKey: sortKey, Cwd: spec.Cwd,
			}
			sortKey++
			if err := c.store.CreateRun(run); err != nil {
				return "", err
			}
			c.store.AddRunEvent(run.ID, model.EventQueued, quoteJSON("batch applied"))
		}
	}
	c.emit("batchUpdated", batch.ID)
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
	if adapter, ok := c.registry.Get(r.Provider); ok {
		adapter.Cancel(runID)
	}
	if cancel != nil {
		cancel()
	}
	c.store.TransitionRun(runID, model.StateCancelRequested, nil, nil)
	c.emitRun(runID)
	c.Poke()
	return nil
}

// RetryRun requeues a failed/attention run from its original prompt.
func (c *Conductor) RetryRun(runID string) error {
	r, err := c.store.GetRun(runID)
	if err != nil || r == nil {
		return fmt.Errorf("run not found")
	}
	c.store.TransitionRun(runID, model.StateQueued, &model.RunEvent{Type: model.EventQueued, Timestamp: c.Now()}, func(x *model.Run) {
		x.Stage = stageFor(r.Staged)
		x.ErrorCode, x.ErrorMsg = "", ""
		x.ProviderSessionID = "" // fresh native session on retry
		x.MeaningfulProgress = false
	})
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
	if b.Status == model.BatchAttention {
		b.Status = model.BatchRunning
		c.store.UpdateBatch(b)
		c.emit("batchUpdated", b.ID)
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
	msg := message
	if msg == "" {
		msg = model.ContinueAfterQuota
	}
	c.store.TransitionRun(runID, model.StateAutoResuming, &model.RunEvent{Type: model.EventAutoResumeStarted, Timestamp: c.Now()}, nil)
	c.launchExecution(r, msg, true)
	c.emitRun(runID)
	return nil
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

// UpdateRun edits a queued (not yet started) run's scheduling fields.
// Prompt edits are rejected after dispatch: original prompts are immutable.
func (c *Conductor) UpdateRun(runID string, in EnqueueRunInput) error {
	r, err := c.store.GetRun(runID)
	if err != nil || r == nil {
		return fmt.Errorf("run not found")
	}
	if r.State != model.StateQueued && r.State != model.StateWaitingForSlot && r.State != model.StateWaitingForExternal {
		return fmt.Errorf("only queued, not-yet-started runs can be edited (state %s); use Continue or Cancel+Retry", r.State)
	}
	if in.Prompt != "" {
		cipher, err := c.vault.Seal(in.Prompt)
		if err != nil {
			return err
		}
		r.PromptCipherStore = cipher
		r.PromptHash = model.HashPrompt(in.Prompt)
		r.PromptPreview = model.PreviewRedacted(in.Prompt, 120)
	}
	if in.Provider != "" {
		r.Provider = in.Provider
	}
	r.Model, r.Effort, r.Permission = in.Model, in.Effort, in.Permission
	if in.Permission != "" {
		r.Permission = in.Permission
	}
	r.AttachSecrets = in.AttachSecrets
	r.Staged = in.Staged
	if r.Staged && r.Stage == model.StageNone {
		r.Stage = model.StagePlan
	}
	return c.store.UpdateRun(r)
}

// ReorderQueue moves a queued run within FIFO bounds (never ahead of its
// batch row dependencies).
func (c *Conductor) ReorderQueue(runID string, beforeRunID string) error {
	r, err := c.store.GetRun(runID)
	if err != nil || r == nil {
		return fmt.Errorf("run not found")
	}
	if r.State != model.StateQueued {
		return fmt.Errorf("only QUEUED runs can be reordered")
	}
	// Build ordering; block moving ahead of an earlier row of same batch.
	if beforeRunID != "" {
		target, err := c.store.GetRun(beforeRunID)
		if err != nil || target == nil || target.State != model.StateQueued {
			return fmt.Errorf("target must be a queued run")
		}
		if target.BatchID == r.BatchID && target.SortKey < r.SortKey {
			// moving earlier within same batch: ensure no row dependency violated
			rows, _ := c.store.ListRows(r.BatchID)
			pos := map[string]int{}
			for _, row := range rows {
				pos[row.ID] = row.Position
			}
			rRow, tRow := pos[r.RowID], pos[target.RowID]
			if tRow > rRow {
				return fmt.Errorf("cannot move a run ahead of its batch row dependency")
			}
		}
		r.SortKey = target.SortKey - 1
	} else {
		r.SortKey = time.Now().UnixNano()
	}
	if err := c.store.UpdateRun(r); err != nil {
		return err
	}
	c.Poke()
	return nil
}

// PauseScheduler / ResumeScheduler toggle queue processing.
func (c *Conductor) PauseScheduler() error {
	s := c.Settings()
	s.Paused = true
	return c.UpdateSettings(s)
}

func (c *Conductor) ResumeScheduler() error {
	s := c.Settings()
	s.Paused = false
	return c.UpdateSettings(s)
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

// DeleteSecretBlock removes the stored block (queued runs attach nothing).
func (c *Conductor) DeleteSecretBlock() error {
	// keep table but mark deleted via setting flag
	return c.store.SetSetting("secretBlock.deleted", true)
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
	c.emit("batchUpdated", b.ID)
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
			fmt.Fprintf(&sb, "## Run %d — %s\n", n, strings.ToUpper(r.Provider[:1])+r.Provider[1:])
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

func newID(prefix string) string {
	return fmt.Sprintf("%s_%s", prefix, fmt.Sprintf("%d%d", time.Now().UnixNano(), time.Now().Unix()%1000))
}
