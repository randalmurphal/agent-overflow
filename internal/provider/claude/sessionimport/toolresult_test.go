package sessionimport

import "testing"

func TestBackgroundAckTaskID_RejectsProseAndPartialMatches(t *testing.T) {
	for _, text := range []string{
		"",
		"Permission to use Bash has been denied",
		"See: Command running in background with ID: abc.",            // not a prefix
		"Command running in background with ID: ",                     // empty id
		"Command failed\nCommand running in background with ID: abc.", // marker off the first line
		"Command did not complete within its 300s timeout (ID: ).",    // empty id
	} {
		for _, requested := range []bool{false, true} {
			if id, ok := BackgroundAckTaskID(text, requested); ok {
				t.Errorf("%q (requested=%v) must not classify (got id %q)", text, requested, id)
			}
		}
	}
	for _, text := range []string{
		"Command running in background with ID: task-bg_1.\r\n",
		"Command exceeded the assistant-mode blocking budget (30s) and was moved to the background with ID: task-bg_1. Output…",
		"Command was manually backgrounded by user with ID: task-bg_1.",
		"Command did not complete within its 300s timeout and was moved to the background (ID: task-bg_1). Output is being written to: /tmp/tasks/task-bg_1.output.",
	} {
		if id, ok := BackgroundAckTaskID(text, true); !ok || id != "task-bg_1" {
			t.Fatalf("%q: got (%q, %v), want (task-bg_1, true)", text, id, ok)
		}
	}
}

// A foreground command the CLI moved to the background carries no
// run_in_background request, and its ack is the only statement of the
// move on a sidechain. The "running in background" ack answers only a
// requested launch.
func TestBackgroundAckTaskID_MovedAcksNeedNoRequest(t *testing.T) {
	for _, text := range []string{
		"Command exceeded the assistant-mode blocking budget (30s) and was moved to the background with ID: task-bg_1. Output…",
		"Command was manually backgrounded by user with ID: task-bg_1.",
		"Command did not complete within its 300s timeout and was moved to the background (ID: task-bg_1). Output is being written to: /tmp/tasks/task-bg_1.output.",
	} {
		if id, ok := BackgroundAckTaskID(text, false); !ok || id != "task-bg_1" {
			t.Fatalf("%q: got (%q, %v), want (task-bg_1, true)", text, id, ok)
		}
	}
	if id, ok := BackgroundAckTaskID("Command running in background with ID: task-bg_1.", false); ok {
		t.Fatalf("an unrequested launch must not read the running-in-background ack (got %q)", id)
	}
}

// Every observed Monitor ack wording names its task first; a refusal or
// a result that only quotes the ack does not classify.
func TestMonitorAckTaskID(t *testing.T) {
	for _, text := range []string{
		"Monitor started (task bs7ev9m4y, timeout 3600000ms). You will be notified on each event.",
		"Monitor started (task bs7ev9m4y, expires in 30m unless the source ends first; you get one notice at expiry). Keep working.",
		"Monitor started (task bs7ev9m4y, persistent — runs until TaskStop or session end). You will be notified on each event.",
	} {
		if id, ok := MonitorAckTaskID(text); !ok || id != "bs7ev9m4y" {
			t.Fatalf("%q: got (%q, %v), want (bs7ev9m4y, true)", text, id, ok)
		}
	}
	for _, text := range []string{
		"",
		"InputValidationError: command is required",
		"See: Monitor started (task bs7ev9m4y, timeout 3600000ms).", // not a prefix
		"Monitor started (task , timeout 3600000ms).",               // empty id
	} {
		if id, ok := MonitorAckTaskID(text); ok {
			t.Errorf("%q must not classify (got id %q)", text, id)
		}
	}
}
