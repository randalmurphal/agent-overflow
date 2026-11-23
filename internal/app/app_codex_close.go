package app

import (
	"fmt"
	"sort"
	"strings"

	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/triage"
)

// codexCommandNames is how an error names Codex commands: each command's
// summary, quoted, sorted.
func codexCommandNames(commands []triage.CodexCommand) []string {
	names := make([]string, 0, len(commands))
	for _, command := range commands {
		names = append(names, fmt.Sprintf("%q", command.Summary))
	}
	sort.Strings(names)
	return names
}

// codexUnclosedThreadsError is the error End Session leaves on a Codex
// thread when Codex did not confirm that it unloaded some of the session's
// threads (codex.ThreadsNotClosed): it names each such thread and the
// commands it was running when the session began to end, which may still be
// running. commands are the thread's live commands as triage read them
// then: the session's end drops them and every frame the close dispatches,
// so a command started during the close is not known, and the error says
// so. It reports false when Codex confirmed every unload.
func codexUnclosedThreadsError(closeErr error, commands []triage.CodexCommand) (string, bool) {
	notClosed := codex.ThreadsNotClosed(closeErr)
	if len(notClosed) == 0 {
		return "", false
	}
	byScope := make(map[string][]triage.CodexCommand, len(notClosed))
	for _, command := range commands {
		byScope[command.ScopeID] = append(byScope[command.ScopeID], command)
	}
	var b strings.Builder
	b.WriteString("Ending the session: Codex did not confirm that it closed these threads, so their commands may still be running. Each thread lists the commands it was running when the session began to end; a command started after that is not listed:")
	causes := make([]string, 0, len(notClosed))
	for _, thread := range notClosed {
		name := "an agent"
		if thread.Root {
			name = "this thread"
		} else if thread.Agent != "" {
			name = "agent " + thread.Agent
		}
		running := "no command was running when the session began to end"
		if names := codexCommandNames(byScope[thread.ThreadID]); len(names) > 0 {
			running = strings.Join(names, ", ")
		}
		fmt.Fprintf(&b, "\n- %s: %s", name, running)
		causes = append(causes, thread.Err.Error())
	}
	fmt.Fprintf(&b, "\nCodex: %s", strings.Join(causes, "; "))
	return b.String(), true
}
