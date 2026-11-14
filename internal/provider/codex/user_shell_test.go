package codex

import (
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestFormatUserShellCommandMatchesTheRolloutRecord(t *testing.T) {
	t.Parallel()
	// The first two wants are the text codex-cli 0.163.0 wrote to its rollout
	// for the same commands run through thread/shellCommand.
	for name, tc := range map[string]struct {
		in   provider.UserShellCommand
		want string
	}{
		"success": {
			provider.UserShellCommand{Command: "echo hello-from-user-shell", Output: "hello-from-user-shell\n", Duration: 20200 * time.Microsecond},
			"<user_shell_command>\n<command>\necho hello-from-user-shell\n</command>\n<result>\nExit code: 0\nDuration: 0.0202 seconds\nOutput:\nhello-from-user-shell\n\n</result>\n</user_shell_command>",
		},
		"failure": {
			provider.UserShellCommand{Command: "ls /nonexistent-xyz", Output: "ls: cannot access '/nonexistent-xyz': No such file or directory\n", ExitCode: 2, Duration: 13400 * time.Microsecond},
			"<user_shell_command>\n<command>\nls /nonexistent-xyz\n</command>\n<result>\nExit code: 2\nDuration: 0.0134 seconds\nOutput:\nls: cannot access '/nonexistent-xyz': No such file or directory\n\n</result>\n</user_shell_command>",
		},
		"timeout": {
			provider.UserShellCommand{Command: "sleep 9", Output: "before", ExitCode: -1, TimedOut: true, Duration: 3 * time.Second},
			"<user_shell_command>\n<command>\nsleep 9\n</command>\n<result>\nExit code: 124\nDuration: 3.0000 seconds\nOutput:\nbefore\ncommand timed out after 3s\n\n</result>\n</user_shell_command>",
		},
	} {
		if got := FormatUserShellCommand(tc.in); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.want)
		}
	}
}
