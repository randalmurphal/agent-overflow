package main

// Composer slash commands (spec §5.2, D31). A composer message whose first
// word is `/name` is expanded at SEND TIME, on this side of the wire:
//
//	provider payload = <exactly what the user typed> + "\n\n" + <command block>
//	stored user row  = <exactly what the user typed>, meta.command = "name"
//
// Typed text first because it is the instruction; the block second because it
// is context. Nothing is inserted into the composer, and nothing is added to
// the session's system prompt — a per-thread system prompt would invalidate
// provider prompt caching for every turn to serve one message that asked for
// it.
//
// The table below is the ONE Go-side registry. Its frontend twin (the menu and
// the composer's accent colouring) is
// `frontend/src/lib/components/composer/slashCommands.ts`; the two are
// deliberately parallel rather than generated, and this one is authoritative —
// a word the frontend colours but this table does not know expands to nothing
// and is marked as nothing.

import (
	"fmt"
	"strings"

	"agent-overflow/internal/usermessage"
)

// composerCommand is one registered command. Adding a command is one entry:
// the name the user types (without the slash) and the resolver that produces
// the block appended to the provider payload.
type composerCommand struct {
	name string
	// expand resolves the block for one thread. An error fails the send
	// loudly — a command the user invoked that silently sent nothing extra
	// is worse than a refused send, because nothing on screen would say the
	// context never arrived.
	expand func(a *App, threadID string) (string, error)
}

var composerCommands = []composerCommand{
	{name: "workflow", expand: (*App).workflowComposerBlock},
}

// lookupComposerCommand finds a registered command by name.
func lookupComposerCommand(name string) (composerCommand, bool) {
	for _, command := range composerCommands {
		if command.name == name {
			return command, true
		}
	}
	return composerCommand{}, false
}

// expandComposerCommand resolves a leading composer command in content.
//
// Returns the provider-bound payload and the recognised command name. Content
// that opens with no command word, or with a word no command claims, comes
// back unchanged with an empty name — an unknown `/foo` is ordinary text, not
// an error, because the user may simply have typed a path or a fraction.
//
// A registered command whose block resolves EMPTY still reports its name: the
// message was a command invocation and the row must say so, even when the
// project had nothing to add.
func (a *App) expandComposerCommand(threadID, content string) (string, string, error) {
	name := usermessage.LeadingCommandWord(content)
	if name == "" {
		return content, "", nil
	}
	command, ok := lookupComposerCommand(name)
	if !ok {
		return content, "", nil
	}
	block, err := command.expand(a, threadID)
	if err != nil {
		return "", "", fmt.Errorf("expand /%s: %w", name, err)
	}
	block = strings.TrimSpace(block)
	if block == "" {
		return content, name, nil
	}
	return strings.TrimRight(content, " \t\r\n") + "\n\n" + block, name, nil
}
