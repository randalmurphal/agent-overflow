package triage

import (
	"encoding/json"
	"strconv"

	"agent-overflow/internal/threadtools"
)

// stampScheduledNextFire records on a thread_loop, thread_watch or
// thread_remind row the next fire time its reply names (`next_fire_at`, Unix
// milliseconds), so the row's summary line can say when it fires without
// reading the reply's payload, and the row's own time (`next_fire_from`), so
// the line says the time alone when the fire is on the row's day and the date
// as well when it is not, the same however late the row is read. Both
// providers stamp `meta.mcp` on an MCP tool row, so this reads no provider's
// wire. Any other row, and a failed call, is returned unchanged.
func stampScheduledNextFire(metaJSON, reply string, failed bool, rowAt int64) string {
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
	patch := `{"next_fire_at":` + strconv.FormatInt(at, 10)
	if rowAt > 0 {
		patch += `,"next_fire_from":` + strconv.FormatInt(rowAt, 10)
	}
	return mergeItemMetaJSON(metaJSON, []byte(patch+`}`))
}
