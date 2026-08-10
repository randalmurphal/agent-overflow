package aocli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
)

// `agent-overflow run amend --seed k=v` — changing a resting run's inputs.
//
// It is the seed half of the repair `--refresh-def` is for a prompt (D50). A
// campaign whose retry budget was seeded one lap too low had exactly two moves
// before this verb: live with it, or cancel and respawn — which on a live
// campaign cost twenty million tokens to buy back a number.
//
// It stays outside the runControl family for the same reason `run resolve`
// does: its answer is not "where is the run now". The run has not moved; what
// the caller needs told is which values changed and WHEN the run will read
// them, and only the app can say the second one.
var runAmendCommand = execCommand{
	name:  "agent-overflow run amend",
	usage: runAmendUsage,
	bind: func(flags *flag.FlagSet) func(*client, []string, io.Writer) (int, error) {
		jsonOutput := flags.Bool("json", false, "write the app's result as JSON")
		seeds := &seedFlag{}
		flags.Var(seeds, "seed", "change one declared input as key=value (repeatable; JSON values are parsed)")
		return func(c *client, args []string, stdout io.Writer) (int, error) {
			if err := requireArgs("agent-overflow run amend", args, 1, "exactly one run id"); err != nil {
				return exitError, err
			}
			encoded, err := seeds.encode()
			if err != nil {
				return exitError, err
			}
			if len(encoded) == 0 {
				return exitError, usageError("agent-overflow run amend",
					"expected at least one --seed key=value to change")
			}
			var amendment amendView
			raw, err := c.callInto(&amendment, "WorkflowAgentAmendSeeds",
				map[string]any{"itemId": args[0], "seeds": json.RawMessage(encoded)})
			if err != nil {
				return exitError, err
			}
			return exitOK, render(stdout, *jsonOutput, raw, amendment.block())
		}
	},
}

// amendView mirrors only the fields the human block prints.
type amendView struct {
	ItemID      string                     `json:"itemId"`
	Names       []string                   `json:"names"`
	Seeds       map[string]json.RawMessage `json:"seeds"`
	Effect      string                     `json:"effect"`
	AppliesNote string                     `json:"appliesNote"`
	CallerNote  string                     `json:"callerNote"`
}

// block prints what changed, the run's whole seed object after the change, and
// the app's own sentences about when it takes effect. The seeds print exactly
// as `run status` prints them — one `seed name=value` line each — because they
// are the same fact and a reader should not have to learn two shapes for it.
func (v amendView) block() string {
	var block strings.Builder
	fmt.Fprint(&block, fields(
		"run="+v.ItemID,
		"amended="+strings.Join(v.Names, ","),
		"effect="+v.Effect,
	))
	writeSeedLines(&block, v.Seeds)
	if v.AppliesNote != "" {
		fmt.Fprintf(&block, "\nwhen: %s", v.AppliesNote)
	}
	if v.CallerNote != "" {
		fmt.Fprintf(&block, "\nnote: %s", v.CallerNote)
	}
	return block.String()
}
