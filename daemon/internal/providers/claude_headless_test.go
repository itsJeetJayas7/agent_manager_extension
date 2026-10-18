package providers

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-conductor/internal/logging"
	"agent-conductor/internal/model"
)

func feed(t *testing.T, lines []string) []Event {
	t.Helper()
	c := NewClaude(logging.NewNop(), NewHookBus(), "/bin/true", "")
	var events []Event
	st := &headlessState{
		spec: ExecSpec{RunID: "run1"},
		sink: func(e Event) { events = append(events, e) },
	}
	for _, l := range lines {
		var msg streamMessage
		if err := json.Unmarshal([]byte(l), &msg); err != nil {
			t.Fatalf("bad fixture line: %v\n%s", err, l)
		}
		c.handleStreamMessage(st, msg)
	}
	return events
}

func TestLegacyPTYCancellationReapsUncooperativeChild(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ntrap '' INT\nsleep 30 &\nwait\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c := NewClaude(logging.NewNop(), NewHookBus(), "/bin/true", "")
	c.binPath = bin
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	var terminal Event
	err := c.executePTY(ctx, ExecSpec{RunID: "legacy", Cwd: dir}, func(ev Event) {
		if ev.Kind == EvTurnCompleted {
			terminal = ev
		}
	})
	if err != nil || terminal.Status != TurnInterrupted || time.Since(start) > 7*time.Second || len(c.ActiveRuns()) != 0 {
		t.Fatalf("legacy child did not terminate: err=%v event=%+v duration=%v", err, terminal, time.Since(start))
	}
}

func findEvent(events []Event, kind EventKind) (Event, bool) {
	for _, e := range events {
		if e.Kind == kind {
			return e, true
		}
	}
	return Event{}, false
}

// Fixtures captured from claude 2.1.278 (`claude -p --output-format
// stream-json --verbose`). See docs/provider-capabilities.md.
const (
	fxInit   = `{"type":"system","subtype":"init","cwd":"/tmp/x","session_id":"8fa948e7-3186-4110-9e55-5717a165dace","tools":[],"model":"claude-haiku-4-5-20251001","permissionMode":"plan"}`
	fxRate   = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1789887600,"rateLimitType":"five_hour","unifiedWindows":{"five_hour":{"utilization":0.84,"resetsAt":1789887600},"seven_day":{"utilization":0.06,"resetsAt":1790485200}}}}`
	fxAsst   = `{"type":"assistant","message":{"model":"claude-sonnet-5","role":"assistant","content":[{"type":"text","text":"OK"}]},"session_id":"8fa948e7-3186-4110-9e55-5717a165dace"}`
	fxTool   = `{"type":"assistant","message":{"model":"claude-sonnet-5","role":"assistant","content":[{"type":"tool_use","name":"Edit"}]},"session_id":"s"}`
	fxResult = `{"type":"result","subtype":"success","is_error":false,"result":"done\n<AC_RESULT>COMPLETE</AC_RESULT>","stop_reason":"end_turn","num_turns":1,"session_id":"8fa948e7-3186-4110-9e55-5717a165dace","permission_denials":[]}`
)

func TestHeadlessStreamHappyPath(t *testing.T) {
	events := feed(t, []string{fxInit, fxRate, fxAsst, fxResult})

	ss, ok := findEvent(events, EvSessionStarted)
	if !ok || ss.SessionID != "8fa948e7-3186-4110-9e55-5717a165dace" {
		t.Fatalf("session id not surfaced: %+v", ss)
	}
	if _, ok := findEvent(events, EvTurnStarted); !ok {
		t.Fatal("no turn started event")
	}
	done, ok := findEvent(events, EvTurnCompleted)
	if !ok {
		t.Fatal("no turn completed event")
	}
	if done.Status != TurnCompletedOK {
		t.Fatalf("status %s", done.Status)
	}
	if !strings.Contains(done.FinalMsg, "AC_RESULT") {
		t.Fatalf("final message lost the completion marker: %q", done.FinalMsg)
	}
	if !done.Progress {
		t.Fatal("assistant text is meaningful progress")
	}
}

// §4.1 — MeaningfulProgress drives the resume-vs-restart decision after a
// quota wait. It used to come only from a non-empty final message because the
// tool hooks it branched on were never registered.
func TestHeadlessToolUseCountsAsProgress(t *testing.T) {
	events := feed(t, []string{fxInit, fxTool})
	d, ok := findEvent(events, EvMessageDelta)
	if !ok {
		t.Fatal("tool use produced no observation")
	}
	if !d.Progress {
		t.Fatal("a tool call is evidence the agent began work")
	}
	if !strings.Contains(d.Text, "Edit") {
		t.Fatalf("tool name lost: %q", d.Text)
	}
}

// §4.1 — Claude does expose structured quota, contrary to the old comment.
func TestHeadlessParsesStructuredQuota(t *testing.T) {
	events := feed(t, []string{fxInit, fxRate})
	q, ok := findEvent(events, EvQuotaUpdate)
	if !ok {
		t.Fatal("rate_limit_event produced no quota observation")
	}
	byID := map[string]QuotaWindow{}
	for _, w := range q.Quota {
		byID[w.ID] = w
	}
	five, ok := byID["five_hour"]
	if !ok {
		t.Fatalf("five_hour window missing: %+v", q.Quota)
	}
	if five.Name != "5-hour" {
		t.Fatalf("window name %q", five.Name)
	}
	// utilization is a fraction; the conductor works in percent.
	if five.UsedPercent < 83.9 || five.UsedPercent > 84.1 {
		t.Fatalf("utilization not converted to percent: %v", five.UsedPercent)
	}
	if !five.HasReset || five.ResetsAt.Unix() != 1789887600 {
		t.Fatalf("reset time lost: %+v", five)
	}
	if five.Exhausted {
		t.Fatal("84%% used is not exhausted")
	}
	if five.Confidence != "exact" || five.Source != "provider_structured" {
		t.Fatalf("confidence must reflect that this is structured data: %+v", five)
	}
	week := byID["seven_day"]
	if week.Name != "Weekly" {
		t.Fatalf("weekly window name %q", week.Name)
	}
}

func TestHeadlessMarksExhaustedWindow(t *testing.T) {
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1789887600,"rateLimitType":"five_hour","unifiedWindows":{"five_hour":{"utilization":1.0,"resetsAt":1789887600},"seven_day":{"utilization":0.2,"resetsAt":1790485200}}}}`
	events := feed(t, []string{fxInit, line})
	q, _ := findEvent(events, EvQuotaUpdate)
	for _, w := range q.Quota {
		if w.ID == "five_hour" && !w.Exhausted {
			t.Fatal("a rejected five_hour window must be exhausted")
		}
		if w.ID == "seven_day" && w.Exhausted {
			t.Fatal("the weekly window was not the one rejected")
		}
	}
}

// A rejection naming a window absent from unifiedWindows must still register.
func TestHeadlessRecordsRejectedWindowNotInUnifiedList(t *testing.T) {
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1789887600,"rateLimitType":"seven_day","unifiedWindows":{}}}`
	events := feed(t, []string{line})
	q, ok := findEvent(events, EvQuotaUpdate)
	if !ok {
		t.Fatal("no quota event")
	}
	if len(q.Quota) != 1 || q.Quota[0].ID != "seven_day" || !q.Quota[0].Exhausted {
		t.Fatalf("rejected window not recorded: %+v", q.Quota)
	}
}

func TestHeadlessClassifiesFailure(t *testing.T) {
	cases := []struct {
		line string
		want model.ErrorClass
	}{
		{`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Claude AI usage limit reached"}`, model.ErrRateLimit},
		{`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"authentication failed"}`, model.ErrAuth},
		{`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"the model is overloaded"}`, model.ErrProviderOverloaded},
		{`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"something odd"}`, model.ErrUnknown},
	}
	for _, tc := range cases {
		events := feed(t, []string{fxInit, tc.line})
		done, ok := findEvent(events, EvTurnCompleted)
		if !ok || done.Status != TurnFailed {
			t.Fatalf("expected a failed turn for %s", tc.line)
		}
		if done.Error == nil || done.Error.Class != tc.want {
			t.Fatalf("classified %v, want %s for %s", done.Error, tc.want, tc.line)
		}
	}
}

func TestHeadlessDetectsInterruption(t *testing.T) {
	line := `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"","terminal_reason":"interrupted"}`
	events := feed(t, []string{fxInit, line})
	done, _ := findEvent(events, EvTurnCompleted)
	if done.Status != TurnInterrupted {
		t.Fatalf("status %s, want interrupted", done.Status)
	}
}

func TestHeadlessIgnoresUnknownLines(t *testing.T) {
	events := feed(t, []string{fxInit, `{"type":"something_new","payload":{"a":1}}`, fxResult})
	if done, ok := findEvent(events, EvTurnCompleted); !ok || done.Status != TurnCompletedOK {
		t.Fatal("an unknown stream frame must not derail the turn")
	}
}

// §4.1 — only ANTHROPIC_API_KEY was stripped, so a base-URL override or a
// Bedrock/Vertex switch silently redirected billing away from the
// subscription.
func TestSubscriptionProtectionStripsEveryBillingRedirect(t *testing.T) {
	for _, name := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
		"ANTHROPIC_MODEL", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX",
		"AWS_SECRET_ACCESS_KEY", "GOOGLE_APPLICATION_CREDENTIALS",
	} {
		t.Setenv(name, "value")
	}
	t.Setenv("PATH", "/usr/bin")
	env := claudeChildEnv(false, map[string]string{"AGENT_CONDUCTOR_RUN_ID": "r1"})
	joined := strings.Join(env, "\n")
	for _, name := range []string{
		"ANTHROPIC_API_KEY=", "ANTHROPIC_AUTH_TOKEN=", "ANTHROPIC_BASE_URL=",
		"ANTHROPIC_MODEL=", "CLAUDE_CODE_USE_BEDROCK=", "CLAUDE_CODE_USE_VERTEX=",
		"AWS_SECRET_ACCESS_KEY=", "GOOGLE_APPLICATION_CREDENTIALS=",
	} {
		if strings.Contains(joined, name) {
			t.Fatalf("%s was not stripped from the managed child environment", name)
		}
	}
	if !strings.Contains(joined, "PATH=/usr/bin") {
		t.Fatal("unrelated environment must be preserved")
	}
	if !strings.Contains(joined, "AGENT_CONDUCTOR_RUN_ID=r1") {
		t.Fatal("managed run id missing")
	}

	// API mode is an explicit opt-in that keeps them.
	env = claudeChildEnv(true, nil)
	if !strings.Contains(strings.Join(env, "\n"), "ANTHROPIC_API_KEY=value") {
		t.Fatal("API mode must keep the key")
	}
}

// §4.3 — the gate belongs only to managed runs, on Stop.
func TestManagedSettingsGateOnlyOnStop(t *testing.T) {
	c := NewClaude(logging.NewNop(), NewHookBus(), "/usr/local/bin/agent-conductor", "")
	raw, err := c.managedSettingsJSON()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	cmdFor := func(ev string) string {
		return doc.Hooks[ev][0].Hooks[0].Command
	}
	if !strings.Contains(cmdFor("Stop"), "--gate") {
		t.Fatalf("Stop must carry the completion gate: %q", cmdFor("Stop"))
	}
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Notification", "SessionEnd", "PreToolUse", "PostToolUse"} {
		if _, ok := doc.Hooks[ev]; !ok {
			t.Fatalf("%s hook is not registered", ev)
		}
		if strings.Contains(cmdFor(ev), "--gate") {
			t.Fatalf("%s must be awareness-only: %q", ev, cmdFor(ev))
		}
	}
}

func TestTurnDeadlineIsConfigurable(t *testing.T) {
	c := NewClaude(logging.NewNop(), NewHookBus(), "/bin/true", "")
	if c.turnDeadline() != defaultTurnDeadline {
		t.Fatal("default deadline wrong")
	}
	c.SetTurnDeadline(90 * time.Minute)
	if c.turnDeadline() != 90*time.Minute {
		t.Fatal("deadline not applied")
	}
	c.SetTurnDeadline(0)
	if c.turnDeadline() != defaultTurnDeadline {
		t.Fatal("zero should restore the default")
	}
}

func TestQuotaWarningDoesNotExhaustWindow(t *testing.T) {
	q := quotaFromRateLimitEvent(&claudeRateLimitInfo{Status: "allowed_warning", RateLimitType: "five_hour", ResetsAt: 2000000000, UnifiedWindows: map[string]claudeRateLimitWin{"five_hour": {Utilization: 0.85, ResetsAt: 2000000000}}})
	if len(q) != 1 || q[0].Exhausted {
		t.Fatalf("warning incorrectly blocked queue: %+v", q)
	}
}

func TestHeadlessUsesErrorsArray(t *testing.T) {
	events := feed(t, []string{`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["API Error: 529 overloaded"],"result":""}`})
	done, _ := findEvent(events, EvTurnCompleted)
	if done.Error == nil || done.Error.Class != model.ErrProviderOverloaded || !strings.Contains(done.Error.Message, "529") {
		t.Fatalf("lost structured error detail: %+v", done)
	}
}

func TestExplicitClaudeModelsAndEffortMatrix(t *testing.T) {
	byID := map[string]ModelInfo{}
	for _, m := range claudeModels {
		byID[m.ID] = m
	}
	opus := byID["claude-opus-5-5"]
	if opus.Name != "Claude Opus 5.5" || strings.Join(opus.Efforts, ",") != "low,medium,high,xhigh,max" {
		t.Fatalf("wrong Opus capabilities: %+v", opus)
	}
	if len(byID["claude-haiku-4-5-20251001"].Efforts) != 0 {
		t.Fatal("Haiku has no effort control")
	}
	if strings.Contains(strings.Join(byID["claude-opus-4-6"].Efforts, ","), "xhigh") {
		t.Fatal("unsupported effort must not be advertised")
	}
	if !strings.Contains(byID["opus"].Name, "alias") {
		t.Fatal("moving alias must be labeled")
	}
}

func TestHeadlessEOFDoesNotDisableDeadline(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude")
	// Closing stdout is not a process exit: the old cancellation watcher
	// stopped here and cmd.Wait hung forever on the still-running child.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec 1>&-\nsleep 30\n"), 0755); err != nil {
		t.Fatal(err)
	}
	c := NewClaude(logging.NewNop(), NewHookBus(), "/bin/true", "")
	c.binPath = bin
	yes := true
	c.streamOK = &yes
	c.effortOK = &yes
	c.permissionPromptsOK = &yes
	c.SetTurnDeadline(100 * time.Millisecond)
	start := time.Now()
	var terminal Event
	err := c.Execute(context.Background(), ExecSpec{RunID: "eof", Cwd: t.TempDir(), Prompt: "test"}, func(e Event) {
		if e.Kind == EvTurnCompleted {
			terminal = e
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 7*time.Second || terminal.Error == nil || terminal.Error.Class != model.ErrNetwork {
		t.Fatalf("deadline not enforced: %+v", terminal)
	}
	if len(c.ActiveRuns()) != 0 {
		t.Fatal("process registration leaked")
	}
}

func TestHeadlessPassesEveryEffortAndPinnedModelOnResume(t *testing.T) {
	c := NewClaude(logging.NewNop(), NewHookBus(), "/bin/true", "")
	yes := true
	c.effortOK = &yes
	c.permissionPromptsOK = &yes
	for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
		args := c.headlessArgs(context.Background(), ExecSpec{Model: "claude-opus-5-5", Effort: effort, SessionID: "same-session"}, "", "{}")
		joined := strings.Join(args, " ")
		for _, want := range []string{"--resume same-session", "--model claude-opus-5-5", "--effort " + effort} {
			if !strings.Contains(joined, want) {
				t.Fatalf("missing %q in %q", want, joined)
			}
		}
	}
}

func TestHeadlessPartialOutputIsVisibleBeforeCompletionWithoutDuplicates(t *testing.T) {
	events := feed(t, []string{
		fxInit,
		`{"type":"stream_event","event":{"type":"message_start"}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"private reasoning"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"O"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"K"}}}`,
		fxAsst, fxTool,
		`{"type":"user","message":{"content":[{"type":"tool_result"}]}}`,
	})
	var output string
	var thinking, result bool
	for _, ev := range events {
		if ev.Kind == EvMessageDelta && !strings.HasPrefix(ev.Text, "tool:") {
			output += ev.Text
		}
		if ev.Text == "Thinking" {
			thinking = true
		}
		if ev.Text == "Tool result received" {
			result = true
		}
		if strings.Contains(ev.Text, "private reasoning") {
			t.Fatal("thinking contents exposed")
		}
	}
	if output != "OK" || !thinking || !result {
		t.Fatalf("lost/duplicated partial activity: output=%q thinking=%v toolResult=%v", output, thinking, result)
	}
}

func TestPartialMessagesCapabilityGated(t *testing.T) {
	for _, supported := range []bool{false, true} {
		c := NewClaude(logging.NewNop(), NewHookBus(), "/bin/true", "")
		c.partialOK = &supported
		permissionPrompts := false
		c.permissionPromptsOK = &permissionPrompts
		args := c.headlessArgs(context.Background(), ExecSpec{}, "s", "")
		found := false
		for _, arg := range args {
			if arg == "--include-partial-messages" {
				found = true
			}
		}
		if found != supported {
			t.Fatalf("partial messages unsupported=%v args=%v", !supported, args)
		}
	}
}
