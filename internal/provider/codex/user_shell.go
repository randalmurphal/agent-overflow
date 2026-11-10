package codex

import (
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// userShellTimeoutExitCode is the exit code Codex itself reports for a user
// shell command its timeout ended.
const userShellTimeoutExitCode = 124

// FormatUserShellCommand renders a command the person ran as the user-message
// text Codex records for its own `thread/shellCommand` (verified against
// codex-cli 0.163.0), so the model reads a command this app ran the same way.
func FormatUserShellCommand(command provider.UserShellCommand) string {
	exitCode := command.ExitCode
	output := command.Output
	if command.TimedOut {
		exitCode = userShellTimeoutExitCode
		if output != "" && !strings.HasSuffix(output, "\n") {
			output += "\n"
		}
		output += fmt.Sprintf("command timed out after %s\n", command.Duration.Round(time.Second))
	}
	return fmt.Sprintf(
		"<user_shell_command>\n<command>\n%s\n</command>\n<result>\nExit code: %d\nDuration: %.4f seconds\nOutput:\n%s\n</result>\n</user_shell_command>",
		command.Command, exitCode, command.Duration.Seconds(), output,
	)
}
