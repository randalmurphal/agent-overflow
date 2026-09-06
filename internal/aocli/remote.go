package aocli

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"agent-overflow/internal/gitapp"
	"agent-overflow/internal/remotejobs"
	"github.com/google/uuid"
)

func remoteCommand(args []string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		if err := writeOutput(stdout, remoteUsage); err != nil {
			return operationalError(stderr, err)
		}
		return exitOK
	}
	verb := args[0]
	if verb != "list" && verb != "run" && verb != "status" && verb != "cancel" {
		return operationalError(stderr, fmt.Errorf("unknown remote command %q", verb))
	}
	command := execCommand{name: "remote " + verb, usage: remoteUsage, bind: func(flags *flag.FlagSet) func(*client, []string, io.Writer) (int, error) {
		computer := flags.String("computer", "", "destination computer UUID")
		project := flags.String("project", "", "destination project UUID")
		workspace := flags.String("workspace", "", "registered worktree path (default: project root)")
		id := flags.String("id", "", "stable request UUID; reuse after a lost reply")
		timeout := flags.Int("timeout", 3600, "command time limit in seconds")
		return func(c *client, positionals []string, out io.Writer) (int, error) {
			method := "AgentRemoteComputers"
			var params []any
			switch verb {
			case "list":
				if len(positionals) != 0 {
					return exitError, errors.New("remote list takes no arguments")
				}
			case "run":
				if *computer == "" || *project == "" || len(positionals) == 0 {
					return exitError, errors.New("remote run needs --computer, --project, and -- followed by a command")
				}
				if *id == "" {
					*id = uuid.NewString()
				}
				// Publish retry identity before posting. If the acknowledgement is
				// lost, this is enough to inspect/retry without creating new work.
				if _, err := fmt.Fprintf(stderr, "Remote request %s on %s (reuse --id after a lost reply)\n", *id, *computer); err != nil {
					return exitError, err
				}
				method = "AgentRemoteStart"
				params = []any{struct {
					ComputerID string              `json:"computerId"`
					Workspace  gitapp.WorkspaceRef `json:"workspace"`
					Request    remotejobs.Request  `json:"request"`
				}{*computer, gitapp.WorkspaceRef{ProjectID: *project, WorkspacePath: *workspace}, remotejobs.Request{ID: *id, Argv: positionals, TimeoutSeconds: *timeout}}}
			case "status", "cancel":
				if *computer == "" || len(positionals) != 1 {
					return exitError, errors.New("remote status/cancel needs --computer and one request UUID")
				}
				method = "AgentRemoteStatus"
				if verb == "cancel" {
					method = "AgentRemoteCancel"
				}
				params = []any{*computer, positionals[0]}
			}
			raw, err := c.call(method, params...)
			if err != nil {
				return exitError, err
			}
			if _, err := fmt.Fprintln(out, string(raw)); err != nil {
				return exitError, err
			}
			return exitOK, nil
		}
	}}
	return command.run(args[1:], lookupEnv, stdout, stderr)
}
