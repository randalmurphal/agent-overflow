package threadtools

import (
	"strconv"
	"strings"
	"testing"
)

// An interval under the floor is refused by the tool itself, and the schema
// says so: 60 seconds unless a test boot lowered it. timeout_seconds and the
// 30-day ceiling are refused here too.
func TestWakeIntervalsAreRefusedAtTheTool(t *testing.T) {
	t.Parallel()
	for _, floor := range []int{WakeFloorSeconds, 1} {
		app := newFakeApp("laptop")
		server := New(app)
		if floor != WakeFloorSeconds {
			server.SetWakeFloor(floor)
		}
		for _, tool := range server.Tools(Shape{}) {
			name, _ := tool["name"].(string)
			if name != "thread_loop" && name != "thread_watch" {
				continue
			}
			every := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)["every_seconds"].(map[string]any)
			if every["minimum"] != floor || every["maximum"] != MaxWakeIntervalSeconds {
				t.Errorf("floor %d: %s every_seconds bounds %v..%v", floor, name, every["minimum"], every["maximum"])
			}
		}
		under := `{"label":"l","note":"n","every_seconds":` + strconv.Itoa(floor-1) + `}`
		if floor > 1 {
			message := callErr(t, server, localCaller(), "thread_loop", under, CodeInvalidRequest)
			if !strings.Contains(message, "between "+strconv.Itoa(floor)+" and 2592000") {
				t.Errorf("floor %d: refusal %q", floor, message)
			}
			callErr(t, server, localCaller(), "thread_watch", `{"label":"w","command":"true","every_seconds":`+strconv.Itoa(floor-1)+`}`, CodeInvalidRequest)
		}
		call(t, server, localCaller(), "thread_loop", `{"label":"l","note":"n","every_seconds":`+strconv.Itoa(floor)+`}`)
		call(t, server, localCaller(), "thread_watch", `{"label":"w","command":"true","every_seconds":`+strconv.Itoa(floor)+`,"timeout_seconds":3600}`)
		callErr(t, server, localCaller(), "thread_loop", `{"label":"l","note":"n","every_seconds":2592001}`, CodeInvalidRequest)
		callErr(t, server, localCaller(), "thread_watch", `{"label":"w","command":"true","every_seconds":60,"timeout_seconds":3601}`, CodeInvalidRequest)
		if len(app.loops) != 1 || len(app.watches) != 1 {
			t.Errorf("floor %d: %d loops and %d watches reached the app, want one each", floor, len(app.loops), len(app.watches))
		}
	}
}
