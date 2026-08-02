package codex

import (
	"encoding/json"
	"log"
	"slices"
	"sync"
)

// codexNotificationCatalog is every server notification method a Codex
// app-server is known to emit, in upstream declaration order.
//
// Sources, both consulted because they disagree: the pinned
// `server_notification_definitions!` block at
// codex-rs/app-server-protocol/src/protocol/common.rs (rust-v0.142.5,
// the tag ~/repos/codex sits on) plus the method table recovered from
// the installed codex-cli 0.146.0 binary, which is what actually runs.
// Every 0.142.5 method is still present in 0.146.0; 0.146.0 adds
// `rawResponse/completed`, `thread/environment/connected` and
// `thread/environment/disconnected`.
//
// The catalog exists to compute the initialize-time opt-out list (the
// complement of what we consume). Being out of date is therefore safe in
// exactly one direction: a method upstream adds and we have not listed
// simply keeps being delivered, and arrives at the drift log in
// dispatchRoutableNotification. It can never cause us to unsubscribe
// from something we handle, because membership here is not what makes a
// method opt-out-able — failing to consume it is.
var codexNotificationCatalog = []string{
	"error",
	"thread/started",
	"thread/status/changed",
	"thread/archived",
	"thread/deleted",
	"thread/unarchived",
	"thread/closed",
	"skills/changed",
	"thread/name/updated",
	"thread/goal/updated",
	"thread/goal/cleared",
	"thread/environment/connected",
	"thread/environment/disconnected",
	"thread/settings/updated",
	"thread/tokenUsage/updated",
	"turn/started",
	"hook/started",
	"turn/completed",
	"hook/completed",
	"turn/diff/updated",
	"turn/plan/updated",
	"item/started",
	"item/autoApprovalReview/started",
	"item/autoApprovalReview/completed",
	"item/completed",
	"rawResponseItem/completed",
	"rawResponse/completed",
	"item/agentMessage/delta",
	"item/plan/delta",
	"command/exec/outputDelta",
	"process/outputDelta",
	"process/exited",
	"item/commandExecution/outputDelta",
	"item/commandExecution/terminalInteraction",
	"item/fileChange/outputDelta",
	"item/fileChange/patchUpdated",
	"serverRequest/resolved",
	"item/mcpToolCall/progress",
	"mcpServer/oauthLogin/completed",
	"mcpServer/startupStatus/updated",
	"account/updated",
	"account/rateLimits/updated",
	"app/list/updated",
	"remoteControl/status/changed",
	"externalAgentConfig/import/progress",
	"externalAgentConfig/import/completed",
	"fs/changed",
	"item/reasoning/summaryTextDelta",
	"item/reasoning/summaryPartAdded",
	"item/reasoning/textDelta",
	"thread/compacted",
	"model/rerouted",
	"model/verification",
	"turn/moderationMetadata",
	"model/safetyBuffering/updated",
	"warning",
	"guardianWarning",
	"deprecationNotice",
	"configWarning",
	"fuzzyFileSearch/sessionUpdated",
	"fuzzyFileSearch/sessionCompleted",
	"thread/realtime/started",
	"thread/realtime/itemAdded",
	"thread/realtime/transcript/delta",
	"thread/realtime/transcript/done",
	"thread/realtime/outputAudio/delta",
	"thread/realtime/sdp",
	"thread/realtime/error",
	"thread/realtime/closed",
	"windows/worldWritableWarning",
	"windowsSandbox/setupCompleted",
	"account/login/completed",
}

// sessionInlineConsumedNotifications lists methods a live Session reads
// for its own state without the classifier reporting them handled. They
// are consumed just as much as a classified method is — they must never
// be opted out, and they must never reach the protocol-drift log.
//
// Keep this list at exactly the inline `if method == …` reads in
// dispatchRoutableNotification that no classifier case covers. Everything
// else routes through either a classifier case or
// sessionSideChannelNotifications, both of which are discovered rather
// than listed.
var sessionInlineConsumedNotifications = []string{
	// item/plan/delta feeds appendPlanDelta; the buffered text surfaces
	// on the item/completed plan item, not on the delta itself.
	"item/plan/delta",
}

// emptyNotificationParams is the probe payload used to ask a classifier
// "would you claim this method?" — see notificationMethodClassified.
var emptyNotificationParams = json.RawMessage(`{}`)

// notificationMethodClassified reports whether ClassifyNotification's
// dispatchers claim a method.
//
// It answers by probing the real dispatchers rather than by keeping a
// second copy of their case lists, which is the whole point: a handler
// added to protocol_*.go automatically stops being opt-out-able, with no
// second edit to remember. The probe is exact because every dispatcher
// decides `handled` from the method alone — the `true` in each case arm
// is unconditional, params only shape the events. TestClassifiedMethods
// AreDecidedByMethodAlone pins that property for the cases with the most
// params-dependent bodies.
func notificationMethodClassified(method string) bool {
	_, handled := classifyNotification("", method, emptyNotificationParams)
	return handled
}

// notificationMethodConsumed reports whether this package does anything
// at all with a notification method: classify it into events, route it to
// a session side channel, or read it inline for session state.
func notificationMethodConsumed(method string) bool {
	if _, ok := sessionSideChannelNotifications[method]; ok {
		return true
	}
	if slices.Contains(sessionInlineConsumedNotifications, method) {
		return true
	}
	return notificationMethodClassified(method)
}

// sessionOptOutNotificationMethods is the initialize-time opt-out list
// for a full chat Session: every catalogued method this package does not
// consume.
//
// Codex filters opted-out notifications per connection before they are
// serialized (codex-rs/app-server/src/transport.rs
// should_skip_notification_for_connection), so this removes real work —
// AO runs one app-server per thread, and each unconsumed notification
// otherwise costs a serialize on their side plus a parse, a route, and a
// classifier walk on ours.
//
// Derived, never hand-written: the list is the complement of the consumed
// set, so it cannot rot into unsubscribing from something a later handler
// starts consuming.
var sessionOptOutNotificationMethods = sync.OnceValue(func() []string {
	optOut := make([]string, 0, len(codexNotificationCatalog))
	for _, method := range codexNotificationCatalog {
		if notificationMethodConsumed(method) {
			continue
		}
		optOut = append(optOut, method)
	}
	return optOut
})

// oneShotOptOutNotificationMethods is the opt-out list for the short-lived
// handshakes (login, account probe, identity probe, model list, MCP
// status). They never construct a Session, so the classifier's consumed
// set is irrelevant to them: everything in the catalogue is noise except
// the notifications their own read loop waits on, which the caller names.
//
// Naming them at the call site is what keeps the opt-out honest — a
// client that starts depending on a notification has to say so in the same
// place it asks not to receive things. Passing none is a legitimate and
// common answer (a client that only reads responses), which is why this is
// a separate function from the Session list rather than a flag on one.
func oneShotOptOutNotificationMethods(keep ...string) []string {
	optOut := make([]string, 0, len(codexNotificationCatalog))
	for _, method := range codexNotificationCatalog {
		if slices.Contains(keep, method) {
			continue
		}
		optOut = append(optOut, method)
	}
	return optOut
}

// maxUnclaimedNotificationMethods bounds the per-session drift-log dedup
// set. The catalogue is ~70 entries and only unconsumed ones can land
// here, so a healthy session logs nothing and a drifting one logs a
// handful. The cap exists because the key is server-supplied: a buggy or
// hostile app-server emitting unique method names must not grow the map
// without limit.
const maxUnclaimedNotificationMethods = 32

// warnUnclaimedNotification reports a notification method no classifier
// claimed, once per method per session.
//
// This is the protocol-drift alarm. Silence here is how seven upstream
// notifications reached production unnoticed between the 2026-06 and
// 2026-07 surveys: the classifier chain simply returned nil with no
// default branch. It is deliberately log-and-continue rather than an
// error — a Codex release adding a notification must never break a live
// session — but it is never silent.
//
// Methods this package consumes without classifying are excluded, so the
// log means "we have never seen this before", not "we chose not to render
// it": everything we deliberately ignore is either claimed by a
// classifier case that returns no events, or opted out at initialize and
// therefore never delivered at all.
func (s *Session) warnUnclaimedNotification(method string) {
	if method == "" {
		return
	}
	// Fast path first. An unknown method can be a high-frequency stream
	// (a new delta notification, say), and the consumed check below walks
	// the classifier chain — it must not run once per delivery.
	s.mu.Lock()
	_, seen := s.unclaimedNotifications[method]
	s.mu.Unlock()
	if seen {
		return
	}
	if notificationMethodConsumed(method) {
		return
	}
	s.mu.Lock()
	if s.unclaimedNotifications == nil {
		s.unclaimedNotifications = make(map[string]struct{})
	}
	// Re-check under the lock: the fast path above released it.
	if _, seen := s.unclaimedNotifications[method]; seen {
		s.mu.Unlock()
		return
	}
	if len(s.unclaimedNotifications) >= maxUnclaimedNotificationMethods {
		alreadyReported := s.unclaimedNotificationsOverflowed
		s.unclaimedNotificationsOverflowed = true
		s.mu.Unlock()
		if !alreadyReported {
			log.Printf(
				"codex: %d unclaimed notification methods on this session; suppressing further drift warnings",
				maxUnclaimedNotificationMethods,
			)
		}
		return
	}
	s.unclaimedNotifications[method] = struct{}{}
	s.mu.Unlock()
	log.Printf(
		"codex: unhandled notification method %q — no classifier claimed it; "+
			"add a case in protocol_*.go or add it to codexNotificationCatalog to opt out",
		method,
	)
}

// codexInitializeParams builds the `initialize` params every entry point
// in this package sends: the shared client identity, the experimental-API
// capability that gates `thread/settings/updated`, `item/plan/delta`, the
// background-terminal RPCs and the collab-agent surface, and the caller's
// notification opt-out list.
//
// One builder so the five handshakes (Session, login, account probe,
// identity probe, model list) cannot drift apart on identity or
// capabilities — only on the opt-out list, which is legitimately
// per-client.
func codexInitializeParams(clientName string, optOut []string) map[string]any {
	capabilities := map[string]any{
		"experimentalApi": true,
	}
	if len(optOut) > 0 {
		capabilities["optOutNotificationMethods"] = optOut
	}
	return map[string]any{
		"clientInfo": map[string]any{
			"name":    clientName,
			"title":   "Agent Overflow",
			"version": "0.1.0",
		},
		"capabilities": capabilities,
	}
}
