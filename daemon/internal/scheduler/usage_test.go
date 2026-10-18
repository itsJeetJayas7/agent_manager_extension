package scheduler

import (
	"encoding/json"
	"testing"
	"time"

	"agent-conductor/internal/model"
)

func TestUsageSummaryEmptyRecentIsJSONArray(t *testing.T) {
	for _, oldHistory := range []bool{false, true} {
		name := "new database"
		if oldHistory {
			name = "no runs in window"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			if oldHistory {
				mustPause(t, h, PauseRequest{Source: PauseSourceUser})
				_, _, err := h.conductor.EnqueueRun(EnqueueRunInput{
					Prompt: "old run", Provider: model.ProviderClaude, Cwd: t.TempDir(),
				})
				if err != nil {
					t.Fatal(err)
				}
				h.advance(8 * 24 * time.Hour)
			}
			summary, err := h.conductor.UsageSince(7 * 24 * time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if summary.Runs != 0 {
				t.Fatalf("expected no runs in window, got %d", summary.Runs)
			}
			encoded, err := json.Marshal(summary)
			if err != nil {
				t.Fatal(err)
			}
			var response map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &response); err != nil {
				t.Fatal(err)
			}
			if got := string(response["recent"]); got != "[]" {
				t.Fatalf("recent must be an empty JSON array, got %s", got)
			}
		})
	}
}
