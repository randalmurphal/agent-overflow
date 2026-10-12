package triage

import (
	"encoding/json"
	"strconv"

	"agent-overflow/internal/threadtools"
)

// stampScheduledNextFire records on a thread_loop, thread_watch or
// thread_remind row the next fire time its reply names (`next_fire_at`, Unix
// milliseconds), so the row's summary line can say when it fires without
// reading the reply's payload. Both providers stamp `meta.mcp` on an MCP
// tool row, so this reads no provider's wire. Any other row, and a failed
// call, is returned unchanged.
func stampScheduledNextFire(metaJSON, reply string, failed bool) string {
	if failed || reply == "" {
		return metaJSON
	}
	var meta struct {
		MCP struct {
			Server string `json:"server"`
			Tool   string `json:"tool"`
		} `json:"mcp"`
	}
	if json.Unmarshal([]byte(metaJSON), &meta) != nil || meta.MCP.Server != threadtools.MCPServerName {
		return metaJSON
	}
	at, ok := threadtools.ScheduledNextFire(meta.MCP.Tool, reply)
	if !ok {
		return metaJSON
	}
	return mergeItemMetaJSON(metaJSON, []byte(`{"next_fire_at":`+strconv.FormatInt(at, 10)+`}`))
}
