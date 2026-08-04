package claude

// slash_guard.go — the outbound half of Claude's slash-command handling.
//
// The CLI routes a stdin user message to its own command router when the
// message text starts with `/`, and a routed message NEVER reaches the model:
// an unknown name answers "Unknown command: /workflow" as CLI-generated text
// with `result{subtype:"success", num_turns:0}`, and everything the user
// appended after it is discarded (verified 2.1.219, 2026-08-03 live probe —
// docs/references/claude-wire.md §"Slash commands"). Routing happens for the
// array-of-content-blocks shape AO sends too, so buildUserMessageBlocks cannot
// hide behind the block wrapper.
//
// AO owns composer commands of its own (`/workflow`), so an unguarded send
// silently loses them. The guard prefixes one "\n", which defeats the CLI's
// `startsWith('/')` test while adding nothing a model reads as instruction.

// guardOutboundSlashCommand returns content adjusted so the Claude CLI's
// command router will not claim it, unless the caller explicitly allowed a
// command (provider.SendOptions.AllowClaudeSlashCommand).
//
// It fires only when the message's FIRST word is command-shaped — see
// startsWithCommandShapedWord. Everything else, including a mid-message slash
// and a leading path like "/etc/hosts is world-readable", passes through
// byte-for-byte.
func guardOutboundSlashCommand(content string, allowCommand bool) string {
	if allowCommand || !startsWithCommandShapedWord(content) {
		return content
	}
	return "\n" + content
}

// startsWithCommandShapedWord reports whether content opens with a token the
// CLI will treat as a command name: a leading `/`, then one or more characters
// from the plausible command-name set, then whitespace or end of message.
//
// The character set is letters, digits, `_`, `-`, and `:` (plugin-prefixed
// names look like `plugin:command`). A `/` inside the token disqualifies it —
// that is what keeps "/etc/hosts …" and "/usr/bin/env" out of the guard, and
// it matches the CLI, which resolves the whole first word against its command
// registry and cannot match a name containing a separator.
//
// No leading trim: the CLI tests the raw string, so " /usage" is already prose
// to it and prefixing would be a change with no effect to justify it.
func startsWithCommandShapedWord(content string) bool {
	if len(content) == 0 || content[0] != '/' {
		return false
	}
	for i := 1; i < len(content); i++ {
		c := content[i]
		if isCommandNameByte(c) {
			continue
		}
		// First byte that cannot be part of a name: a word break ends a
		// command-shaped token, anything else (a slash, a dot, a comma)
		// means the first word was never a command name.
		return isASCIISpace(c) && i > 1
	}
	// Ran to the end of the message: command-shaped as long as the name is
	// non-empty ("/" alone is not a command).
	return len(content) > 1
}

func isCommandNameByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z',
		c >= 'A' && c <= 'Z',
		c >= '0' && c <= '9':
		return true
	}
	return c == '_' || c == '-' || c == ':'
}

// isASCIISpace matches the whitespace bytes that can separate a command name
// from its arguments. Deliberately byte-wise rather than unicode.IsSpace: every
// multi-byte space rune starts with a byte >= 0x80, which is not a name byte
// either, so it falls out as "not a command" — the conservative answer, since a
// false positive would rewrite a user's prose.
func isASCIISpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}
