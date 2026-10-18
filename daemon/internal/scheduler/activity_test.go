package scheduler

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"agent-conductor/internal/logging"
	"agent-conductor/internal/providers"
)

func activityConductor() *Conductor {
	c := &Conductor{log: logging.NewNop()}
	c.SetClock(time.Now)
	return c
}

func TestActivityDistinguishesProcessLivenessFromProgress(t *testing.T) {
	c := activityConductor()
	c.beginActivity("r")
	c.recordActivity("r", providers.Event{Kind: providers.EvProcessStarted, ProcessID: os.Getpid()})
	a := c.RunActivity("r")
	if !a.ExecutionActive || a.ProcessAlive == nil || !*a.ProcessAlive {
		t.Fatalf("process evidence missing: %+v", a)
	}
	last := *a.LastEventAt
	c.SetClock(func() time.Time { return last.Add(time.Minute) })
	a = c.RunActivity("r")
	if !a.LastEventAt.Equal(last) || !a.ObservedAt.After(last) {
		t.Fatal("poll fabricated provider progress")
	}
	c.endActivity("r")
	a = c.RunActivity("r")
	if a.ExecutionActive || *a.ProcessAlive {
		t.Fatal("completed worker reported alive")
	}
	if a = activityConductor().RunActivity("r"); a.ExecutionActive || a.LastEventAt != nil || len(a.Entries) != 0 {
		t.Fatal("new daemon fabricated execution evidence")
	}
}

func TestActivityCoalescesAndRedactsSplitTokens(t *testing.T) {
	c := activityConductor()
	c.log.Redactor().Add("secretpassword")
	c.beginActivity("r")
	c.recordActivity("r", providers.Event{Kind: providers.EvActivity, Progress: true})
	for i, text := range []string{"Output: secret", "password"} {
		changed := c.recordActivity("r", providers.Event{Kind: providers.EvMessageDelta, Text: text, Progress: true})
		if changed != (i == 0) {
			t.Fatal("every token requested a durable update")
		}
	}
	a := c.RunActivity("r")
	if len(a.Entries) != 2 || strings.Contains(a.Entries[1].Text, "secretpassword") || !strings.Contains(a.Entries[1].Text, "REDACTED") {
		t.Fatalf("split secret leaked: %+v", a)
	}
	a.Entries[1].Text = "mutated"
	if c.RunActivity("r").Entries[1].Text == "mutated" {
		t.Fatal("caller mutated the monitor")
	}
	c.recordActivity("r", providers.Event{Kind: providers.EvMessageDelta, Model: "actual-model"})
	if c.recordActivity("r", providers.Event{Kind: providers.EvMessageDelta, Model: "actual-model"}) {
		t.Fatal("repeated model requested update")
	}
}

func TestActivityBoundsMemoryAndResetsOnContinuation(t *testing.T) {
	c := activityConductor()
	for n := 0; n < 110; n++ {
		id := fmt.Sprint(n)
		c.beginActivity(id)
		for i := 0; i < 200; i++ {
			c.recordActivity(id, providers.Event{Kind: providers.EvActivity, Text: fmt.Sprint(i)})
		}
		if len(c.RunActivity(id).Entries) != 100 {
			t.Fatal("unbounded activity history")
		}
		c.endActivity(id)
	}
	if len(c.activity.runs) != 100 {
		t.Fatal("unbounded retained runs")
	}
	c.beginActivity("109")
	a := c.RunActivity("109")
	if len(a.Entries) != 1 || a.ProcessID != 0 || a.LastEventAt != nil {
		t.Fatal("continuation retained old process evidence")
	}
}
