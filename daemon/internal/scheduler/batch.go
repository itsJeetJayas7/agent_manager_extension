package scheduler

import (
	"context"
	"fmt"
	"path/filepath"

	"agent-conductor/internal/db"
	"agent-conductor/internal/model"
	"agent-conductor/internal/worktrees"
)

// advanceBatch moves one batch forward: prepares worktrees, dispatches row
// runs respecting slots, enforces barriers, integrates parallel results.
func (c *Conductor) advanceBatch(ctx context.Context, b *model.Batch, st *slotState) error {
	rows, err := c.store.ListRows(b.ID)
	if err != nil {
		return err
	}
	runs, err := c.store.ListRunsByBatch(b.ID)
	if err != nil {
		return err
	}
	runsByRow := map[string][]*model.Run{}
	for _, r := range runs {
		runsByRow[r.RowID] = append(runsByRow[r.RowID], r)
	}

	firstStarted := false
	for _, row := range rows {
		rowRuns := runsByRow[row.ID]
		if len(rowRuns) == 0 {
			// Neither rowDone nor rowAllStarted can ever be true for an empty
			// slice, so such a row stalled its batch forever. Creation rejects
			// them now; this skips any that predate that check.
			if row.Status != model.RowDone {
				c.store.UpdateRowStatus(row.ID, model.RowDone)
				c.log.Errorf("batch %s row %d has no runs; skipping", b.ID, row.Position+1)
			}
			continue
		}
		if row.Status == model.RowPending || row.Status == model.RowRunning {
			// preconditions for starting this row
			if row.Status == model.RowPending && row.Mode == model.RowParallel {
				if b.IntegrationWTPath == "" {
					if err := c.prepareParallelBase(b); err != nil {
						return c.failBatch(b, err.Error())
					}
					b2, _ := c.store.GetBatch(b.ID)
					if b2 != nil {
						b = b2
					}
				}
				if len(rowRuns) > 0 && rowRuns[0].WorktreePath == "" {
					if err := c.prepareCardWorktrees(b, row, rowRuns); err != nil {
						return c.failBatch(b, err.Error())
					}
					runs, _ = c.store.ListRunsByBatch(b.ID)
					runsByRow = map[string][]*model.Run{}
					for _, r := range runs {
						runsByRow[r.RowID] = append(runsByRow[r.RowID], r)
					}
					rowRuns = runsByRow[row.ID]
				}
			}
			// After parallel integration, later rows run inside the
			// integration worktree so they see all integrated work.
			if b.IntegrationWTPath != "" {
				for _, r := range rowRuns {
					if r.WorktreePath == "" && r.Cwd == b.BaseCwd {
						r.Cwd = b.IntegrationWTPath
						c.store.UpdateRun(r)
					}
				}
			}
			// dispatch eligible runs of this row (also re-dispatches retried
			// runs inside RUNNING rows)
			anyStarted, err := c.dispatchRow(ctx, b, row, rowRuns, st, row.Mode == model.RowParallel)
			if err != nil {
				return err
			}
			if anyStarted {
				if row.Status == model.RowPending {
					c.store.UpdateRowStatus(row.ID, model.RowRunning)
				}
				if !firstStarted && b.Status == model.BatchQueued {
					b.Status = model.BatchRunning
					b.StartedAt = c.Now()
					c.store.UpdateBatch(b)
					c.emit("batchUpdated", b.ID)
				}
				firstStarted = true
			}
			// If nothing could start due to external busy, mark waiting state on runs.
			c.markWaitingExternal(rowRuns, st)
			// A still-pending row that cannot start blocks this batch here.
			if row.Status == model.RowPending && !anyStarted && len(rowRuns) > 0 && !rowDone(rowRuns) {
				return nil
			}
		}

		if row.Status == model.RowRunning || rowAllStarted(rowRuns) {
			if !rowDone(rowRuns) {
				if rowFailed(rowRuns) {
					// Batch failure rule: no subsequent row starts until retry/skip/cancel.
					if b.Status != model.BatchAttention {
						b.Status = model.BatchAttention
						c.store.UpdateBatch(b)
						c.emit("batchUpdated", b.ID)
					}
					return nil
				}
				return nil // barrier: row still working
			}
			// row finished successfully
			if row.Mode == model.RowParallel {
				if err := c.integrateRow(b, row, rowRuns); err != nil {
					return err
				}
			}
			c.store.UpdateRowStatus(row.ID, model.RowDone)
			c.emit("batchUpdated", b.ID)
			continue // next row
		}
		return nil
	}

	// all rows done
	if b.Status != model.BatchDone {
		b.Status = model.BatchDone
		b.FinishedAt = c.Now()
		c.store.UpdateBatch(b)
		c.emit("batchUpdated", b.ID)
	}
	return nil
}

// dispatchRow starts eligible runs of a row within slot limits.
func (c *Conductor) dispatchRow(ctx context.Context, b *model.Batch, row *model.Row, rowRuns []*model.Run, st *slotState, isParallel bool) (bool, error) {
	started := false
	_ = ctx
	for _, r := range rowRuns {
		if r.State != model.StateQueued && r.State != model.StateWaitingForSlot && r.State != model.StateWaitingForExternal {
			continue
		}
		ok, _ := st.canStart(r.Provider, isParallel)
		if !ok {
			continue
		}
		if err := c.startRun(ctx, r); err != nil {
			c.log.Errorf("start run %s: %v", r.ID, err)
			continue
		}
		st.counts[r.Provider]++
		st.total++
		started = true
	}
	return started, nil
}

// markWaitingExternal records why queued runs cannot start yet.
func (c *Conductor) markWaitingExternal(rowRuns []*model.Run, st *slotState) {
	for _, r := range rowRuns {
		if r.State != model.StateQueued && r.State != model.StateWaitingForSlot && r.State != model.StateWaitingForExternal {
			continue
		}
		ok, _ := st.canStart(r.Provider, false)
		if ok {
			// back to plain QUEUED if previously marked waiting
			if r.State != model.StateQueued && model.CanTransition(r.State, model.StateQueued) {
				c.transition(r.ID, model.StateQueued, nil, nil)
			}
			continue
		}
		if len(st.busyExt) > 0 {
			if model.CanTransition(r.State, model.StateWaitingForExternal) {
				c.transition(r.ID, model.StateWaitingForExternal, nil, nil)
			}
		} else if model.CanTransition(r.State, model.StateWaitingForSlot) {
			c.transition(r.ID, model.StateWaitingForSlot, nil, nil)
		}
	}
}

func rowDone(runs []*model.Run) bool {
	for _, r := range runs {
		switch r.State {
		case model.StateCompleted, model.StateSkipped:
			continue
		default:
			return false
		}
	}
	return len(runs) > 0
}

// rowFailed: any run in a failure state that blocks the barrier.
func rowFailed(runs []*model.Run) bool {
	for _, r := range runs {
		switch r.State {
		case model.StateFailed, model.StateNeedsAttention, model.StateBlocked,
			model.StateCancelled, model.StateOrphaned:
			return true
		}
	}
	return false
}

func rowAllStarted(runs []*model.Run) bool {
	for _, r := range runs {
		if r.State == model.StateQueued || r.State == model.StateWaitingForSlot || r.State == model.StateWaitingForExternal {
			return false
		}
	}
	return len(runs) > 0
}

func (c *Conductor) failBatch(b *model.Batch, why string) error {
	b2, _ := c.store.GetBatch(b.ID)
	if b2 == nil {
		b2 = b
	}
	b2.Status = model.BatchAttention
	c.store.UpdateBatch(b2)
	runs, _ := c.store.ListRunsByBatch(b.ID)
	for _, r := range runs {
		if r.State == model.StateQueued {
			c.transition(r.ID, model.StateNeedsAttention, nil, func(x *model.Run) {
				x.ErrorCode = "WORKSPACE"
				x.ErrorMsg = why
			})
		}
	}
	c.log.Errorf("batch %s blocked: %s", b.ID, why)
	c.emit("batchUpdated", b.ID)
	return fmt.Errorf("%s", why)
}

// prepareParallelBase verifies preconditions and creates the integration
// branch + worktree for a parallel batch.
func (c *Conductor) prepareParallelBase(b *model.Batch) error {
	if err := c.wt.Precheck(b.BaseCwd); err != nil {
		return err
	}
	layout := c.wt.Layout(batchNumberOf(b), b.ID, b.BaseCwd)
	if err := c.wt.CreateIntegration(b.BaseCwd, layout.IntegrationBranch, layout.IntegrationDir, b.BaseCommit); err != nil {
		return err
	}
	b.IntegrationBranch = layout.IntegrationBranch
	b.IntegrationWTPath = layout.IntegrationDir
	c.store.UpdateBatch(b)
	c.store.AddWorktree(&db.WorktreeRecord{BatchID: b.ID, Path: layout.IntegrationDir, Branch: layout.IntegrationBranch, Kind: "integration", Status: "ACTIVE"})
	c.log.Infof("integration worktree ready: %s (%s)", layout.IntegrationDir, layout.IntegrationBranch)
	return nil
}

// prepareCardWorktrees creates isolated worktrees for each parallel card.
func (c *Conductor) prepareCardWorktrees(b *model.Batch, row *model.Row, rowRuns []*model.Run) error {
	layout := c.wt.Layout(batchNumberOf(b), b.ID, b.BaseCwd)
	for i, r := range rowRuns {
		if r.WorktreePath != "" {
			continue
		}
		// Both indices are 1-based; they used to mix 1-indexed rows with
		// 0-indexed cards.
		branch := fmt.Sprintf("%s/r%02dc%02d", layout.CardBranchPrefix, row.Position+1, i+1)
		dir := filepath.Join(filepath.Dir(layout.IntegrationDir), fmt.Sprintf("card-r%02dc%02d", row.Position+1, i+1))
		if err := c.wt.CardWorktree(b.BaseCwd, branch, dir, b.IntegrationBranch); err != nil {
			return err
		}
		c.store.AddWorktree(&db.WorktreeRecord{BatchID: b.ID, RunID: r.ID, Path: dir, Branch: branch, Kind: "card", Status: "ACTIVE"})
		c.transition(r.ID, r.State, nil, func(x *model.Run) {
			x.WorktreePath = dir
			x.Branch = branch
		})
		c.store.AddRunEvent(r.ID, model.EventWorktreeCreated, quoteJSON(dir))
		c.log.Infof("card worktree ready: %s (%s)", dir, branch)
	}
	return nil
}

// integrateRow commits card work and merges into the integration branch.
// Conflict => AWAITING_INTEGRATION with worktrees preserved.
func (c *Conductor) integrateRow(b *model.Batch, row *model.Row, rowRuns []*model.Run) error {
	batchNum := batchNumberOf(b)
	layout := c.wt.Layout(batchNum, b.ID, b.BaseCwd)
	// deterministic order: card index
	for _, r := range rowRuns {
		if r.State == model.StateSkipped || r.WorktreePath == "" {
			continue
		}
		msg := fmt.Sprintf("Agent Conductor: B%02d R%02d", batchNum, row.Position+1)
		if _, err := c.wt.CommitAll(r.WorktreePath, msg); err != nil {
			return c.awaitIntegration(b, fmt.Sprintf("commit %s failed: %v", r.WorktreePath, err))
		}
		c.store.AddRunEvent(r.ID, model.EventWorktreeCommitted, quoteJSON(msg))
		if err := c.wt.MergeBranch(layout.IntegrationDir, r.Branch); err != nil {
			if mc, ok := err.(*worktrees.MergeConflictError); ok {
				return c.awaitIntegration(b, fmt.Sprintf("conflict merging %s: %v", r.Branch, mc.Files))
			}
			return c.awaitIntegration(b, err.Error())
		}
		c.store.AddRunEvent(r.ID, model.EventIntegrationStarted, quoteJSON("merged "+r.Branch))
	}
	return nil
}

// awaitIntegration stops the batch with every worktree preserved.
func (c *Conductor) awaitIntegration(b *model.Batch, why string) error {
	b2, _ := c.store.GetBatch(b.ID)
	if b2 == nil {
		b2 = b
	}
	b2.Status = model.BatchAttention
	c.store.UpdateBatch(b2)
	runs, _ := c.store.ListRunsByBatch(b.ID)
	for _, r := range runs {
		if r.State == model.StateQueued || r.State == model.StateRunning || r.State == model.StateIntegrating {
			c.transition(r.ID, model.StateAwaitingIntegration, nil, func(x *model.Run) {
				x.ErrorMsg = why
			})
			c.store.AddRunEvent(r.ID, model.EventIntegrationConflict, quoteJSON(why))
		}
	}
	c.log.Errorf("batch %s awaiting integration: %s", b.ID, why)
	c.emit("batchUpdated", b.ID)
	return fmt.Errorf("integration stopped: %s", why)
}

// batchNumberOf returns the batch's persisted monotonic number.
//
// This used to hash the batch id into 1..89. Those numbers drive git branch
// names, so collisions were near-certain and `git branch -f` then silently
// destroyed a previous batch's integration branch along with its work.
// Batches created before the batch_number column exists fall back to 0, which
// the worktree layout disambiguates with the batch id.
func batchNumberOf(b *model.Batch) int {
	return int(b.Number)
}
