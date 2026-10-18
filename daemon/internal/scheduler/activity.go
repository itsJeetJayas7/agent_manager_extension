package scheduler

import (
	"strings"
	"sync"
	"syscall"
	"time"

	"agent-conductor/internal/providers"
)

// Activity is a bounded, in-memory observation of this daemon's execution.
// It deliberately distinguishes worker/process liveness from provider output.
// A quiet provider is not evidence that a process is dead (or making progress).
type RunActivity struct {
	ObservedAt      time.Time       `json:"observedAt"`
	ExecutionActive bool            `json:"executionActive"`
	ProcessID       int             `json:"processId,omitempty"`
	ProcessAlive    *bool           `json:"processAlive,omitempty"`
	LastEventAt     *time.Time      `json:"lastEventAt,omitempty"`
	Entries         []ActivityEntry `json:"entries"`
	// Live is the daemon-owned provider process a native UI can attach to,
	// and how many native views are attached to it right now.
	Live *providers.LiveStatus `json:"live,omitempty"`
}

type ActivityEntry struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	Text string    `json:"text"`
}

type activityRecord struct {
	RunActivity
	progress bool
	model    string
}

type activityMonitor struct {
	sync.Mutex
	runs map[string]*activityRecord
}

func (c *Conductor) beginActivity(runID string) {
	c.activity.Lock()
	defer c.activity.Unlock()
	if c.activity.runs == nil {
		c.activity.runs = make(map[string]*activityRecord)
	}
	// Retain recent finished runs across panel reloads, without growing forever.
	if len(c.activity.runs) >= 100 {
		oldest := ""
		var at time.Time
		for id, r := range c.activity.runs {
			if !r.ExecutionActive && (oldest == "" || r.ObservedAt.Before(at)) {
				oldest, at = id, r.ObservedAt
			}
		}
		delete(c.activity.runs, oldest)
	}
	c.activity.runs[runID] = &activityRecord{RunActivity: RunActivity{
		ObservedAt: c.Now(), ExecutionActive: true,
		Entries: []ActivityEntry{{At: c.Now(), Kind: "status", Text: "Starting provider"}},
	}}
}

func (c *Conductor) endActivity(runID string) {
	c.activity.Lock()
	defer c.activity.Unlock()
	if r := c.activity.runs[runID]; r != nil {
		r.ExecutionActive = false
		r.ObservedAt = c.Now()
	}
}

// recordActivity returns whether a delta also needs a durable model/progress
// update. Streaming tokens must not each trigger SQLite and a full UI fan-out.
func (c *Conductor) recordActivity(runID string, ev providers.Event) bool {
	c.activity.Lock()
	defer c.activity.Unlock()
	r := c.activity.runs[runID]
	if r == nil {
		return true
	}
	changed := (ev.Progress && !r.progress) || (ev.Model != "" && ev.Model != r.model)
	// Only deltas take the durable progress path in onProviderEvent. An
	// informational tool-result event must not suppress the first such write.
	if ev.Kind == providers.EvMessageDelta {
		r.progress = r.progress || ev.Progress
	}
	if ev.Model != "" {
		r.model = ev.Model
	}
	now := c.Now()
	r.ObservedAt = now
	r.LastEventAt = &now
	kind, text := "status", ""
	switch ev.Kind {
	case providers.EvProcessStarted:
		r.ProcessID = ev.ProcessID
		text = "Provider process started"
	case providers.EvSessionStarted:
		text = "Session ready"
	case providers.EvTurnStarted:
		text = "Turn started"
	case providers.EvMessageDelta:
		kind, text = "message", ev.Text
		if strings.HasPrefix(text, "tool:") {
			kind, text = "tool", "Using "+strings.TrimPrefix(text, "tool:")
		}
	case providers.EvActivity:
		text = ev.Text
	case providers.EvTurnCompleted:
		text = "Turn " + string(ev.Status)
		if ev.Error != nil {
			text += ": " + ev.Error.Message
		}
	case providers.EvProcessExited:
		text = "Provider process exited"
	}
	if text == "" {
		return changed
	}
	n := len(r.Entries)
	if n > 0 && r.Entries[n-1].Kind == kind && (kind == "message" || r.Entries[n-1].Text == text) {
		entry := &r.Entries[n-1]
		entry.At = now
		if kind == "message" {
			entry.Text += text
		}
		// Keep a bounded tail even for an enormous streamed response.
		if text := []rune(entry.Text); len(text) > 16000 {
			entry.Text = string(text[len(text)-16000:])
		}
	} else {
		r.Entries = append(r.Entries, ActivityEntry{At: now, Kind: kind, Text: truncate(text, 16000)})
		if len(r.Entries) > 100 {
			r.Entries = append([]ActivityEntry(nil), r.Entries[len(r.Entries)-100:]...)
		}
	}
	return changed
}

func (c *Conductor) RunActivity(runID string) RunActivity {
	c.activity.Lock()
	out := RunActivity{Entries: []ActivityEntry{}}
	if r := c.activity.runs[runID]; r != nil {
		out = r.RunActivity
		out.Entries = append([]ActivityEntry(nil), r.Entries...)
	}
	c.activity.Unlock()
	out.ObservedAt = c.Now()
	if c.store == nil || c.registry == nil {
		// fall through: no provider to ask
	} else if r, err := c.store.GetRun(runID); err == nil && r != nil {
		if a, ok := c.registry.Get(r.Provider); ok {
			if lv, ok := a.(providers.LiveViewer); ok {
				if st, ok := lv.LiveStatus(runID); ok {
					out.Live = &st
				}
			}
		}
	}
	if out.ProcessID > 0 {
		err := syscall.Kill(out.ProcessID, 0)
		alive := out.ExecutionActive && (err == nil || err == syscall.EPERM)
		out.ProcessAlive = &alive
	}
	// Redact after assembling chunks, so secrets split across deltas are still
	// removed. Never persist raw provider output in the run database.
	for i := range out.Entries {
		out.Entries[i].Text = truncate(c.log.Redactor().Redact(out.Entries[i].Text), 8000)
	}
	return out
}

// NativeViewChanged tells connected UIs that a provider's own UI attached to
// or detached from a run's process.
func (c *Conductor) NativeViewChanged(runID string) {
	if runID != "" {
		c.emit("nativeViewChanged", map[string]any{"runId": runID})
	}
}
