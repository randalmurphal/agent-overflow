package transport

import "testing"

func TestEventVisibleToOrigin(t *testing.T) {
	for _, channel := range []string{
		"git:status",
		"provider:approval",
		"provider:status",
		"provider:queue_flushed",
		"provider:queue_state_changed",
		"provider:background_task_state",
		"provider:user_input",
		"provider:account",
		"terminal:exit",
		"terminal:output",
	} {
		if eventVisibleToOrigin(channel, false) {
			t.Fatalf("%s visible to non-loopback peer", channel)
		}
		if !eventVisibleToOrigin(channel, true) {
			t.Fatalf("%s hidden from loopback peer", channel)
		}
	}
	for _, channel := range []string{
		"provider:item_event",
		"provider:usage",
		"provider:session_died",
	} {
		if !eventVisibleToOrigin(channel, false) {
			t.Fatalf("remote-safe event %s hidden from non-loopback peer", channel)
		}
		if !eventVisibleToOrigin(channel, true) {
			t.Fatalf("remote-safe event %s hidden from loopback peer", channel)
		}
	}
}
