package threadtools

import (
	"testing"
	"time"
)

func TestScheduledNextFireReadsTheSchedulingReplies(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 19, 12, 10, 0, 0, time.UTC).UnixMilli()
	for _, tc := range []struct {
		name, tool, reply string
		want              int64
	}{
		{"a loop", "thread_loop", `{"kind":"loop","label":"board","next_fire":"2026-09-19T12:10:00Z"}`, at},
		{"a watch, with Codex's structured content after it", "thread_watch",
			`{"kind":"watch","next_fire":"2026-09-19T12:10:00Z"}` + "\n\nStructured content:\n{}", at},
		{"a reminder", "thread_remind", `{"token":"tok","next_fire":"2026-09-19T12:10:00Z"}`, at},
		{"a long-running watch", "thread_watch", `{"kind":"watch","long_running":true}`, 0},
		{"a stop", "thread_loop", `{"label":"board","state":"cancelled"}`, 0},
		{"another tool", "thread_status", `{"next_fire":"2026-09-19T12:10:00Z"}`, 0},
		{"an error text", "thread_loop", `every_seconds must be at least 60`, 0},
		{"a malformed time", "thread_loop", `{"next_fire":"soon"}`, 0},
	} {
		got, ok := ScheduledNextFire(tc.tool, tc.reply)
		if got != tc.want || ok != (tc.want != 0) {
			t.Errorf("%s: ScheduledNextFire = %d, %v; want %d", tc.name, got, ok, tc.want)
		}
	}
}
