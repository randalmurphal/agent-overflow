package claude

import (
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestFormatUserShellCommandMatchesTheCLIRecord(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		in   provider.UserShellCommand
		want string
	}{
		"success trims the trailing newline and escapes output only": {
			provider.UserShellCommand{Command: "echo '<a&b>'", Output: "<a&b>\nwarn\n"},
			"<bash-input>echo '<a&b>'</bash-input>\n<bash-stdout>&lt;a&amp;b&gt;\nwarn</bash-stdout><bash-stderr></bash-stderr>",
		},
		"empty success": {
			provider.UserShellCommand{Command: "true"},
			"<bash-input>true</bash-input>\n<bash-stdout></bash-stdout><bash-stderr></bash-stderr>",
		},
		"failure puts the output and exit code in stderr": {
			provider.UserShellCommand{Command: "ls /x", Output: "ls: cannot access '/x'\n", ExitCode: 2},
			"<bash-input>ls /x</bash-input>\n<bash-stdout></bash-stdout><bash-stderr>ls: cannot access '/x'\nExit code 2</bash-stderr>",
		},
		"failure with no output": {
			provider.UserShellCommand{Command: "false", ExitCode: 1},
			"<bash-input>false</bash-input>\n<bash-stdout></bash-stdout><bash-stderr>Exit code 1</bash-stderr>",
		},
		"failure without a final newline": {
			provider.UserShellCommand{Command: "printf x; false", Output: "x", ExitCode: 1},
			"<bash-input>printf x; false</bash-input>\n<bash-stdout></bash-stdout><bash-stderr>x\nExit code 1</bash-stderr>",
		},
		"timeout": {
			provider.UserShellCommand{Command: "sleep 9", Output: "before\n", ExitCode: -1, TimedOut: true, Duration: 3 * time.Second},
			"<bash-input>sleep 9</bash-input>\n<bash-stdout></bash-stdout><bash-stderr>before\nCommand timed out after 3s</bash-stderr>",
		},
		"signal": {
			provider.UserShellCommand{Command: "kill -9 $$", ExitCode: -1},
			"<bash-input>kill -9 $$</bash-input>\n<bash-stdout></bash-stdout><bash-stderr>Command was ended by a signal</bash-stderr>",
		},
	} {
		if got := FormatUserShellCommand(tc.in); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.want)
		}
	}
}
