package usermessage

import (
	"slices"
	"strings"
)

// Composer shell commands. A command the person runs as `!command` is stored
// as that typed text with the command's output as an attachment (ShellMeta).
// The provider reads the output where the text was typed and the timeline
// shows a command block in its place, so both sides ask one question: which
// parts of this text stand for which attachment.
//
// This file owns that rule, as command.go owns the slash command's. The
// frontend's copy is `frontend/src/lib/utils/shellInvocation.ts`, and both
// run testdata/shell_invocation_vectors.json.

// ShellInvocation is the text a command is typed and stored as.
func ShellInvocation(command string) string {
	return "!" + command
}

// PartRun is a run of a message's joined parts, First to Last inclusive.
// First is negative when there is no run.
type PartRun struct {
	First, Last int
}

// ShellInvocationRuns reports, for each command in order, the parts of one
// message that are its typed invocation. parts is the message split on
// JoinSeparator. Rules, mirrored exactly by the frontend:
//
//   - an invocation is whole parts: the run's text, joined by the separator
//     and trimmed of surrounding white space, is exactly `!command`. A
//     command that holds the separator itself, as a heredoc can, is a run
//     of several parts;
//   - commands claim in order, each the first run no earlier command took,
//     so a command run twice matches its two invocations in turn.
//
// A run grows one part at a time until its text is as long as the
// invocation, which is where it either matches or cannot.
func ShellInvocationRuns(parts []string, commands []string) []PartRun {
	runs := make([]PartRun, len(commands))
	claimed := make([]bool, len(parts))
	for i, command := range commands {
		runs[i] = PartRun{First: -1, Last: -1}
		invocation := ShellInvocation(command)
		for first := range parts {
			last := invocationEnd(parts, claimed, first, invocation)
			if last < 0 {
				continue
			}
			for part := first; part <= last; part++ {
				claimed[part] = true
			}
			runs[i] = PartRun{First: first, Last: last}
			break
		}
	}
	return runs
}

// invocationEnd is the last part of the run starting at first whose text is
// invocation, or -1.
func invocationEnd(parts []string, claimed []bool, first int, invocation string) int {
	for last := first; last < len(parts) && !claimed[last]; last++ {
		typed := strings.TrimSpace(strings.Join(parts[first:last+1], JoinSeparator))
		if len(typed) < len(invocation) {
			continue
		}
		if typed == invocation {
			return last
		}
		return -1
	}
	return -1
}

// IsShellInvocation reports whether text is exactly the typed invocation of
// one of commands.
func IsShellInvocation(text string, commands []string) bool {
	typed := strings.TrimSpace(text)
	return slices.ContainsFunc(commands, func(command string) bool {
		return typed == ShellInvocation(command)
	})
}

// utf16Length is the length of s in UTF-16 code units, which is how the
// frontend measures the same text. An invalid byte counts as the one
// replacement character encoding/json writes for it.
func utf16Length(s string) int {
	units := 0
	for _, r := range s {
		units++
		if r >= 0x10000 {
			units++
		}
	}
	return units
}
