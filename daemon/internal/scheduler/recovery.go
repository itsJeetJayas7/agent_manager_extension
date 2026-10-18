package scheduler

import (
	"fmt"
	"time"

	"agent-conductor/internal/model"
)

// Internal recovery failures must be visible instead of leaving a run active
// with no live provider and no next dispatch.
func (c *Conductor) queueRecovery(id, prompt, why string, at *time.Time) {
	if err := c.queueContinuation(id, prompt, why, at); err != nil {
		c.log.Errorf("queue recovery %s: %v", id, err)
		if r, _ := c.store.GetRun(id); r != nil {
			c.finishFailure(r, "RECOVERY", err.Error())
		}
	}
}

// All continuations re-enter the scheduler. They must obey pause, quota,
// row-barrier and concurrency rules, just like new work.
func (c *Conductor) queueContinuation(id, prompt, why string, at *time.Time) error {
	cipher := ""
	var err error
	if prompt != "" {
		cipher, err = c.vault.Seal(prompt)
		if err != nil {
			return err
		}
	}
	err = c.store.TransitionRun(id, model.StateQueued, &model.RunEvent{
		Type: model.EventContinuationSent, Timestamp: c.Now(), Data: quoteJSON(why),
	}, func(x *model.Run) {
		x.PendingPromptCipher = cipher
		x.NextEligibleAt = at
		x.FinishedAt = time.Time{}
		x.FinalOutcome = ""
	})
	if err != nil {
		return err
	}
	r, err := c.store.GetRun(id)
	if err != nil {
		return err
	}
	// A manually continued completed row must rejoin its batch too.
	if err := c.store.UpdateRowStatus(r.RowID, model.RowRunning); err != nil {
		return err
	}
	b, err := c.store.GetBatch(r.BatchID)
	if err != nil {
		return err
	}
	if b != nil && b.Status != model.BatchRunning {
		b.Status, b.FinishedAt = model.BatchRunning, time.Time{}
		if err := c.store.UpdateBatch(b); err != nil {
			return err
		}
		c.emitBatch(b.ID)
	}
	c.emitRun(id)
	c.Poke()
	return nil
}

func (c *Conductor) recoveryPrompt(r *model.Run) string {
	if !r.MeaningfulProgress || r.ProviderSessionID == "" {
		if r.PendingPromptCipher != "" {
			if text, err := c.vault.Open(r.PendingPromptCipher); err == nil {
				return text
			}
		}
		return "" // rebuild the entire execution contract, original task and opted-in secrets
	}
	if r.Staged && r.Stage == model.StagePlan {
		return "Continue the read-only plan from where you stopped. End with <AC_STAGE>PLAN_READY</AC_STAGE>."
	}
	return model.ContinueAfterQuota
}

func (c *Conductor) retryTransient(r *model.Run, cls model.ErrorClass, msg string) bool {
	switch cls {
	case model.ErrNetwork, model.ErrProviderOverloaded, model.ErrProcessCrash:
	default:
		return false
	}
	limit := c.Settings().MaxTransientRetries
	if r.RetryCount >= limit {
		return false
	}
	delay := 5 * time.Second
	for i := 0; i < r.RetryCount && delay < 5*time.Minute; i++ {
		delay *= 2
	}
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	at := c.Now().Add(delay)
	if !c.transition(r.ID, r.State, nil, func(x *model.Run) {
		x.RetryCount++
		x.ErrorCode = string(cls)
		x.ErrorMsg = fmt.Sprintf("Automatic retry %d/%d: %s", x.RetryCount, limit, truncate(c.log.Redactor().Redact(msg), 350))
	}) {
		return false
	}
	if err := c.queueContinuation(r.ID, c.recoveryPrompt(r), "recoverable provider failure", &at); err != nil {
		c.log.Errorf("schedule retry %s: %v", r.ID, err)
		return false
	}
	return true
}
