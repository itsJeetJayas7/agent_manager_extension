package model

import (
	"testing"
	"time"
)

func TestCanTransitionValid(t *testing.T) {
	cases := []struct{ from, to RunState }{
		{StateDraft, StateQueued},
		{StateQueued, StatePreparing},
		{StatePreparing, StateStarting},
		{StateStarting, StateRunning},
		{StateRunning, StateWaitingForQuota},
		{StateWaitingForQuota, StateAutoResuming},
		{StateAutoResuming, StateRunning},
		{StateRunning, StateVerifying},
		{StateVerifying, StateCompleted},
		{StateRunning, StateCancelRequested},
		{StateCancelRequested, StateCancelled},
		{StateCompleted, StateRunning}, // manual continue
	}
	for _, c := range cases {
		if !CanTransition(c.from, c.to) {
			t.Errorf("expected transition %s -> %s to be legal", c.from, c.to)
		}
	}
}

func TestCanTransitionInvalid(t *testing.T) {
	cases := []struct{ from, to RunState }{
		{StateDraft, StateRunning},
		{StateQueued, StateRunning},
		{StateCompleted, StateQueued},
		{StateCancelled, StateRunning},
		{StateSkipped, StateRunning},
		{StateFailed, StateCompleted},
	}
	for _, c := range cases {
		if CanTransition(c.from, c.to) {
			t.Errorf("expected transition %s -> %s to be illegal", c.from, c.to)
		}
	}
}

func TestParseResultMarker(t *testing.T) {
	if out, _ := ParseResultMarker("done.\n<AC_RESULT>COMPLETE</AC_RESULT>"); out != "COMPLETE" {
		t.Errorf("want COMPLETE got %q", out)
	}
	out, reason := ParseResultMarker("cannot.\n<AC_RESULT>BLOCKED: missing env X</AC_RESULT>")
	if out != "BLOCKED" || reason != "missing env X" {
		t.Errorf("want BLOCKED/missing env X got %q/%q", out, reason)
	}
	if out, _ := ParseResultMarker("just talking"); out != "" {
		t.Errorf("expected no marker, got %q", out)
	}
}

func TestEffectivePromptOrdering(t *testing.T) {
	p := EffectivePrompt("TASK A", true, "SECRETPW", false)
	if !contains(p, ExecutionContract) || !contains(p, "TASK A") || !contains(p, "SECRETPW") {
		t.Fatal("prompt missing sections")
	}
	if !contains(p, "[USER TASK — VERBATIM]") || !contains(p, "[SHARED TESTING / SUDO BLOCK — USER SUPPLIED]") {
		t.Fatal("prompt missing delimiters")
	}
	// ordering: contract < task < secrets
	if !(idx(p, ExecutionContract) < idx(p, "TASK A") && idx(p, "TASK A") < idx(p, "SECRETPW")) {
		t.Fatal("wrong section ordering")
	}
	// plan phase: no secrets even when flag on
	pp := EffectivePrompt("TASK A", true, "SECRETPW", true)
	if contains(pp, "SECRETPW") {
		t.Fatal("secret leaked into plan phase")
	}
	if !contains(pp, PlanStageInstruction) {
		t.Fatal("plan instruction missing")
	}
	// secrets flag off: no block
	np := EffectivePrompt("TASK A", false, "SECRETPW", false)
	if contains(np, "SECRETPW") {
		t.Fatal("secret appended when disabled")
	}
}

func contains(s, sub string) bool { return len(sub) == 0 || idx(s, sub) >= 0 }

func idx(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestTerminalAndActive(t *testing.T) {
	if !StateCompleted.Terminal() || !StateFailed.Terminal() {
		t.Error("completed/failed should be terminal")
	}
	if StateRunning.Terminal() {
		t.Error("running should not be terminal")
	}
	if !StateRunning.Active() || StateWaitingForQuota.Active() {
		t.Error("active classification wrong")
	}
	if !StateVerifying.Active() {
		t.Error("verifying should hold slot")
	}
}

func TestPreviewRedacted(t *testing.T) {
	in := "First line\nsecond line with SECRETPW"
	got := PreviewRedacted(in, 20)
	if got != "First line" {
		t.Errorf("got %q", got)
	}
	long := ""
	for i := 0; i < 100; i++ {
		long += "x"
	}
	if got := PreviewRedacted(long, 20); len([]rune(got)) != 21 { // 20 + ellipsis
		t.Errorf("preview truncation wrong: %q", got)
	}
}

func TestClassify(t *testing.T) {
	if ClassifyClaudeStopFailure("rate_limit") != ErrRateLimit {
		t.Error("rate_limit")
	}
	if ClassifyClaudeStopFailure("authentication_failed") != ErrAuth {
		t.Error("auth")
	}
	if ClassifyCodexError("", "Usage limit reached") != ErrRateLimit {
		t.Error("codex rate limit")
	}
}

func TestDeterministicTitle(t *testing.T) {
	title := DeterministicSessionTitle(14, 3, 2, "Authentication frontend overhaul now")
	if title != "[AC B14-R03-C02] Authentication frontend overhaul now" {
		t.Errorf("got %q", title)
	}
	_ = time.Now()
}
