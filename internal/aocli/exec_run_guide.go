package aocli

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

// `agent-overflow run guide <run-id> "<text>"` — steering a run without parking
// it.
//
// It stays outside the runControl family for the same reason `run amend` does:
// its answer is not "where is the run now". The run has not moved — that is the
// point — and what the caller needs told is that the entry is waiting and when
// the run will read it, which only the app can say.
var runGuideCommand = execCommand{
	name:  "agent-overflow run guide",
	usage: runGuideUsage,
	bind: func(flags *flag.FlagSet) func(*client, []string, io.Writer) (int, error) {
		jsonOutput := flags.Bool("json", false, "write the app's result as JSON")
		return func(c *client, args []string, stdout io.Writer) (int, error) {
			// The text is a positional for the reason `run answer`'s is: it is the
			// point of the command. `requireArgs` refuses a missing one, and the app
			// refuses a blank one — guidance that says nothing steers nothing.
			if err := requireArgs("agent-overflow run guide", args, 2, "a run id and the guidance text"); err != nil {
				return exitError, err
			}
			var guided guideView
			raw, err := c.callInto(&guided, "WorkflowAgentGuideRun",
				map[string]any{"itemId": args[0], "text": args[1]})
			if err != nil {
				return exitError, err
			}
			return exitOK, render(stdout, *jsonOutput, raw, guided.block())
		}
	},
}

// guideView mirrors only the fields the human block prints.
type guideView struct {
	ItemID         string `json:"itemId"`
	Pending        int    `json:"pending"`
	MaxPending     int    `json:"maxPending"`
	By             string `json:"by"`
	State          string `json:"state"`
	Reason         string `json:"reason"`
	PhaseID        string `json:"phaseId"`
	DeliversNote   string `json:"deliversNote"`
	CallerNote     string `json:"callerNote"`
	QuarantineNote string `json:"quarantineNote"`
}

// block prints what is now waiting and the app's own sentence about when it is
// read. The guidance text is not echoed: the caller just typed it, and a CLI
// that reprinted it would be spending an agent's context on its own input.
func (v guideView) block() string {
	var block strings.Builder
	row := []string{
		"run=" + v.ItemID,
		fmt.Sprintf("pending=%d/%d", v.Pending, v.MaxPending),
		"by=" + v.By,
		"state=" + v.State,
	}
	if v.Reason != "" {
		row = append(row, "reason="+v.Reason)
	}
	if v.PhaseID != "" {
		row = append(row, "phase="+v.PhaseID)
	}
	fmt.Fprint(&block, fields(row...))
	if v.DeliversNote != "" {
		fmt.Fprintf(&block, "\nwhen: %s", v.DeliversNote)
	}
	// The quarantine goes above the caller note and reads as a warning, because
	// it is the one line here about something the caller LOST rather than about
	// what they just did.
	if v.QuarantineNote != "" {
		fmt.Fprintf(&block, "\nwarning: %s", v.QuarantineNote)
	}
	if v.CallerNote != "" {
		fmt.Fprintf(&block, "\nnote: %s", v.CallerNote)
	}
	return block.String()
}
