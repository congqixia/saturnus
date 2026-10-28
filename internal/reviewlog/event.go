// Package reviewlog records review lifecycle events with correlation fields.
package reviewlog

import (
	"fmt"
	"log"
	"strings"
)

// Event deliberately contains no command argv, prompt, instruction or tool
// output: those can contain callback credentials and belong in the transcript.
type Event struct {
	Name, Component, Kind                 string
	ReviewID, RunID, ReactionID, WorkerID string
	Repo                                  string
	PRNumber                              int
	Status, Error                         string
	Tool, LogPath                         string
}

func (e Event) String() string {
	var b strings.Builder
	for _, field := range []struct{ key, value string }{
		{"event", e.Name}, {"component", e.Component}, {"kind", e.Kind},
		{"review_id", e.ReviewID}, {"run_id", e.RunID}, {"reaction_id", e.ReactionID},
		{"worker_id", e.WorkerID}, {"repo", e.Repo}, {"status", e.Status},
		{"tool", e.Tool}, {"log_path", e.LogPath}, {"error", e.Error},
	} {
		if field.value != "" {
			fmt.Fprintf(&b, " %s=%q", field.key, field.value)
		}
	}
	if e.PRNumber > 0 {
		fmt.Fprintf(&b, " pr_number=%d", e.PRNumber)
	}
	return "review_task" + b.String()
}

func (e Event) Log() { log.Print(e.String()) }
