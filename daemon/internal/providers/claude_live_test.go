package providers

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agent-conductor/internal/logging"
	"agent-conductor/internal/model"
)

// The test binary doubles as a fake `claude` that speaks the stream-json
// protocol of Claude Code 2.1.288 as observed live (see
// docs/claude-native-live-view.md): initialize is answered once, every user
// message is echoed with isReplay and its uuid, then system/init, output and
// a result follow; an interrupt ends the turn with error_during_execution.
func TestMain(m *testing.M) {
	if os.Getenv("AC_FAKE_CLAUDE") == "1" {
		fakeClaude()
		return
	}
	os.Exit(m.Run())
}

func fakeClaude() {
	out := bufio.NewWriter(os.Stdout)
	var mu sync.Mutex
	emit := func(v any) {
		b, _ := json.Marshal(v)
		mu.Lock()
		out.Write(append(b, '\n'))
		out.Flush()
		mu.Unlock()
	}
	if log := os.Getenv("AC_FAKE_LOG"); log != "" {
		f, _ := os.OpenFile(log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		fmt.Fprintf(f, "pid=%d entrypoint=%s args=%s\n", os.Getpid(), os.Getenv("CLAUDE_CODE_ENTRYPOINT"), strings.Join(os.Args[1:], " "))
		f.Close()
	}
	sid := ""
	for i, a := range os.Args {
		if (a == "--session-id" || a == "--resume") && i+1 < len(os.Args) {
			sid = os.Args[i+1]
		}
	}
	slow := os.Getenv("AC_FAKE_SLOW") == "1"
	interrupt := make(chan struct{}, 1)
	work := make(chan map[string]any, 16)
	go func() {
		for msg := range work {
			emit(map[string]any{"type": "system", "subtype": "init", "session_id": sid, "model": "fake-model", "uuid": newUUID()})
			content, _ := msg["message"].(map[string]any)["content"].(string)
			emit(map[string]any{"type": "assistant", "uuid": newUUID(), "session_id": sid,
				"message": map[string]any{"model": "fake-model", "content": []any{map[string]any{"type": "tool_use", "name": "Bash"}}}})
			if slow {
				// A tool process in Claude's process group, as Bash tools are.
				tool := exec.Command("sleep", "30")
				tool.Start()
				if f := os.Getenv("AC_FAKE_TOOLPID"); f != "" {
					os.WriteFile(f, []byte(fmt.Sprint(tool.Process.Pid)), 0o600)
				}
				// Like Claude, an interrupt rejects the tool use without
				// killing the tool process itself.
				select {
				case <-interrupt:
					emit(map[string]any{"type": "user", "uuid": newUUID(), "session_id": sid,
						"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user for tool use]"}}}})
					emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "stop_reason": "tool_use", "session_id": sid})
					continue
				case <-time.After(1500 * time.Millisecond):
				}
			}
			text := "reply to: " + content + "\n<AC_RESULT>COMPLETE</AC_RESULT>"
			emit(map[string]any{"type": "assistant", "uuid": newUUID(), "session_id": sid,
				"message": map[string]any{"model": "fake-model", "content": []any{map[string]any{"type": "text", "text": text}}}})
			emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": text, "session_id": sid})
		}
	}()
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for in.Scan() {
		var m map[string]any
		if json.Unmarshal(in.Bytes(), &m) != nil {
			continue
		}
		switch m["type"] {
		case "control_request":
			req, _ := m["request"].(map[string]any)
			resp := map[string]any{"subtype": "success", "request_id": m["request_id"]}
			switch req["subtype"] {
			case "initialize":
				resp["response"] = map[string]any{"commands": []any{}, "models": []any{}, "pid": os.Getpid()}
			case "interrupt":
				select {
				case interrupt <- struct{}{}:
				default:
				}
			case "get_settings":
				resp["response"] = map[string]any{"effective": map[string]any{}}
			}
			emit(map[string]any{"type": "control_response", "response": resp})
		case "user":
			m["isReplay"] = true
			emit(m)
			work <- m
		}
	}
	close(work)
	time.Sleep(100 * time.Millisecond)
}

func fakeClaudeAdapter(t *testing.T, slow bool) (*Claude, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "fake.log")
	bin := filepath.Join(dir, "claude")
	self, _ := os.Executable()
	script := fmt.Sprintf("#!/bin/sh\nAC_FAKE_CLAUDE=1 AC_FAKE_LOG=%q AC_FAKE_SLOW=%v AC_FAKE_TOOLPID=%q exec %q \"$@\"\n", logPath, map[bool]string{true: "1"}[slow], filepath.Join(dir, "tool.pid"), self)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "config"))
	c := NewClaude(logging.NewNop(), NewHookBus(), "/bin/true", "")
	c.binPath = bin
	yes, no := true, false
	c.streamOK, c.streamInputOK, c.effortOK, c.partialOK = &yes, &yes, &yes, &no
	c.permissionPromptsOK = &yes
	return c, logPath
}

func collect() (Sink, func() []Event) {
	var mu sync.Mutex
	var evs []Event
	return func(e Event) { mu.Lock(); evs = append(evs, e); mu.Unlock() },
		func() []Event { mu.Lock(); defer mu.Unlock(); return append([]Event(nil), evs...) }
}

func spawnCount(t *testing.T, logPath string) int {
	b, _ := os.ReadFile(logPath)
	return strings.Count(string(b), "pid=")
}

// Root cause 1: print-mode transcripts are tagged sdk-cli, which the VS Code
// extension leaves out of the conversations it can open. Managed processes
// must carry their own entrypoint and the streaming protocol.
func TestLiveProcessIsLabeledAndStreams(t *testing.T) {
	c, logPath := fakeClaudeAdapter(t, false)
	sink, events := collect()
	if err := c.Execute(context.Background(), ExecSpec{RunID: "r1", Cwd: t.TempDir(), Prompt: "first"}, sink); err != nil {
		t.Fatal(err)
	}
	done, ok := findEvent(events(), EvTurnCompleted)
	if !ok || done.Status != TurnCompletedOK || !strings.Contains(done.FinalMsg, "reply to: first") {
		t.Fatalf("turn did not complete through stdin: %+v", done)
	}
	log, _ := os.ReadFile(logPath)
	for _, want := range []string{"entrypoint=" + conductorEntrypoint, "--input-format stream-json", "--replay-user-messages", "--permission-prompts none"} {
		if !strings.Contains(string(log), want) {
			t.Fatalf("managed process missing %q: %s", want, log)
		}
	}
	if strings.Contains(string(log), "first") {
		t.Fatal("prompt must travel on stdin, never argv")
	}
	c.Close()
}

// Continuations reuse the run's live process, so an attached Claude Code tab
// keeps following the conversation and no second process ever resumes it.
func TestContinuationReusesLiveProcess(t *testing.T) {
	c, logPath := fakeClaudeAdapter(t, false)
	defer c.Close()
	sink, events := collect()
	cwd := t.TempDir()
	c.Execute(context.Background(), ExecSpec{RunID: "r1", Cwd: cwd, Prompt: "first"}, sink)
	ss, _ := findEvent(events(), EvSessionStarted)
	if ss.SessionID == "" {
		t.Fatal("no confirmed session")
	}
	sink2, events2 := collect()
	c.Execute(context.Background(), ExecSpec{RunID: "r1", Cwd: cwd, Prompt: "second", SessionID: ss.SessionID, Continue: true}, sink2)
	done, _ := findEvent(events2(), EvTurnCompleted)
	if done.Status != TurnCompletedOK || !strings.Contains(done.FinalMsg, "second") {
		t.Fatalf("continuation failed: %+v", done)
	}
	if n := spawnCount(t, logPath); n != 1 {
		t.Fatalf("continuation started %d processes; must reuse the live one", n)
	}
}

type pipeClient struct {
	toDaemon *io.PipeWriter
	lines    chan map[string]any
	raw      chan string
}

func attachClient(t *testing.T, ls *liveSession) *pipeClient {
	t.Helper()
	pr, pw := io.Pipe()
	pc := &pipeClient{toDaemon: pw, lines: make(chan map[string]any, 256), raw: make(chan string, 256)}
	go ls.ServeNative(bufio.NewReader(pr), func(b []byte) error {
		var m map[string]any
		json.Unmarshal(b, &m)
		pc.lines <- m
		pc.raw <- string(b)
		return nil
	}, func() { pr.Close() })
	return pc
}

func (pc *pipeClient) send(t *testing.T, v any) {
	b, _ := json.Marshal(v)
	if _, err := pc.toDaemon.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

func (pc *pipeClient) waitFor(t *testing.T, pred func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case m := <-pc.lines:
			if pred(m) {
				return m
			}
		case <-deadline:
			t.Fatal("timed out waiting for a line on the attached tab")
		}
	}
}

func liveOf(c *Claude, runID string) *liveSession {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ls := range c.live {
		ls.mu.Lock()
		match := ls.runID == runID
		ls.mu.Unlock()
		if match {
			return ls
		}
	}
	return nil
}

// Root cause 2: a tab can only follow a run that it is attached to. The
// attached tab gets its initialize answered from the process's own response,
// then live output, its own control requests routed back, and its messages
// executed by the same process.
func TestNativeTabFollowsAndControlsRunningTurn(t *testing.T) {
	c, logPath := fakeClaudeAdapter(t, true)
	defer c.Close()
	sink, events := collect()
	done := make(chan struct{})
	go func() {
		c.Execute(context.Background(), ExecSpec{RunID: "r1", Cwd: t.TempDir(), Prompt: "long task"}, sink)
		close(done)
	}()
	var ls *liveSession
	for i := 0; i < 100 && ls == nil; i++ {
		time.Sleep(20 * time.Millisecond)
		ls = liveOf(c, "r1")
	}
	if ls == nil {
		t.Fatal("no live process")
	}
	tab := attachClient(t, ls)
	tab.send(t, map[string]any{"type": "control_request", "request_id": "tab-init", "request": map[string]any{"subtype": "initialize", "hooks": map[string]any{}}})
	init := tab.waitFor(t, func(m map[string]any) bool { return m["type"] == "control_response" })
	if r := init["response"].(map[string]any); r["request_id"] != "tab-init" || r["response"] == nil {
		t.Fatalf("initialize not answered from the cached response: %v", init)
	}
	tab.send(t, map[string]any{"type": "control_request", "request_id": "tab-7", "request": map[string]any{"subtype": "get_settings"}})
	got := tab.waitFor(t, func(m map[string]any) bool {
		r, _ := m["response"].(map[string]any)
		return m["type"] == "control_response" && r["request_id"] == "tab-7"
	})
	if got == nil {
		t.Fatal("control response not routed back with the tab's id")
	}
	tab.waitFor(t, func(m map[string]any) bool { return m["type"] == "result" })
	<-done
	if st, _ := c.LiveStatus("r1"); st.Viewers != 1 || st.Attaches != 1 {
		t.Fatalf("attachment not reported: %+v", st)
	}
	// A message typed in the tab runs in the same process, as a native turn
	// that is not reported to the scheduler as Conductor's.
	before := len(events())
	tab.send(t, map[string]any{"type": "user", "uuid": "tab-msg-1", "message": map[string]any{"role": "user", "content": "follow-up from tab"}})
	echo := tab.waitFor(t, func(m map[string]any) bool { return m["type"] == "user" && m["uuid"] == "tab-msg-1" })
	if echo["isReplay"] != true {
		t.Fatal("tab message not acknowledged")
	}
	tab.waitFor(t, func(m map[string]any) bool { return m["type"] == "result" })
	if n := spawnCount(t, logPath); n != 1 {
		t.Fatalf("%d processes for one session", n)
	}
	if len(events()) != before {
		t.Fatal("a native turn must not be reported as the run's turn")
	}
}

// Stop in the Claude Code tab interrupts the managed turn and is reported as a
// user interrupt from Claude Code, not as a provider failure.
func TestNativeStopIsAUserInterrupt(t *testing.T) {
	c, _ := fakeClaudeAdapter(t, true)
	defer c.Close()
	sink, events := collect()
	done := make(chan struct{})
	go func() {
		c.Execute(context.Background(), ExecSpec{RunID: "r1", Cwd: t.TempDir(), Prompt: "long task"}, sink)
		close(done)
	}()
	var ls *liveSession
	for i := 0; i < 100 && ls == nil; i++ {
		time.Sleep(20 * time.Millisecond)
		ls = liveOf(c, "r1")
	}
	tab := attachClient(t, ls)
	tab.send(t, map[string]any{"type": "control_request", "request_id": "i", "request": map[string]any{"subtype": "initialize"}})
	tab.waitFor(t, func(m map[string]any) bool { return m["type"] == "assistant" })
	tab.send(t, map[string]any{"type": "control_request", "request_id": "stop-1", "request": map[string]any{"subtype": "interrupt"}})
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("interrupted turn never finished")
	}
	ev, _ := findEvent(events(), EvTurnCompleted)
	if ev.Status != TurnInterrupted || ev.Error == nil || ev.Error.Class != model.ErrUserInterrupt || !strings.Contains(ev.Error.Message, "Claude Code") {
		t.Fatalf("native stop misreported: %+v %+v", ev, ev.Error)
	}
}

// Cancelling in Agent Conductor interrupts and ends the process, even with a
// tab attached: cancellation stops execution.
func TestCancelEndsLiveProcess(t *testing.T) {
	c, _ := fakeClaudeAdapter(t, true)
	sink, events := collect()
	done := make(chan struct{})
	go func() {
		c.Execute(context.Background(), ExecSpec{RunID: "r1", Cwd: t.TempDir(), Prompt: "long task"}, sink)
		close(done)
	}()
	var ls *liveSession
	for i := 0; i < 100 && ls == nil; i++ {
		time.Sleep(20 * time.Millisecond)
		ls = liveOf(c, "r1")
	}
	attachClient(t, ls)
	time.Sleep(200 * time.Millisecond)
	if err := c.Cancel("r1"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("cancel did not end the turn")
	}
	select {
	case <-ls.exited:
	case <-time.After(15 * time.Second):
		t.Fatal("cancel left the process running")
	}
	ev, _ := findEvent(events(), EvTurnCompleted)
	if ev.Status != TurnInterrupted {
		t.Fatalf("cancel reported %+v", ev)
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(c.binPath), "tool.pid"))
	if err != nil {
		t.Fatal(err)
	}
	var tool int
	fmt.Sscan(string(b), &tool)
	time.Sleep(200 * time.Millisecond)
	if st := processState(tool); st != "" && !strings.HasPrefix(st, "Z") {
		syscall.Kill(tool, syscall.SIGKILL)
		t.Fatalf("cancel left the run's tool process running (state %q)", st)
	}
}

// A tab opening mid-turn already shows the transcript; replaying lines it has
// would duplicate them (the extension does not de-duplicate). Only output after
// the newest persisted line is replayed.
func TestReplaySkipsWhatTheTranscriptAlreadyHas(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	proj := filepath.Join(dir, "projects", "-tmp-x")
	os.MkdirAll(proj, 0o700)
	os.WriteFile(filepath.Join(proj, "sess.jsonl"), []byte(`{"uuid":"u1"}`+"\n"+`{"uuid":"u2"}`+"\n"), 0o600)
	ls := &liveSession{sessionID: "sess"}
	for _, l := range []struct{ line, uuid string }{
		{`{"type":"user","uuid":"u1"}`, "u1"},
		{`{"type":"assistant","uuid":"u2"}`, "u2"},
		{`{"type":"system","subtype":"status","uuid":"s1"}`, "s1"},
		{`{"type":"stream_event","uuid":"p1"}`, "p1"},
	} {
		ls.remember([]byte(l.line), l.uuid)
	}
	got := ls.replay()
	if len(got) != 2 || !strings.Contains(string(got[0]), "s1") || !strings.Contains(string(got[1]), "p1") {
		t.Fatalf("replay must start after the newest persisted line: %q", got)
	}
}

// A managed session already open in another Claude process is not started a
// second time for a tab.
func TestHostingRefusesSessionHeldElsewhere(t *testing.T) {
	c, logPath := fakeClaudeAdapter(t, false)
	defer c.Close()
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")
	os.MkdirAll(filepath.Join(cfg, "sessions"), 0o700)
	rec, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "sessionId": "held-session"})
	os.WriteFile(filepath.Join(cfg, "sessions", fmt.Sprintf("%d.json", os.Getpid())), rec, 0o600)
	_, res := c.PrepareNativeAttach(context.Background(), "held-session", &ExecSpec{RunID: "r9", Cwd: t.TempDir()})
	if res.Attached || res.Reason != "held" || res.HolderPID != os.Getpid() {
		t.Fatalf("held session not refused: %+v", res)
	}
	if spawnCount(t, logPath) != 0 {
		t.Fatal("started a second process for a held session")
	}
	_, res = c.PrepareNativeAttach(context.Background(), "unmanaged", nil)
	if res.Attached {
		t.Fatal("an unmanaged session must be left to Claude Code")
	}
}

func processState(pid int) string {
	out, _ := exec.Command("ps", "-o", "stat=", "-p", fmt.Sprint(pid)).Output()
	return strings.TrimSpace(string(out))
}

// A tab reads the transcript, then attaches. Opening it from Agent Conductor
// pauses the CLI in between so nothing falls between the two, and the tab's
// initialize resumes it. Never paused when a tab is already attached.
func TestOpeningATabPausesUntilItAttaches(t *testing.T) {
	c, _ := fakeClaudeAdapter(t, true)
	defer c.Close()
	sink, _ := collect()
	done := make(chan struct{})
	go func() {
		c.Execute(context.Background(), ExecSpec{RunID: "r1", Cwd: t.TempDir(), Prompt: "long task"}, sink)
		close(done)
	}()
	var ls *liveSession
	for i := 0; i < 100 && ls == nil; i++ {
		time.Sleep(20 * time.Millisecond)
		ls = liveOf(c, "r1")
	}
	if !c.FreezeForAttach(ls.sessionID, 5*time.Second) {
		t.Fatal("could not pause for an opening tab")
	}
	if st := processState(ls.pid); !strings.HasPrefix(st, "T") {
		t.Fatalf("process not paused: %q", st)
	}
	tab := attachClient(t, ls)
	tab.send(t, map[string]any{"type": "control_request", "request_id": "i", "request": map[string]any{"subtype": "initialize"}})
	tab.waitFor(t, func(m map[string]any) bool { return m["type"] == "control_response" })
	time.Sleep(100 * time.Millisecond)
	if st := processState(ls.pid); strings.HasPrefix(st, "T") {
		t.Fatalf("attach did not resume the process: %q", st)
	}
	if c.FreezeForAttach(ls.sessionID, 5*time.Second) {
		t.Fatal("focusing an attached tab must never pause the run")
	}
	tab.waitFor(t, func(m map[string]any) bool { return m["type"] == "result" })
	<-done
}

func TestPauseIsBoundedWhenNoTabAttaches(t *testing.T) {
	c, _ := fakeClaudeAdapter(t, true)
	defer c.Close()
	sink, events := collect()
	done := make(chan struct{})
	go func() {
		c.Execute(context.Background(), ExecSpec{RunID: "r1", Cwd: t.TempDir(), Prompt: "long task"}, sink)
		close(done)
	}()
	var ls *liveSession
	for i := 0; i < 100 && ls == nil; i++ {
		time.Sleep(20 * time.Millisecond)
		ls = liveOf(c, "r1")
	}
	c.FreezeForAttach(ls.sessionID, 300*time.Millisecond)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a pause with no tab attaching never ended")
	}
	if ev, _ := findEvent(events(), EvTurnCompleted); ev.Status != TurnCompletedOK {
		t.Fatalf("run did not finish after the bounded pause: %+v", ev)
	}
}
