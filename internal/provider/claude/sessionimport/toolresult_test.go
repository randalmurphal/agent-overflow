package sessionimport

import "testing"

func TestBackgroundAckTaskID_RejectsProseAndPartialMatches(t *testing.T) {
	for _, text := range []string{
		"",
		"Permission to use Bash has been denied",
		"See: Command running in background with ID: abc.",            // not a prefix
		"Command running in background with ID: ",                     // empty id
		"Command failed\nCommand running in background with ID: abc.", // marker off the first line
	} {
		if id, ok := BackgroundAckTaskID(text); ok {
			t.Errorf("%q must not classify (got id %q)", text, id)
		}
	}
	for _, text := range []string{
		"Command running in background with ID: task-bg_1.\r\n",
		"Command exceeded the assistant-mode blocking budget (30s) and was moved to the background with ID: task-bg_1. Output…",
		"Command was manually backgrounded by user with ID: task-bg_1.",
	} {
		if id, ok := BackgroundAckTaskID(text); !ok || id != "task-bg_1" {
			t.Fatalf("%q: got (%q, %v), want (task-bg_1, true)", text, id, ok)
		}
	}
}
