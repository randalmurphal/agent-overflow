package claude

import (
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

var userShellOutputEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// FormatUserShellCommand renders a command the person ran as the user-message
// text Claude Code's own `!` input mode records (verified against 2.1.321):
// the command in <bash-input>, then the merged output in <bash-stdout> for a
// command that succeeded and in <bash-stderr> for one that failed, with the
// output XML-escaped and the command verbatim. The CLI records no exit code;
// the trailing line that names one is this app's addition.
func FormatUserShellCommand(command provider.UserShellCommand) string {
	output := userShellOutputEscaper.Replace(command.Output)
	var out strings.Builder
	out.Grow(len(command.Command) + len(output) + 96)
	out.WriteString("<bash-input>")
	out.WriteString(command.Command)
	out.WriteString("</bash-input>\n")
	if command.ExitCode == 0 && !command.TimedOut {
		out.WriteString("<bash-stdout>")
		out.WriteString(strings.TrimRight(output, "\n"))
		out.WriteString("</bash-stdout><bash-stderr></bash-stderr>")
		return out.String()
	}
	out.WriteString("<bash-stdout></bash-stdout><bash-stderr>")
	out.WriteString(output)
	if output != "" && !strings.HasSuffix(output, "\n") {
		out.WriteByte('\n')
	}
	switch {
	case command.TimedOut:
		fmt.Fprintf(&out, "Command timed out after %s", command.Duration.Round(time.Second))
	case command.ExitCode < 0:
		out.WriteString("Command was ended by a signal")
	default:
		fmt.Fprintf(&out, "Exit code %d", command.ExitCode)
	}
	out.WriteString("</bash-stderr>")
	return out.String()
}
