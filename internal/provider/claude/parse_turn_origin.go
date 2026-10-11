package claude

import (
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionimport"
)

// Turns the CLI starts on its own say why only on the main-transcript user
// row it mirrors (`transcript_mirror`, claude-wire.md §E7 and §E11): no
// stdout user envelope carries the prompt. `turnOrigin` names the cause.
const (
	mirrorTurnOriginScheduled        = "scheduled"
	mirrorTurnOriginTaskNotification = "task_notification"
)

// ScheduledTurnOrigin is the Meta.origin stamped on the user row of a turn
// the CLI started from its own schedule: a CronCreate job's fire or a
// ScheduleWakeup wake. The prompt is the one the model stored when it
// scheduled the turn, so the row is badged rather than shown as the user's.
// History import stamps the same value on the same row.
const ScheduledTurnOrigin = sessionimport.ScheduledTurnOrigin

// backgroundWakeNotificationKind is the timeline notification for a
// background task report that started a turn on an idle session.
const backgroundWakeNotificationKind = "background_wake"

// maxBackgroundWakeDetailRunes bounds the report text a wake row carries.
// An agent's `<result>` is its whole final report; the row shows the start
// of it and the task's own output stays where it was.
const maxBackgroundWakeDetailRunes = 2000

// mainTranscriptTurnStarts reads the main-transcript rows of one mirror
// batch for the turns the CLI started itself. Every other main row
// already arrived on stdout.
//
// A scheduled row becomes the turn's user row and moves the cron job
// table past the fire. A task-notification row becomes a wake row only
// while the session is between turns: one injected mid-turn reaches the
// stdout replay echo, which already reports it (parse_user_replay.go).
func (p *Parser) mainTranscriptTurnStarts(threadID string, entries []json.RawMessage, facts mirrorEntryInspection, now time.Time) []provider.ProviderEvent {
	var events []provider.ProviderEvent
	for i, fact := range facts.entries {
		if i >= len(entries) || fact.uuid == "" {
			continue
		}
		switch fact.turnOrigin {
		case mirrorTurnOriginScheduled:
			prompt := mirrorUserRowText(entries[i])
			if prompt == "" {
				continue
			}
			p.noteCommandOrigin(fact.uuid, ScheduledTurnOrigin)
			meta, err := json.Marshal(map[string]any{
				"provider_item_id": fact.uuid,
				"origin":           ScheduledTurnOrigin,
			})
			if err != nil {
				log.Printf("claude: marshal scheduled turn meta for %s: %v", fact.uuid, err)
				continue
			}
			events = append(events, provider.ProviderEvent{
				Kind:      provider.EventUserText,
				ThreadID:  threadID,
				Content:   prompt,
				Meta:      meta,
				Timestamp: now,
			})
			events = p.noteScheduledFire(events, threadID, now)
		case mirrorTurnOriginTaskNotification:
			if !p.betweenTurns {
				continue
			}
			for n, fields := range ExtractAllTaskNotificationFields(mirrorUserRowText(entries[i])) {
				if event, ok := backgroundWakeEvent(threadID, fact.uuid, n, fields, now); ok {
					events = append(events, event)
				}
			}
		}
	}
	return events
}

// backgroundWakeEvent builds the wake row for one `<task-notification>`.
// The summary already names the task (`Background command "…" completed
// (exit code 0)`, `Agent "…" finished`, `Monitor event: "…"`); the detail
// is the agent's report or the Monitor's event line.
func backgroundWakeEvent(threadID, rowUUID string, block int, fields TaskNotificationFields, now time.Time) (provider.ProviderEvent, bool) {
	summary := strings.TrimSpace(fields.Summary)
	if summary == "" {
		return provider.ProviderEvent{}, false
	}
	meta := map[string]any{"kind": backgroundWakeNotificationKind}
	if detail := truncateWakeDetail(firstNonEmpty(fields.Result, fields.Event)); detail != "" {
		meta["detail"] = detail
	}
	if fields.TaskID != "" {
		meta["taskId"] = fields.TaskID
	}
	if fields.Status != "" {
		meta["status"] = fields.Status
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		log.Printf("claude: marshal background wake meta for %s: %v", rowUUID, err)
		return provider.ProviderEvent{}, false
	}
	itemID := "background-wake:" + rowUUID
	if block > 0 {
		itemID += ":" + strconv.Itoa(block)
	}
	return provider.ProviderEvent{
		Kind:      provider.EventNotification,
		ThreadID:  threadID,
		ItemID:    itemID,
		Content:   summary,
		Meta:      encoded,
		Timestamp: now,
	}, true
}

func truncateWakeDetail(text string) string {
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) <= maxBackgroundWakeDetailRunes {
		return text
	}
	runes := []rune(text)
	return strings.TrimSpace(string(runes[:maxBackgroundWakeDetailRunes-1])) + "…"
}

// mirrorUserRowText is a transcript user row's `message.content`, a string
// or a list of text blocks.
func mirrorUserRowText(entry json.RawMessage) string {
	var row struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(entry, &row) != nil || len(row.Message.Content) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(row.Message.Content, &text) == nil {
		return strings.TrimSpace(text)
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(row.Message.Content, &blocks) != nil {
		return ""
	}
	return strings.TrimSpace(blockTextContent(blocks))
}

// noteCommandOrigin records what positively started the turn behind a
// CLI-minted command uuid: the peer delivery echo or the scheduled mirror
// row. parseCommandLifecycle stamps it on the bracket's later frames and
// releases it at the terminal one.
func (p *Parser) noteCommandOrigin(commandUUID, origin string) {
	if p == nil || commandUUID == "" {
		return
	}
	if p.commandOrigins == nil || len(p.commandOrigins) >= parserTaskMapCap {
		p.commandOrigins = make(map[string]string)
	}
	p.commandOrigins[commandUUID] = origin
}

// observeTurnWindow tracks whether the session is between turns: from a
// top-level `result` until the next turn shows itself (a top-level
// assistant message or stream start, or a `command_lifecycle` start). A
// background agent's forwarded output carries its parent tool id and does
// not count. `system/init` does not either: a woken turn emits it before
// its prompt row.
func (p *Parser) observeTurnWindow(msgType string, raw map[string]json.RawMessage) {
	switch msgType {
	case "result":
		p.betweenTurns = true
	case "assistant":
		if readRawString(raw["parent_tool_use_id"]) == "" {
			p.betweenTurns = false
		}
	case "stream_event":
		if readRawString(raw["parent_tool_use_id"]) != "" {
			return
		}
		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw["event"], &event) == nil && event.Type == "message_start" {
			p.betweenTurns = false
		}
	case "command_lifecycle":
		if readRawString(raw["state"]) == string(provider.CommandStarted) {
			p.betweenTurns = false
		}
	}
}
