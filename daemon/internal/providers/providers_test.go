package providers

import (
	"context"
	"testing"
	"time"

	"agent-conductor/internal/model"
)

func TestFakeHappyPath(t *testing.T) {
	f := NewFake()
	var events []Event
	sink := func(e Event) { events = append(events, e) }
	err := f.Execute(context.Background(), ExecSpec{RunID: "r1", Prompt: "do it"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	var final *Event
	for i := range events {
		if events[i].Kind == EvTurnCompleted {
			final = &events[i]
		}
	}
	if final == nil || final.Status != TurnCompletedOK {
		t.Fatalf("no completed terminal: %+v", final)
	}
	if out, _ := model.ParseResultMarker(final.FinalMsg); out != "COMPLETE" {
		t.Fatalf("marker missing: %q", final.FinalMsg)
	}
	if final.SessionID != "fake-sess-r1" {
		t.Fatalf("session id %q", final.SessionID)
	}
}

func TestFakeRateLimitScenarios(t *testing.T) {
	cases := []struct {
		name   string
		script Script
		class  model.ErrorClass
	}{
		{"before start", Script{Match: "*", FailBefore: true, RateLimit: true}, model.ErrRateLimit},
		{"partial work", Script{Match: "*", RateLimit: true, PartialWork: true}, model.ErrRateLimit},
		{"generic failure", Script{Match: "*", FailBefore: true, Class: model.ErrAuth}, model.ErrAuth},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := NewFake()
			f.SetScripts([]Script{c.script})
			var final Event
			f.Execute(context.Background(), ExecSpec{RunID: "r", Prompt: "x"}, func(e Event) {
				if e.Kind == EvTurnCompleted {
					final = e
				}
			})
			if final.Status != TurnFailed || final.Error == nil || final.Error.Class != c.class {
				t.Fatalf("got %+v", final)
			}
		})
	}
}

func TestFakeSameSessionOnContinue(t *testing.T) {
	f := NewFake()
	f.SetScripts([]Script{{Match: "*", RateLimit: true, PartialWork: true}})
	var sess string
	f.Execute(context.Background(), ExecSpec{RunID: "r1", Prompt: "x"}, func(e Event) {
		if e.Kind == EvSessionStarted {
			sess = e.SessionID
		}
	})
	var sess2 string
	f.Execute(context.Background(), ExecSpec{RunID: "r1", Prompt: "continue", SessionID: sess, Continue: true}, func(e Event) {
		if e.Kind == EvSessionStarted {
			sess2 = e.SessionID
		}
	})
	if sess != sess2 || sess == "" {
		t.Fatalf("continuation must reuse session: %q vs %q", sess, sess2)
	}
}

func TestFakeCancel(t *testing.T) {
	f := NewFake()
	f.SetScripts([]Script{{Match: "*", Duration: 5 * time.Second}})
	done := make(chan Event, 1)
	go f.Execute(context.Background(), ExecSpec{RunID: "rc", Prompt: "x"}, func(e Event) {
		if e.Kind == EvTurnCompleted {
			done <- e
		}
	})
	time.Sleep(50 * time.Millisecond)
	f.Cancel("rc")
	select {
	case e := <-done:
		if e.Status != TurnInterrupted {
			t.Fatalf("expected interrupted, got %s", e.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not terminate run")
	}
}

func TestHookBusFanout(t *testing.T) {
	b := NewHookBus()
	ch := b.Subscribe()
	b.Publish(HookPayload{Provider: "claude", Event: "Stop"})
	select {
	case p := <-ch:
		if p.Event != "Stop" {
			t.Fatal("wrong event")
		}
	default:
		t.Fatal("event not delivered")
	}
	b.Unsubscribe(ch)
	b.Publish(HookPayload{Event: "Stop"}) // must not panic on closed channel path
}

func TestCodexRateLimitParsing(t *testing.T) {
	payload := map[string]any{
		"rateLimits": map[string]any{
			"primary":   map[string]any{"usedPercent": 100.0, "resetsAt": "2026-01-01T05:00:00Z", "windowMinutes": 300.0, "isAtLimit": true},
			"secondary": map[string]any{"usedPercent": 42.0, "resetsAt": "2026-01-05T00:00:00Z", "windowMinutes": 10080.0},
		},
	}
	ws := codexParseRateLimits(payload)
	if len(ws) != 2 {
		t.Fatalf("want 2 windows, got %d", len(ws))
	}
	var five, week QuotaWindow
	for _, w := range ws {
		if w.ID == "primary" {
			five = w
		} else {
			week = w
		}
	}
	if !five.Exhausted || !five.HasReset || five.Confidence != "exact" {
		t.Fatalf("primary wrong: %+v", five)
	}
	if week.Exhausted || week.UsedPercent != 42 {
		t.Fatalf("secondary wrong: %+v", week)
	}
}
