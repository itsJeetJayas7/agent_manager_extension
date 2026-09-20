package scheduler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"agent-conductor/internal/model"
	"agent-conductor/internal/providers"
)

// startRun moves a queued run through preparation into execution.
func (c *Conductor) startRun(ctx context.Context, r *model.Run) error {
	now := c.Now()
	// Interactive Claude runs need an explicitly trusted workspace; never
	// accept trust silently on the user's behalf.
	if r.Provider == model.ProviderClaude {
		if claude, ok := c.registry.Get(model.ProviderClaude); ok {
			if cl, ok := claude.(*providers.Claude); ok {
				cwd := r.Cwd
				if r.WorktreePath != "" {
					cwd = r.WorktreePath
				}
				if !cl.IsWorkspaceTrusted(cwd) {
					if err := c.store.TransitionRun(r.ID, model.StateNeedsAttention, nil, func(x *model.Run) {
						x.ErrorCode = "WORKSPACE_UNTRUSTED"
						x.ErrorMsg = "Claude Code has not trusted this folder yet. Open it once in Claude Code (or use Trust Workspace in Agent Conductor), then Continue the run."
					}); err != nil {
						return err
					}
					c.emitRun(r.ID)
					return nil
				}
			}
		}
	}
	if err := c.store.TransitionRun(r.ID, model.StatePreparing, &model.RunEvent{Type: model.EventQueued, Timestamp: now}, nil); err != nil {
		return err
	}
	c.launchExecution(r, "", false)
	c.emitRun(r.ID)
	return nil
}

// launchExecution assembles the effective prompt and runs the provider turn.
// promptOverride is set for continuations/fix loops; empty = fresh start.
func (c *Conductor) launchExecution(r *model.Run, promptOverride string, isContinue bool) {
	adapter, ok := c.registry.Get(r.Provider)
	if !ok {
		c.finishFailure(r, string(model.ErrUnknown), "no adapter for provider "+r.Provider)
		return
	}

	// decrypt user prompt
	userPrompt, err := c.decryptPrompt(r)
	if err != nil {
		c.finishFailure(r, "VAULT", "cannot decrypt stored prompt: "+err.Error())
		return
	}

	// staged intelligence: PLAN phase settings
	planPhase := r.Staged && r.Stage == model.StagePlan
	modelID, effort := r.Model, r.Effort
	permission := r.Permission
	attachSecrets := r.AttachSecrets
	if r.Staged {
		s := c.Settings()
		if r.Provider == model.ProviderClaude {
			if planPhase {
				modelID, effort = s.ClaudePlanModel, s.ClaudePlanEffort
				permission = "plan"
				attachSecrets = false // never leak secrets into planning phase by default
			} else if r.Stage == model.StageAudit {
				modelID, effort = s.ClaudeAuditModel, s.ClaudeAuditEffort
				permission = "plan" // audit read-only first
			} else {
				modelID, effort = s.ClaudeBuildModel, s.ClaudeBuildEffort
				permission = r.Permission
			}
		} else {
			if planPhase {
				if s.CodexPlanModel != "" {
					modelID = s.CodexPlanModel
				}
				effort = s.CodexPlanEffort
				permission = "plan"
				attachSecrets = false
			} else if r.Stage == model.StageAudit {
				if s.CodexAuditModel != "" {
					modelID = s.CodexAuditModel
				}
				effort = s.CodexAuditEffort
				permission = "plan"
			} else {
				if s.CodexBuildModel != "" {
					modelID = s.CodexBuildModel
				}
				effort = s.CodexBuildEffort
				permission = r.Permission
			}
		}
	}

	var effective string
	switch {
	case promptOverride != "":
		effective = promptOverride
	default:
		// A continuation always carries an override, so there is no separate
		// isContinue branch; the old one was unreachable.
		var secretBlock string
		if attachSecrets {
			block, _ := c.latestSecretBlock()
			secretBlock = block
		}
		effective = model.EffectivePrompt(userPrompt, attachSecrets, secretBlock, planPhase)
	}
	if isContinue && attachSecrets && promptOverride != "" && strings.Contains(promptOverride, "implement the original user request") {
		// staged build continuation: attach secrets block if enabled
		if block, _ := c.latestSecretBlock(); block != "" {
			effective = effective + "\n\n[SHARED TESTING / SUDO BLOCK — USER SUPPLIED]\n\n" + block + "\n[END SHARED TESTING / SUDO BLOCK]"
		}
	}

	cwd := r.Cwd
	if r.WorktreePath != "" {
		cwd = r.WorktreePath
	}

	dispatchUUID := newDispatchUUID(r.ID, r.Attempt+1)
	if err := c.store.TransitionRun(r.ID, pickActiveState(r), &model.RunEvent{Type: model.EventTurnStarted, Timestamp: c.Now()}, func(x *model.Run) {
		x.DispatchUUID = dispatchUUID
		x.Attempt = r.Attempt + 1
		x.StartedAt = c.Now()
		x.MeaningfulProgress = false
	}); err != nil {
		// Dispatching against a run whose persisted state does not match is how
		// runs ended up wedged; refuse instead.
		c.log.Errorf("dispatch %s: %v", r.ID, err)
		c.finishFailure(r, "DISPATCH", err.Error())
		return
	}
	c.store.RecordAttemptStart(r.ID, r.Attempt+1, dispatchUUID)
	runID := r.ID
	spec := providers.ExecSpec{
		RunID:      runID,
		Provider:   r.Provider,
		Model:      modelID,
		Effort:     effort,
		Permission: permission,
		Cwd:        cwd,
		Prompt:     effective,
		SessionID:  r.ProviderSessionID,
		Continue:   isContinue,
		Stage:      r.Stage,
		Title:      r.ProviderTitle,
		Env:        map[string]string{},
	}
	execCtx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	c.cancels[runID] = cancel
	c.mu.Unlock()

	go func() {
		defer func() {
			c.mu.Lock()
			delete(c.cancels, runID)
			c.mu.Unlock()
			cancel()
			c.Poke()
		}()
		err := adapter.Execute(execCtx, spec, func(ev providers.Event) { c.onProviderEvent(runID, ev) })
		if err != nil {
			c.log.Errorf("execute %s: %v", runID, err)
			c.finishFailure(r, "ADAPTER", err.Error())
		}
	}()
}

// pickActiveState is the state a run enters when its turn is dispatched.
//
// AUTO_RESUMING is a transient pre-dispatch marker, never a dispatched state:
// it has no legal edge to COMPLETED, so returning it here left resumed runs
// stuck forever. This only ever worked because callers passed a stale,
// pre-transition copy of the run.
// transition applies a state change and never swallows a refusal.
//
// Roughly two dozen call sites discarded TransitionRun's error, so a run could
// silently stay in a state its caller had already moved past. Failures are now
// logged and written to the run's timeline where the user can see them.
func (c *Conductor) transition(runID string, to model.RunState, evt *model.RunEvent, mut func(*model.Run)) bool {
	if err := c.store.TransitionRun(runID, to, evt, mut); err != nil {
		c.log.Errorf("transition %s -> %s: %v", runID, to, err)
		c.store.AddRunEvent(runID, model.EventStateChanged, quoteJSON("refused: "+err.Error()))
		return false
	}
	return true
}

func pickActiveState(r *model.Run) model.RunState {
	if r.ProviderSessionID != "" {
		return model.StateRunning
	}
	return model.StateStarting
}

// onProviderEvent processes one adapter event for a run.
func (c *Conductor) onProviderEvent(runID string, ev providers.Event) {
	r, err := c.store.GetRun(runID)
	if err != nil || r == nil {
		return
	}
	switch ev.Kind {
	case providers.EvSessionStarted:
		if r.ProviderSessionID == "" && ev.SessionID != "" {
			c.transition(r.ID, model.StateRunning, &model.RunEvent{Type: model.EventProviderSessionCreated, Timestamp: ev.At}, func(x *model.Run) {
				x.ProviderSessionID = ev.SessionID
				x.ProviderSubSession = ev.SubSession
			})
			c.store.AddRunEvent(r.ID, model.EventProviderSessionCreated, quoteJSON(ev.SessionID))
		} else if ev.SessionID != "" && ev.SessionID != r.ProviderSessionID {
			c.transition(r.ID, r.State, nil, func(x *model.Run) {
				x.ProviderSessionID = ev.SessionID
			})
		}
	case providers.EvTurnStarted:
		if ev.TurnID != "" {
			c.transition(r.ID, r.State, nil, func(x *model.Run) {
				x.ProviderTurnID = ev.TurnID
			})
		}
		// Real work naturally anchors the current quota window: claim the
		// anchor slot so the synthetic anchor never double-spends it.
		c.markWindowAnchoredByWork(r.Provider)
	case providers.EvMessageDelta:
		// Persist only the first meaningful delta. Writing on every delta meant
		// a DB round-trip plus a UI fan-out per streamed token.
		if ev.Progress && !r.MeaningfulProgress {
			c.transition(r.ID, r.State, nil, func(x *model.Run) {
				x.MeaningfulProgress = true
			})
			break
		}
		return // nothing changed: no emitRun, no query next time
	case providers.EvQuotaUpdate:
		c.observeQuota(r.Provider, ev.Quota)
	case providers.EvProcessExited:
		// mid-turn crash with progress: continuation preferred
		if r.MeaningfulProgress && r.ProviderSessionID != "" {
			c.transition(r.ID, model.StateNeedsAttention, nil, func(x *model.Run) {
				x.ErrorCode = string(model.ErrProcessCrash)
				x.ErrorMsg = "provider process exited mid-turn; same-session continue available"
			})
		} else {
			c.finishFailure(r, string(model.ErrProcessCrash), "provider process exited mid-turn")
		}
	case providers.EvTurnCompleted:
		c.onTurnCompleted(r, ev)
	}
	c.emitRun(runID)
}

func (c *Conductor) onTurnCompleted(r *model.Run, ev providers.Event) {
	attempt := r.Attempt
	c.store.FinishAttempt(r.ID, attempt, ev.SessionID, ev.TurnID, string(ev.Status), ev.Progress)
	switch ev.Status {
	case providers.TurnInterrupted:
		switch {
		case r.State == model.StateCancelRequested:
			c.transition(r.ID, model.StateCancelled, &model.RunEvent{Type: model.EventRunCancelled, Timestamp: c.Now()}, func(x *model.Run) {
				x.FinishedAt = c.Now()
			})
		case c.wasPauseHeld(r.ID):
			// Stopped by an INCLUDE_ACTIVE pause, not by a failure. The native
			// session is intact and the run is continued when the queue
			// resumes.
			c.transition(r.ID, model.StatePaused, &model.RunEvent{Type: model.EventPausedAtSafePoint, Timestamp: c.Now()}, func(x *model.Run) {
				x.ErrorCode = ""
				x.ErrorMsg = ""
			})
		default:
			c.finishFailure(r, string(model.ErrUserInterrupt), "run interrupted")
		}
		return
	case providers.TurnFailed:
		if ev.Error != nil && ev.Error.Class == model.ErrRateLimit {
			c.handleRateLimit(r, ev)
			return
		}
		cls := model.ErrUnknown
		msg := "turn failed"
		if ev.Error != nil {
			cls, msg = ev.Error.Class, ev.Error.Message
		}
		c.finishFailure(r, string(cls), msg)
		return
	case providers.TurnCompletedOK:
		// staged PLAN phase completion?
		if r.Staged && r.Stage == model.StagePlan {
			c.onPlanComplete(r, ev.FinalMsg)
			return
		}
		outcome, reason := model.ParseResultMarker(ev.FinalMsg)
		switch outcome {
		case "COMPLETE":
			c.store.AddRunEvent(r.ID, model.EventCompletionMarkerFound, quoteJSON("COMPLETE"))
			c.beginVerification(r)
		case "BLOCKED":
			c.store.AddRunEvent(r.ID, model.EventCompletionMarkerFound, quoteJSON("BLOCKED"))
			c.transition(r.ID, model.StateBlocked, nil, func(x *model.Run) {
				x.FinalOutcome = "BLOCKED: " + reason
				x.FinishedAt = c.Now()
			})
			c.emit("runUpdated", r.ID)
			c.Poke()
		default:
			// Agent stopped without the required marker after the completion
			// gate already gave one continuation: needs human attention.
			c.transition(r.ID, model.StateNeedsAttention, nil, func(x *model.Run) {
				x.ErrorCode = "NO_RESULT_MARKER"
				x.ErrorMsg = "agent stopped without <AC_RESULT> marker"
				x.FinishedAt = c.Now()
			})
			c.Poke()
		}
	}
}

// onPlanComplete validates the staged plan phase and launches BUILD on the
// same native session.
func (c *Conductor) onPlanComplete(r *model.Run, planMsg string) {
	if !model.HasStageMarker(planMsg) {
		c.transition(r.ID, model.StateNeedsAttention, nil, func(x *model.Run) {
			x.ErrorCode = "NO_PLAN_MARKER"
			x.ErrorMsg = "plan phase ended without <AC_STAGE>PLAN_READY</AC_STAGE> marker"
			x.FinishedAt = c.Now()
		})
		c.Poke()
		return
	}
	c.transition(r.ID, model.StateRunning, &model.RunEvent{Type: model.EventStageChanged, Timestamp: c.Now()}, func(x *model.Run) {
		x.Stage = model.StageBuild
	})
	c.launchExecution(r, model.BuildStageContinuation, true)
	c.emitRun(r.ID)
}

// handleRateLimit records quota observations and schedules the same-session
// resume after every exhausted window resets.
func (c *Conductor) handleRateLimit(r *model.Run, ev providers.Event) {
	provider := r.Provider
	// enrich from structured provider data when available
	if adapter, ok := c.registry.Get(provider); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if ws, err := adapter.ProbeQuota(ctx); err == nil && len(ws) > 0 {
			c.observeQuota(provider, ws)
		}
		cancel()
	}
	now := c.Now()
	ne, hasTime, missing := c.nextEligible(provider, now)
	c.store.AddRunEvent(r.ID, model.EventRateLimitDetected, quoteJSON("provider rate limited"))
	if !hasTime || missing {
		// Claude weekly with unknown reset: surface honestly, wait for user override or provider event.
		c.transition(r.ID, model.StateWaitingForQuota, nil, func(x *model.Run) {
			x.NextEligibleAt = nil
			x.ErrorMsg = "quota exhausted; reset time unknown — configure weekly reset override or wait for provider data"
		})
		c.store.AddRunEvent(r.ID, model.EventWaitScheduled, quoteJSON("unknown reset"))
		c.Poke()
		return
	}
	c.transition(r.ID, model.StateWaitingForQuota, &model.RunEvent{Type: model.EventWaitScheduled, Timestamp: now}, func(x *model.Run) {
		t := ne
		x.NextEligibleAt = &t
	})
	c.store.AddRunEvent(r.ID, model.EventWaitScheduled, quoteJSON(ne.Format(time.RFC3339)))
	c.log.Infof("run %s waiting for quota until %s (same-session resume scheduled)", r.ID, ne.Format(time.RFC3339))
	c.Poke()
}

// observeQuota feeds the guardian and persists windows.
func (c *Conductor) observeQuota(provider string, ws []providers.QuotaWindow) {
	if len(ws) == 0 {
		return
	}
	now := c.Now()
	c.guardian.Observe(provider, ws, now)
	for _, w := range ws {
		c.store.UpsertQuotaWindow(quotaRowFromWindow(provider, w))
	}
	c.emit("quotaUpdated", provider)
}

// defaultVerificationTimeout bounds a verification pass when no setting is
// configured. It used to be 30 minutes, hard-coded and unconfigurable.
const defaultVerificationTimeout = 30 * time.Minute

// beginVerification runs the optional configured verification commands.
func (c *Conductor) beginVerification(r *model.Run) {
	s := c.Settings()
	if len(s.VerificationCommands) == 0 || r.Staged && r.Stage == model.StageAudit {
		c.completeRun(r)
		return
	}
	c.transition(r.ID, model.StateVerifying, &model.RunEvent{Type: model.EventVerificationStarted, Timestamp: c.Now()}, nil)
	go func() {
		cwd := r.Cwd
		if r.WorktreePath != "" {
			cwd = r.WorktreePath
		}
		timeout := time.Duration(s.VerificationTimeoutSec) * time.Second
		if timeout <= 0 {
			timeout = defaultVerificationTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		c.store.AddRunEvent(r.ID, model.EventVerificationStarted,
			quoteJSON(fmt.Sprintf("%d command(s), timeout %s", len(s.VerificationCommands), timeout)))
		ok, summary := c.verifier.Verify(ctx, cwd, s.VerificationCommands)
		// Verification output can echo anything the commands printed, so it is
		// redacted before it is persisted or shown.
		summary = c.log.Redactor().Redact(summary)
		r2, _ := c.store.GetRun(r.ID)
		if r2 == nil {
			return
		}
		c.store.AddRunEvent(r.ID, model.EventVerificationFinished, quoteJSON(strings.TrimSpace(summary)))
		if ok {
			c.completeRun(r2)
		} else if r2.VerifyCycles < s.MaxVerifyFixCycles {
			// resume the SAME provider session once with a concise failure report
			r2.VerifyCycles++
			c.store.UpdateRun(r2)
			fix := "Your verification commands failed. Fix the failures and complete the task again.\n\nVerification output (truncated):\n" +
				truncate(redactLines(summary), 4000) +
				"\n\nEnd with the standard Agent Conductor result marker."
			c.launchExecution(r2, fix, true)
		} else {
			c.transition(r.ID, model.StateNeedsAttention, nil, func(x *model.Run) {
				x.ErrorCode = "VERIFICATION_FAILED"
				x.ErrorMsg = truncate(redactLines(summary), 500)
				x.FinishedAt = c.Now()
			})
			c.Poke()
		}
	}()
}

// completeRun finalizes a successful run (including optional audit stage).
func (c *Conductor) completeRun(r *model.Run) {
	s := c.Settings()
	if r.Staged && s.AuditEnabled && r.Stage != model.StageAudit && r.AuditCycles < s.MaxAuditCycles {
		c.transition(r.ID, model.StateRunning, &model.RunEvent{Type: model.EventStageChanged, Timestamp: c.Now()}, func(x *model.Run) {
			x.Stage = model.StageAudit
			x.AuditCycles++
		})
		audit := `Audit the completed work read-only. Review the actual diff and test results for missed requirements or regressions.
If everything is genuinely complete and verified, reply with exactly: <AC_RESULT>COMPLETE</AC_RESULT>
If you find a real issue, reply with exactly: <AC_RESULT>BLOCKED: precise description of the issue</AC_RESULT>`
		c.launchExecution(r, audit, true)
		return
	}
	c.transition(r.ID, model.StateCompleted, &model.RunEvent{Type: model.EventRunCompleted, Timestamp: c.Now()}, func(x *model.Run) {
		x.FinalOutcome = "COMPLETE"
		x.FinishedAt = c.Now()
		x.ErrorMsg = ""
	})
	c.store.AddRunEvent(r.ID, model.EventRunCompleted, quoteJSON("COMPLETE"))
	c.Poke()
}

// finishFailure marks a run failed with a redacted message.
func (c *Conductor) finishFailure(r *model.Run, code, msg string) {
	c.transition(r.ID, model.StateFailed, &model.RunEvent{Type: model.EventRunFailed, Timestamp: c.Now()}, func(x *model.Run) {
		x.ErrorCode = code
		x.ErrorMsg = truncate(msg, 500)
		x.FinishedAt = c.Now()
	})
	c.store.AddRunEvent(r.ID, model.EventRunFailed, quoteJSON(code))
	c.Poke()
}

// emitRun publishes the changed run itself, not just its id, so the UI can
// merge one entity instead of re-requesting a full snapshot per event.
func (c *Conductor) emitRun(runID string) {
	r, err := c.store.GetRun(runID)
	if err != nil || r == nil {
		c.emit("runUpdated", map[string]any{"id": runID})
		return
	}
	c.emit("runUpdated", r)
}

// emitBatch publishes the changed batch with its rows, so the UI can merge it.
func (c *Conductor) emitBatch(batchID string) {
	b, err := c.store.GetBatch(batchID)
	if err != nil || b == nil {
		c.emit("batchUpdated", map[string]any{"id": batchID})
		return
	}
	rows, _ := c.store.ListRows(batchID)
	if rows == nil {
		rows = []*model.Row{}
	}
	c.emit("batchUpdated", map[string]any{"batch": b, "rows": rows})
}

func newDispatchUUID(runID string, attempt int) string {
	return fmt.Sprintf("%s-%d-%d", runID, attempt, time.Now().UnixNano())
}

// truncate shortens to n runes. Byte slicing split UTF-8 sequences and
// produced mojibake in the UI.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// sensitiveWords marks a whole output line for removal. Three keywords caught
// almost nothing.
var sensitiveWords = []string{
	"password", "passwd", "authorization:", "api_key", "apikey", "api-key",
	"secret", "token", "credential", "private key", "-----begin",
	"sk-ant-", "sk-proj-", "ghp_", "bearer ", "aws_secret", "client_secret",
	"passphrase", "sudo",
}

func containsSensitiveWord(lower string) bool {
	for _, w := range sensitiveWords {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

// redactLines strips obviously sensitive lines from verification output
// before storing (fail-safe; primary redaction happens in the logger).
func redactLines(s string) string {
	keep := []string{}
	for _, l := range strings.Split(s, "\n") {
		low := strings.ToLower(l)
		if containsSensitiveWord(low) {
			keep = append(keep, "[line redacted]")
			continue
		}
		keep = append(keep, l)
	}
	return strings.Join(keep, "\n")
}
