package claude

import (
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/provider"
)

const maxTrackedDirectSlashCommands = 64

// directSlashCommand is the command-router input AO put on stdin. It is
// recorded independently of the asynchronously advertised command list:
// command-shaped text is native Claude syntax even when discovery has not
// arrived yet, and unknown names must reach Claude's own visible error.
type directSlashCommand struct {
	Name     string
	Argument string
	Internal bool
}

type directSlashCommands struct {
	mu     sync.Mutex
	byUUID map[string]directSlashCommand
}

type mirroredCommand struct {
	command           directSlashCommand
	launchID          string
	agentID           string
	rootProjectionKey string
	skillName         string
	provisional       bool
	forked            bool
	pendingOutput     []provider.ProviderEvent
}

func (l *directSlashCommands) note(uuid, content string, opts provider.SendOptions) {
	uuid = strings.TrimSpace(uuid)
	if uuid == "" || opts.GuardClaudeSlashCommand || !startsWithCommandShapedWord(content) {
		return
	}
	name, argument := outboundSlashCommandPreservingArgument(content)
	if name == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byUUID == nil {
		l.byUUID = make(map[string]directSlashCommand, 2)
	}
	if _, exists := l.byUUID[uuid]; exists {
		return
	}
	if len(l.byUUID) >= maxTrackedDirectSlashCommands {
		log.Printf("claude: direct slash-command tracking limit reached; command %q will not be mirror-classified", name)
		return
	}
	l.byUUID[uuid] = directSlashCommand{Name: name, Argument: argument, Internal: opts.InternalCommand}
}

func (l *directSlashCommands) get(uuid string) (directSlashCommand, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	command, ok := l.byUUID[uuid]
	return command, ok
}

func (l *directSlashCommands) release(uuid string) {
	if uuid == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byUUID, uuid)
}

func outboundSlashCommandPreservingArgument(content string) (name, argument string) {
	if !startsWithCommandShapedWord(content) {
		return "", ""
	}
	end := len(content)
	for i := 1; i < len(content); i++ {
		if !isCommandNameByte(content[i]) {
			end = i
			break
		}
	}
	name = strings.ToLower(content[1:end])
	argument, _, _ = strings.Cut(strings.TrimSpace(content[end:]), "\n")
	return name, strings.TrimSpace(argument)
}

func (p *Parser) startMirroredCommand(threadID, commandUUID string, now time.Time) []provider.ProviderEvent {
	if p == nil || p.peerTurns == nil {
		return nil
	}
	command, ok := p.peerTurns.directSlashCommand(commandUUID)
	if !ok {
		return nil
	}
	state := p.ensureTranscriptMirrorState()
	state.commands[commandUUID] = &mirroredCommand{command: command}
	if command.Internal {
		return nil
	}
	return p.provisionalMirroredCommandEvent(threadID, commandUUID, now)
}

func (p *Parser) holdMirroredCommandOutput(commandUUID string, events []provider.ProviderEvent) bool {
	if p == nil || p.transcriptMirror == nil {
		return false
	}
	command := p.transcriptMirror.commands[commandUUID]
	if command == nil {
		return false
	}
	command.pendingOutput = append(command.pendingOutput, events...)
	return true
}

func (p *Parser) provisionalMirroredCommandEvent(threadID, commandUUID string, now time.Time) []provider.ProviderEvent {
	if p == nil || p.transcriptMirror == nil {
		return nil
	}
	command := p.transcriptMirror.commands[commandUUID]
	if command == nil || command.launchID != "" {
		return nil
	}
	command.launchID = "claude-command:" + commandUUID
	command.provisional = true
	input := map[string]any{"command": "/" + command.command.Name}
	if command.command.Argument != "" {
		input["args"] = command.command.Argument
	}
	meta, _ := json.Marshal(map[string]any{
		"toolName":                         "Command",
		"input":                            input,
		provider.MetaTranscriptMirroredKey: true,
	})
	return []provider.ProviderEvent{{
		Kind:      provider.EventToolStart,
		ThreadID:  threadID,
		ItemID:    command.launchID,
		ItemType:  "Command",
		Meta:      meta,
		Timestamp: now,
	}}
}

func (p *Parser) finishMirroredCommand(threadID, commandUUID string, terminal provider.CommandLifecycleState, now time.Time) []provider.ProviderEvent {
	if p == nil || p.transcriptMirror == nil {
		return nil
	}
	state := p.transcriptMirror
	command := state.commands[commandUUID]
	if command == nil {
		return nil
	}
	delete(state.commands, commandUUID)
	delete(state.degradedCommands, commandUUID)
	for key, owner := range state.pendingOwner {
		if owner == commandUUID {
			state.clearPending(key)
		}
	}
	if command.launchID == "" {
		return command.pendingOutput
	}

	var events []provider.ProviderEvent
	for key, projection := range state.projections {
		if projection.commandUUID != commandUUID {
			continue
		}
		result := projection.projector.Close()
		events = append(events, state.providerEvents(threadID, result, key)...)
		state.removeProjection(key)
	}
	metaFields := map[string]any{}
	if command.forked {
		metaFields["directCommandFork"] = true
		metaFields["skillFork"] = map[string]string{
			"agentId":     command.agentID,
			"commandName": firstNonEmpty(command.skillName, command.command.Name),
		}
	}
	var content strings.Builder
	var raw []byte
	var forkResult *provider.ProviderEvent
	if command.forked && len(command.pendingOutput) > 0 {
		output := command.pendingOutput[0]
		for _, part := range command.pendingOutput {
			if content.Len() > 0 && part.Content != "" {
				content.WriteString("\n\n")
			}
			content.WriteString(part.Content)
			if len(part.Raw) > 0 {
				raw = part.Raw
			}
		}
		if content.Len() > 0 {
			var resultMeta provider.CommandResultMeta
			_ = json.Unmarshal(output.Meta, &resultMeta)
			resultMeta.CommandUUID = commandUUID
			resultMeta.Suppressed = false
			resultMeta.AgentResult = &provider.CommandAgentResultMeta{
				LaunchID:   command.launchID,
				SourceKind: "skill",
				SourceName: firstNonEmpty(command.skillName, command.command.Name),
			}
			output.Meta, _ = json.Marshal(resultMeta)
			// Keep the result top-level. Parenting it to the Skill would make the
			// grouping projection hide the answer whenever the activity card is
			// collapsed.
			output.ParentToolUseID = ""
			output.Content = content.String()
			output.ContentPresent = true
			output.Raw = raw
			output.Timestamp = now
			forkResult = &output
		}
	} else {
		for _, output := range command.pendingOutput {
			var resultMeta provider.CommandResultMeta
			_ = json.Unmarshal(output.Meta, &resultMeta)
			resultMeta.CommandUUID = commandUUID
			if content.Len() > 0 && output.Content != "" {
				content.WriteString("\n\n")
			}
			content.WriteString(output.Content)
			if len(output.Raw) > 0 {
				raw = output.Raw
			}
			resultMeta.Suppressed = true
			output.Meta, _ = json.Marshal(resultMeta)
			events = append(events, output)
		}
	}
	// A synthetic command result is a completed local-command answer. Claude
	// can still close the command-lifecycle bracket as cancelled after the
	// fork's own reporting tool failed and recovered; that transport state must
	// not overwrite the answer the command actually returned.
	if terminal != provider.CommandCompleted && forkResult == nil {
		metaFields["is_error"] = true
	}
	if forkResult != nil {
		metaFields["directCommandResult"] = true
	}
	meta, _ := json.Marshal(metaFields)
	completion := provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  threadID,
		ItemID:    command.launchID,
		Meta:      meta,
		Timestamp: now,
	}
	if !command.forked && content.Len() > 0 {
		completion.Content = content.String()
		completion.ContentPresent = true
		completion.Raw = raw
	}
	events = append(events, completion)
	if forkResult != nil {
		events = append(events, *forkResult)
	}
	return events
}
