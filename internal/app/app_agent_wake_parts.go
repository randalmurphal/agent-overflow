package app

import (
	"encoding/json"
	"slices"
	"strings"

	"agent-overflow/internal/agentmeta"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// The app-written messages an unconfirmed item carries: their send ids, and
// a joined message's text split into its members by the parts its dispatch
// recorded.

// appWrittenSendID reports a message the app wrote for a thread rather than
// a person: the app parts agentAppPartRoot names, the answers, reminders
// and completions that belong to a request or a watch, and other threads'
// requests.
func appWrittenSendID(sendID string) bool {
	return agentAppPartRoot(sendID) != "" || ownedSendID(sendID) || threadRequestSendIDToken(sendID) != ""
}

// threadRequestSendIDToken is the token of a request's send id, or empty.
func threadRequestSendIDToken(sendID string) string {
	token, _ := strings.CutPrefix(sendID, threadRequestSendIDPrefix)
	if token == sendID {
		return ""
	}
	return token
}

// splitSessionEndParts splits an unconfirmed item's text into the
// app-written messages it names, by send id, each with the attribution it
// was written with, and the rest, which is the person's (person nil when
// nothing is left). A message of one member is all that member's. A joined
// one splits by the parts its dispatch recorded, exactly, whatever number
// of its members have send ids; one whose parts are unknown or no longer
// describe its text does not split.
func splitSessionEndParts(members unconfirmedMembers, content string) (app map[string]usermessage.AgentMessage, person *string, ok bool) {
	if !members.joined {
		if len(members.ids) != 1 {
			return nil, nil, false
		}
		own := members.own
		return map[string]usermessage.AgentMessage{
			members.ids[0]: storedAgentMessage(members.ids[0], content, own.Origin, own.OriginThread, own.OriginWake),
		}, nil, true
	}
	texts, fits := agentmeta.SplitJoinedParts(content, members.parts)
	if !fits || !partsNameIDs(members.parts, members.ids) {
		return nil, nil, false
	}
	app = make(map[string]usermessage.AgentMessage)
	var kept []string
	for i, part := range members.parts {
		if appWrittenSendID(part.SendID) {
			app[part.SendID] = storedAgentMessage(part.SendID, texts[i], part.Origin, part.OriginThread, part.OriginWake)
			continue
		}
		kept = append(kept, texts[i])
	}
	if len(kept) == 0 {
		return app, nil, true
	}
	joined := strings.Join(kept, agentmeta.JoinSeparator)
	return app, &joined, true
}

// partsNameIDs reports whether parts name the send ids of a joined message
// in its order; a member without a send id is in parts but not in ids.
func partsNameIDs(parts []agentmeta.JoinedPart, ids []string) bool {
	var named []string
	for _, part := range parts {
		if part.SendID != "" {
			named = append(named, part.SendID)
		}
	}
	return slices.Equal(named, ids)
}

// agentAppPartRoot is the send id root of a message the app wrote for a
// thread (a loop or watch wake, an outside send, a worker's prompt), or
// empty for any other message, which is a person's.
func agentAppPartRoot(sendID string) string {
	for _, root := range []string{agentWakeSendIDRoot, agentSendIDRoot, workerPromptSendIDRoot, assistantSendIDRoot} {
		if strings.HasPrefix(sendID, root) {
			return root
		}
	}
	return ""
}

// unconfirmedMembers is what an unconfirmed item says of the messages it
// carries: their send ids in order, whether it is a joined message, and the
// parts its dispatch recorded when it is.
type unconfirmedMembers struct {
	ids    []string
	joined bool
	parts  []agentmeta.JoinedPart
	// own is the attribution of an item of one message, its facts
	// included: its row's, or its payload's when it has no row.
	own agentmeta.JoinedPart
}

// unconfirmedMembersOf reads an unconfirmed item's members: from the row it
// reserved or deferred, which a joined message lists them all on, or from
// its queue payload when it never got that far, which a joined message
// requeued after a session death lists them on too. Parts come from its
// pending send, its payload or its row, in that order.
func unconfirmedMembersOf(item triage.UnconfirmedFlushItem) unconfirmedMembers {
	var members unconfirmedMembers
	members.ids, members.joined = unconfirmedSendIDs(item)
	var payload flushQueuePayload
	decoded := len(item.Payload) > 0 && json.Unmarshal(item.Payload, &payload) == nil
	members.own = agentmeta.JoinedPart{Origin: payload.Origin, OriginThread: payload.OriginThread, OriginWake: payload.OriginWake}
	for _, row := range []*store.Item{item.QuietItem, item.DeferredItem} {
		if row == nil {
			continue
		}
		if meta, err := usermessage.FromItem(*row); err == nil {
			members.own = agentmeta.JoinedPart{Origin: meta.Origin, OriginThread: meta.OriginThread, OriginWake: meta.OriginWake}
			break
		}
	}
	switch {
	case len(item.JoinedParts) > 0:
		members.parts = item.JoinedParts
	case decoded && len(payload.JoinedParts) > 0:
		members.parts = payload.JoinedParts
	default:
		for _, row := range []*store.Item{item.DeferredItem, item.QuietItem} {
			if row == nil {
				continue
			}
			if meta, err := usermessage.FromItem(*row); err == nil && len(meta.JoinedParts) > 0 {
				members.parts = meta.JoinedParts
				break
			}
		}
	}
	members.joined = members.joined || len(members.parts) > 0
	return members
}

// unconfirmedSendIDs names every message an unconfirmed item carries, in
// order, and reports whether the item is a joined message
// (unconfirmedMembersOf).
func unconfirmedSendIDs(item triage.UnconfirmedFlushItem) (ids []string, joined bool) {
	for _, row := range []*store.Item{item.DeferredItem, item.QuietItem} {
		if row == nil {
			continue
		}
		meta, err := usermessage.FromItem(*row)
		if err != nil {
			return nil, false
		}
		if len(meta.JoinedSendIDs) > 0 {
			return meta.JoinedSendIDs, true
		}
		if meta.SendID != "" {
			return []string{meta.SendID}, false
		}
	}
	var payload flushQueuePayload
	if len(item.Payload) > 0 && json.Unmarshal(item.Payload, &payload) == nil {
		if len(payload.JoinedSendIDs) > 0 {
			return payload.JoinedSendIDs, true
		}
		if payload.SendID != "" {
			return []string{payload.SendID}, false
		}
	}
	return nil, false
}
