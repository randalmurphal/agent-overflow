package threadtools

import (
	"encoding/json"
	"strings"
	"time"
)

// MCPServerName is the MCP server the thread tools are served under, as a
// provider names it on a tool call.
const MCPServerName = "ao-thread-tools"

// ScheduledNextFire reads the next fire time a thread_loop, thread_watch or
// thread_remind reply carries, in Unix milliseconds, so the call's timeline
// row can say when it fires. False for any other tool, a stop or cancel
// reply, a long-running watch, which has no fire time, and a reply that is
// not one of this package's.
func ScheduledNextFire(tool, reply string) (int64, bool) {
	switch tool {
	case "thread_loop", "thread_watch", "thread_remind":
	default:
		return 0, false
	}
	// The reply is one JSON object. A client may append more after it
	// (Codex adds structured content), so only the first value is read.
	var result struct {
		NextFire string `json:"next_fire"`
	}
	if err := json.NewDecoder(strings.NewReader(reply)).Decode(&result); err != nil || result.NextFire == "" {
		return 0, false
	}
	at, err := time.Parse(time.RFC3339, result.NextFire)
	if err != nil {
		return 0, false
	}
	return at.UnixMilli(), true
}
