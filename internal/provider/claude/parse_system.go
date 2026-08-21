// Package claude — parser for `system`-type NDJSON lines (init metadata,
// compact_boundary, and the task_started / task_updated / task_notification
// triples that drive Claude's background-task lifecycle).

package claude

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/slicesx"
)

func (p *Parser) parseSystem(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
	var subtype string
	if err := json.Unmarshal(raw["subtype"], &subtype); err != nil {
		return nil, nil // no subtype — skip
	}

	switch subtype {
	case "init":
		info := extractSessionInfo(raw)
		// Remember the model id so result usage can be priced without a
		// round-trip to the store. p is nil only in the package-level
		// ParseLine helper's test-only fast path; in that path we can't
		// price anyway, so skip the assignment.
		if p != nil {
			p.model = strings.TrimSpace(info.Model)
		}
		meta, _ := json.Marshal(info)
		return []provider.ProviderEvent{{
			Kind:      provider.EventInit,
			ThreadID:  threadID,
			Meta:      meta,
			Timestamp: now,
			Raw:       line,
		}}, nil

	case "commands_changed":
		// Spontaneous full-list push after mid-session skill discovery or a
		// plugin reload (verified 2.1.219, 2026-08-03 live probe;
		// claude-wire.md §"Slash commands"). REPLACE semantics: the payload is
		// the whole answer, and a name that fell off the list is gone. An
		// envelope with no `commands` key says nothing and is dropped —
		// see decodeCommandsChanged for why that is not the same as `[]`.
		commands, present := decodeCommandsChanged(raw["commands"])
		if !present {
			return nil, nil
		}
		meta, err := json.Marshal(provider.CommandsChangedMeta{
			Commands: slicesx.OrEmpty(commands),
		})
		if err != nil {
			return nil, fmt.Errorf("parse commands_changed: marshal meta: %w", err)
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventCommandsChanged,
			ThreadID:  threadID,
			Meta:      meta,
			Timestamp: now,
		}}, nil

	case "session_state_changed":
		return []provider.ProviderEvent{{
			Kind:      provider.EventSessionStatus,
			ThreadID:  threadID,
			Content:   "session_state_changed",
			Meta:      raw["data"],
			Timestamp: now,
		}}, nil

	case "status":
		return parseStatusEvent(threadID, raw, now)

	case "tool_progress":
		// Streaming tool progress is intentionally dropped. The chat rewrite
		// renders successive tool_call summary upserts rather than a parallel
		// progress-event channel.
		return nil, nil

	case "compact_boundary":
		meta := extractCompactBoundaryMeta(raw)
		return []provider.ProviderEvent{{
			Kind:      provider.EventCompactBoundary,
			ThreadID:  threadID,
			ItemID:    readRawString(raw["uuid"]),
			Content:   readRawString(raw["content"]),
			Meta:      meta,
			Timestamp: now,
		}}, nil

	case "api_retry":
		// Normalize the SDK's `data` payload into a shape both providers
		// share: `attempt` (1-indexed), `max_retries`, and an `error`
		// string. Triage uses these to render the timeline retry row
		// (hiding attempts < 4, mirroring Claude Code's TUI). The raw
		// `data` is preserved under `wire` for forensics.
		retryPayload := apiRetryPayload(raw)
		retryMeta := buildClaudeAPIRetryMeta(retryPayload)
		return []provider.ProviderEvent{{
			Kind:      provider.EventAPIRetry,
			ThreadID:  threadID,
			Meta:      retryMeta,
			Failure:   claudeAPIRetryFailure(retryPayload),
			Timestamp: now,
		}}, nil

	case "api_error":
		// The RICHER twin of `api_retry`: the CLI emits both for one
		// retryable API failure (2.1.237 — `api_error` carries the
		// plain-data snapshot the REPL's retry banner renders, and
		// `api_retry` is the flattened SDK message). Routing it onto the
		// same EventAPIRetry is deliberate: triage upserts a single
		// per-turn row keyed `retry:<turnIndex>`, so the pair collapses
		// onto one row instead of double-rendering, and whichever
		// envelope a given CLI build emits produces the same surface.
		//
		// Wire shape (2.1.237 SDK schema + the internal SystemMessage it
		// is built from): `error:{message, status?, request_id?,
		// formatted, connection:{code,message,is_ssl_error}|null,
		// is_network_down, rate_limits:{resets_at?,rate_limit_type?}|null}`
		// plus retry counters. The counters are spelled `retry_attempt` /
		// `max_retries` / `retry_in_ms` in the schema and
		// `retryAttempt` / `maxRetries` / `retryInMs` on the internal
		// object that the passthrough branch of the serializer emits —
		// buildClaudeAPIRetryMeta reads both spellings.
		errorPayload := apiRetryPayload(raw)
		errorMeta := buildClaudeAPIRetryMeta(errorPayload)
		return []provider.ProviderEvent{{
			Kind:      provider.EventAPIRetry,
			ThreadID:  threadID,
			Meta:      errorMeta,
			Failure:   claudeAPIRetryFailure(errorPayload),
			Timestamp: now,
		}}, nil

	case "model_refusal_fallback", "model_fallback", "model_consent_fallback":
		return p.parseModelFallbackEvent(subtype, threadID, raw, now, line)

	case "model_refusal_no_fallback":
		return parseModelRefusalNoFallbackEvent(threadID, raw, now, line)

	case "permission_denied":
		return parsePermissionDeniedEvent(threadID, raw, now, line)

	case "permission_retry":
		return parsePermissionRetryEvent(threadID, raw, now, line)

	case "task_started":
		return p.parseTaskStartedEvent(threadID, raw, now)

	case "task_updated":
		return p.parseTaskLifecycleEvent(threadID, raw, now)

	case "task_notification":
		return p.parseTaskNotificationEvent(threadID, raw, now)

	// Explicitly skipped subtypes — no action, no error.
	case "hook_started", "hook_progress", "hook_response",
		"notification",
		"files_persisted",
		"tool_use_summary",
		"memory_recall",
		"local_command_output",
		"task_progress":
		return nil, nil

	default:
		// Unknown system subtype — skip.
		return nil, nil
	}
}

const (
	maxClaudeFallbackReasonRunes      = 1000
	maxClaudeFallbackExplanationRunes = 300
	maxClaudeFallbackModelRunes       = 128
	maxClaudeFallbackLabelRunes       = 64
	maxClaudeFallbackIDRunes          = 128
)

// parseStatusEvent handles `system/status`, the CLI's general activity
// status channel (claude-wire.md §system/status; verified 2.1.219):
//
//   - `{"status":"compacting"}` — a compaction (manual /compact or
//     auto, both route through the CLI's shared compactConversation)
//     just started, and the wire will now go silent until it resolves.
//     Remote sessions may re-emit it as a 30s keep-alive; triage treats
//     repeats as idempotent.
//   - `{"status":null,"compact_result":"success"}` — compaction
//     finished; `system/compact_boundary` follows within milliseconds.
//   - `{"status":null,"compact_result":"failed","compact_error":…}` —
//     compaction failed or was interrupted; no boundary follows.
//   - `{"status":"requesting"}` — the CLI opened an API request. Fires
//     constantly during ordinary turns; deliberately dropped because
//     turn activity is already wire-pushed through the round lifecycle.
//
// Unknown status values are dropped like `requesting` — this channel is
// additive and a new value must not become an event nothing routes.
func parseStatusEvent(threadID string, raw map[string]json.RawMessage, now time.Time) ([]provider.ProviderEvent, error) {
	status := readRawString(raw["status"])
	compactResult := readRawString(raw["compact_result"])
	switch {
	case status == "compacting":
		meta, err := json.Marshal(provider.CompactionStatusMeta{Active: true})
		if err != nil {
			return nil, fmt.Errorf("parse system status: marshal compacting meta: %w", err)
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventCompactionStatus,
			ThreadID:  threadID,
			Meta:      meta,
			Timestamp: now,
		}}, nil
	case compactResult != "":
		meta, err := json.Marshal(provider.CompactionStatusMeta{
			Active:       false,
			Result:       compactResult,
			ErrorMessage: readRawString(raw["compact_error"]),
		})
		if err != nil {
			return nil, fmt.Errorf("parse system status: marshal compact_result meta: %w", err)
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventCompactionStatus,
			ThreadID:  threadID,
			Meta:      meta,
			Timestamp: now,
		}}, nil
	default:
		return nil, nil
	}
}

// readRawStringAny returns the first non-empty string among the named
// keys. The model-fallback family is spelled snake_case ON THE WIRE
// (`fallback_model`, `api_refusal_category`, … — verified in the
// stream-json serializer of 2.1.214, 2.1.219 and 2.1.237) while the
// CLI's own INTERNAL message objects use camelCase, and the passthrough
// branch of one serializer path emits an internal object verbatim. Every
// field in this family therefore accepts both spellings: reading only one
// is how the pre-existing camelCase-only reads silently rejected every
// real `model_refusal_fallback` envelope.
func readRawStringAny(raw map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(readRawString(raw[key])); value != "" {
			return value
		}
	}
	return ""
}

// parseModelFallbackEvent handles the three system subtypes that mean
// "the turn survived, but on a different model than the user asked for".
// All three land on EventModelFallback, which triage persists as a
// timeline notification AND projects as the session-scoped effective
// model — the reason they share one path is that the user-visible
// consequence is identical; only the CAUSE differs, and the cause rides
// meta.kind.
//
// Wire shapes (2.1.237 SDK schema; snake_case on the wire, camelCase on
// the CLI's internal object — both accepted):
//
//   - model_refusal_fallback   trigger, direction, scope?, original_model,
//     fallback_model, request_id, api_refusal_category,
//     api_refusal_explanation, refused_user_message_uuid,
//     retracted_message_uuids?, saw_cyber_refusal?, provisional?, content
//   - model_fallback           trigger ("model_not_found" |
//     "permission_denied" | "model_blocked" | …), original_model,
//     fallback_model, content
//   - model_consent_fallback   choice, original_model, fallback_model,
//     persisted_as_default, content
//
// `fallback_model` is required on all three (triage refuses an empty one)
// — without it there is no "now running as" to report and the event is a
// parse error rather than a silent drop.
func (p *Parser) parseModelFallbackEvent(
	subtype, threadID string,
	raw map[string]json.RawMessage,
	now time.Time,
	line []byte,
) ([]provider.ProviderEvent, error) {
	fallbackModel := readRawStringAny(raw, "fallback_model", "fallbackModel")
	if fallbackModel == "" {
		return nil, fmt.Errorf("parse %s: empty fallback_model", subtype)
	}
	if len([]rune(fallbackModel)) > maxClaudeFallbackModelRunes {
		return nil, fmt.Errorf("parse %s: fallback_model exceeds %d runes", subtype, maxClaudeFallbackModelRunes)
	}
	// The following assistant snapshot carries message.model and a
	// content block of type "fallback", but this system envelope is the
	// authoritative user-facing signal: it includes the classifier reason,
	// category, requested model, and refused user-message identity.
	if p != nil {
		p.model = fallbackModel
	}
	originalModel := boundedClaudeFallbackField(
		readRawStringAny(raw, "original_model", "originalModel"), maxClaudeFallbackModelRunes)
	reason := truncate(strings.TrimSpace(readRawString(raw["content"])), maxClaudeFallbackReasonRunes)
	if reason == "" {
		reason = fallbackReasonFallbackCopy(subtype, originalModel, fallbackModel)
	}
	fields := map[string]any{
		"kind":                   subtype,
		"originalModel":          originalModel,
		"fallbackModel":          fallbackModel,
		"reason":                 reason,
		"trigger":                boundedClaudeFallbackField(readRawStringAny(raw, "trigger"), maxClaudeFallbackLabelRunes),
		"apiRefusalCategory":     boundedClaudeFallbackField(readRawStringAny(raw, "api_refusal_category", "apiRefusalCategory"), maxClaudeFallbackLabelRunes),
		"apiRefusalExplanation":  truncate(readRawStringAny(raw, "api_refusal_explanation", "apiRefusalExplanation"), maxClaudeFallbackExplanationRunes),
		"refusedUserMessageUuid": boundedClaudeFallbackField(readRawStringAny(raw, "refused_user_message_uuid", "refusedUserMessageUuid"), maxClaudeFallbackIDRunes),
	}
	// model_consent_fallback's own two fields: WHICH consent choice the
	// user (or the CLI's default) made, and whether it was written back as
	// the account default rather than applying for this session only.
	if choice := boundedClaudeFallbackField(readRawStringAny(raw, "choice"), maxClaudeFallbackLabelRunes); choice != "" {
		fields["choice"] = choice
	}
	// Only recorded when TRUE: the shared readBoolValue helper reports
	// "saw a true", not "saw a value", and `false` here means "for this
	// session only" — which the composed sentence already says.
	if readBoolValue(raw, "persisted_as_default", "persistedAsDefault") {
		fields["persistedAsDefault"] = true
	}
	meta, _ := json.Marshal(fields)

	requestID := readRawStringAny(raw, "request_id", "requestId", "uuid")
	return []provider.ProviderEvent{{
		Kind:      provider.EventModelFallback,
		ThreadID:  threadID,
		ItemID:    claudeFallbackItemID(subtype, requestID),
		Content:   reason,
		Meta:      meta,
		Timestamp: now,
		Raw:       line,
	}}, nil
}

// fallbackReasonFallbackCopy is used only when the CLI sent an empty
// `content`. It is observed empty on at least one producer of
// `model_refusal_no_fallback` and is optional on the rest, and a
// notification with no sentence is worse than a generic one.
func fallbackReasonFallbackCopy(subtype, originalModel, fallbackModel string) string {
	if originalModel == "" {
		originalModel = "the requested model"
	}
	switch subtype {
	case "model_consent_fallback":
		return fmt.Sprintf("Switched to %s — %s requires usage credits.", fallbackModel, originalModel)
	case "model_fallback":
		return fmt.Sprintf("Switched to %s because %s is not available.", fallbackModel, originalModel)
	default:
		return fmt.Sprintf("%s could not handle this request and switched to %s.", originalModel, fallbackModel)
	}
}

// parseModelRefusalNoFallbackEvent handles the ONE member of the refusal
// family that does not continue on another model: the request was
// refused and no fallback route matched, so the turn produces nothing.
// That makes it an ERROR notice, not the informational "now running as
// X" notice its three siblings get (core principle 5 — errors are
// user-facing state).
//
// Wire shape (2.1.237): `original_model`, `request_id`,
// `api_refusal_category`, `api_refusal_explanation`,
// `refused_user_message_uuid`, `content` — every one of which the CLI
// may send as null, and `content` is observed EMPTY on the live
// producer, so the human sentence is composed here.
//
// The meta deliberately carries no top-level `fatal` and no top-level
// `error` string: the session is alive (only the turn died), and triage
// reads those two keys as "the provider process is gone" and "this is an
// SDK error enum" respectively.
func parseModelRefusalNoFallbackEvent(
	threadID string,
	raw map[string]json.RawMessage,
	now time.Time,
	line []byte,
) ([]provider.ProviderEvent, error) {
	originalModel := boundedClaudeFallbackField(
		readRawStringAny(raw, "original_model", "originalModel"), maxClaudeFallbackModelRunes)
	category := boundedClaudeFallbackField(
		readRawStringAny(raw, "api_refusal_category", "apiRefusalCategory"), maxClaudeFallbackLabelRunes)
	explanation := truncate(
		readRawStringAny(raw, "api_refusal_explanation", "apiRefusalExplanation"), maxClaudeFallbackExplanationRunes)

	summary := truncate(strings.TrimSpace(readRawString(raw["content"])), maxClaudeFallbackReasonRunes)
	if summary == "" {
		named := originalModel
		if named == "" {
			named = "The model"
		}
		summary = named + " refused this request and no fallback model was available."
		if explanation != "" {
			summary += " " + explanation
		}
	}

	meta, _ := json.Marshal(map[string]any{
		"kind":                   "model_refusal_no_fallback",
		"originalModel":          originalModel,
		"reason":                 summary,
		"apiRefusalCategory":     category,
		"apiRefusalExplanation":  explanation,
		"refusedUserMessageUuid": boundedClaudeFallbackField(readRawStringAny(raw, "refused_user_message_uuid", "refusedUserMessageUuid"), maxClaudeFallbackIDRunes),
		"requestId":              boundedClaudeFallbackField(readRawStringAny(raw, "request_id", "requestId"), maxClaudeFallbackIDRunes),
	})

	return []provider.ProviderEvent{{
		Kind:      provider.EventError,
		ThreadID:  threadID,
		Content:   summary,
		Meta:      meta,
		Timestamp: now,
		Raw:       line,
	}}, nil
}

func boundedClaudeFallbackField(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes])
}

// claudeFallbackItemID builds the timeline row id for one member of the
// model-fallback family.
//
// The SUBTYPE is part of the id, not decoration. `request_id` names the
// API REQUEST, and one request can produce more than one notice from this
// family — a `model_consent_fallback` moving the session and a
// `model_refusal_fallback` for the same refused request both carry it.
// Triage UPSERTS on the item id, so a subtype-less id would let the
// second notice overwrite the first and the timeline would show one
// event where two happened.
//
// `subtype` needs no bounding or sanitising: the only caller is the
// dispatch switch in parseSystemEvent, which reaches this family through
// three string literals. A request id, by contrast, is wire-controlled,
// so it keeps its own charset + length gate and falls back to a hash of
// itself.
func claudeFallbackItemID(subtype, requestID string) string {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return ""
	}
	prefix := "model-fallback:" + subtype + ":"
	if len(requestID) <= maxClaudeFallbackIDRunes {
		valid := true
		for _, char := range requestID {
			if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
				(char >= '0' && char <= '9') || char == '_' || char == '-' || char == '.' || char == ':' {
				continue
			}
			valid = false
			break
		}
		if valid {
			return prefix + requestID
		}
	}
	sum := sha256.Sum256([]byte(requestID))
	return fmt.Sprintf("%ssha256:%x", prefix, sum[:16])
}

// Bounds for the permission-notice family. Every field is
// provider-authored prose or an identifier that becomes user-facing
// state, so each is capped before it reaches items.meta and the WS
// frame.
const (
	maxClaudePermissionToolNameRunes = 128
	maxClaudePermissionIDRunes       = 128
	maxClaudePermissionReasonRunes   = 500
	maxClaudePermissionCommands      = 16
	maxClaudePermissionCommandRunes  = 120
)

// permissionDeniedNoticeKind / permissionRetryNoticeKind are the
// `meta.kind` discriminators the notice family rides, mirroring the
// model-fallback family: one event kind, one meta.kind per wire subtype.
const (
	permissionDeniedNoticeKind = "permission_denied"
	permissionRetryNoticeKind  = "permission_retry"
)

// parsePermissionDeniedEvent handles `system/permission_denied` — a tool
// call the CLI refused BEFORE any interactive permission prompt, which
// is why the user never sees an approval card for it. Without this
// notice the only trace in AO is a tool_result carrying the CLI's
// rejection sentence, so a rule- or classifier-denied tool reads as "the
// model decided not to run it". Core principle 5: that is user-facing
// state.
//
// Wire shape (2.1.237). Two producers, one shape:
//
//   - the stream-json engine's CanUseTool wrapper —
//     `if(Ln.behavior==="deny"&&Czs(Ln))_e({type:"system",
//     subtype:"permission_denied",tool_name:ur.name,tool_use_id:xn,
//     agent_id:Wr.agentId,decision_reason_type:Ln.decisionReason?.type,
//     decision_reason:Y8n(Ln.decisionReason),message:Ln.message})`,
//     gated by `function Czs(e){return e.decideLocation==="pre-ask"}`
//   - the control-protocol client's `emitPermissionDenied(e,t,r,n)`,
//     which enqueues the same object plus `uuid` and `session_id`
//
// and the published schema (`NpH`):
//
//	{type:"system", subtype:"permission_denied",
//	 tool_name: string, tool_use_id: string,
//	 agent_id?: string,          // "Subagent ID when the denied tool call
//	                             //  originated inside a subagent"
//	 decision_reason_type?: string,
//	 decision_reason?: string,   // "Human-readable reason from the
//	                             //  deciding component"
//	 message: string,            // "The rejection message returned to the
//	                             //  model in the tool_result"
//	 uuid, session_id}
//
// `decision_reason_type` is the `PermissionDecisionReason` discriminator.
// The full union in 2.1.237 is
// `["rule","mode","subcommandResults","permissionPromptTool","hook",
// "asyncAgent","sandboxOverride","workingDir","safetyCheck","classifier",
// "other"]`, and it is an OPEN SET — an unrecognised value composes the
// generic sentence rather than being dropped.
//
// The `Czs` gate is the reason AO sees this at all AND the reason it is
// narrow: only a PRE-ASK decision emits. A denial the user makes in AO's
// own approval card is a post-ask decision and already has a row.
//
// **This envelope is live-wire only.** Both persistence paths drop it —
// the SDK message→transcript converter answers
// `if(e.subtype==="permission_denied")return{type:"ignored"}` and the
// REPL's append loop does `if(a.type==="system"&&
// a.subtype==="permission_denied")continue`. So it is absent from
// `~/.claude/projects/**.jsonl` and cannot be recovered on resume,
// import, or fork: whatever AO persists at receive time is the only
// record that will ever exist.
//
// Workspace-boundary vs rule denial. `decision_reason_type:"workingDir"`
// is the boundary case, and it is a genuinely different remedy from a
// rule denial, which is why the copy separates them. The CLI raises it
// as `{type:"workingDir",reason:"Path is outside allowed working
// directories"}`, and the SUGGESTIONS it pairs with that reason on the
// interactive path are `{type:"addDirectories",directories:[dir],
// destination:"session"}` (write/create) or an `addRules` entry for
// `Read` on `<dir>/**` (read) — never a `Bash(...)` rule. A user who
// answers a boundary denial by adding a tool rule has fixed nothing.
// Note that `blocked_path` itself rides `control_request/can_use_tool`,
// NOT this envelope: on a denial the reason TYPE is the only boundary
// signal, so that is what the flag is derived from.
func parsePermissionDeniedEvent(
	threadID string,
	raw map[string]json.RawMessage,
	now time.Time,
	line []byte,
) ([]provider.ProviderEvent, error) {
	toolName := boundedClaudeFallbackField(
		readRawStringAny(raw, "tool_name", "toolName"), maxClaudePermissionToolNameRunes)
	toolUseID := boundedClaudeFallbackField(
		readRawStringAny(raw, "tool_use_id", "toolUseId"), maxClaudePermissionIDRunes)
	agentID := boundedClaudeFallbackField(
		readRawStringAny(raw, "agent_id", "agentId"), maxClaudePermissionIDRunes)
	reasonType := boundedClaudeFallbackField(
		readRawStringAny(raw, "decision_reason_type", "decisionReasonType"), maxClaudeFallbackLabelRunes)
	reason := truncate(
		readRawStringAny(raw, "decision_reason", "decisionReason"), maxClaudePermissionReasonRunes)
	message := truncate(readRawStringAny(raw, "message"), maxClaudePermissionReasonRunes)

	// The detail line prefers `decision_reason` (the deciding component's
	// own words) over `message` (the sentence handed to the MODEL, which
	// is written to steer it rather than to inform the user). The CLI's
	// own debug renderer picks the same order:
	// `v1(r,"decision_reason",500)??v1(r,"message",500)`.
	detail := firstNonEmpty(reason, message)
	boundary := isClaudeWorkspaceBoundaryDenial(reasonType)

	fields := map[string]any{
		"kind":               permissionDeniedNoticeKind,
		"toolName":           toolName,
		"toolUseId":          toolUseID,
		"decisionReasonType": reasonType,
		"decisionReason":     detail,
	}
	if agentID != "" {
		fields["agentId"] = agentID
	}
	if boundary {
		// Recorded only when TRUE, like persistedAsDefault on the consent
		// fallback: false would claim "this is a rule denial", which an
		// unknown reason type does not establish.
		fields["workspaceBoundary"] = true
	}
	meta, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("parse permission_denied: marshal meta: %w", err)
	}

	return []provider.ProviderEvent{{
		Kind:     provider.EventNotification,
		ThreadID: threadID,
		// Deterministic and namespaced: a re-delivered envelope upserts
		// the same row instead of minting a second notice, and the prefix
		// keeps it off the tool_call row, whose id IS the bare
		// tool_use_id. Empty tool_use_id falls back to triage's per-turn
		// notification counter.
		ItemID:    permissionDeniedItemID(toolUseID),
		Content:   claudePermissionDeniedSummary(toolName, reasonType, boundary),
		Meta:      meta,
		Timestamp: now,
		Raw:       line,
	}}, nil
}

// permissionDeniedItemID namespaces the notice row by the tool call it
// explains. Returns "" when the envelope carried no tool_use_id, which
// hands triage its counter-allocated id instead.
func permissionDeniedItemID(toolUseID string) string {
	if toolUseID == "" {
		return ""
	}
	return "permission-denied:" + toolUseID
}

// isClaudeWorkspaceBoundaryDenial reports whether the denial was a
// workspace-boundary refusal (the path is outside the session's allowed
// directories) rather than a rule / mode / classifier decision. The two
// have different remedies — see parsePermissionDeniedEvent.
func isClaudeWorkspaceBoundaryDenial(reasonType string) bool {
	return strings.EqualFold(strings.TrimSpace(reasonType), "workingDir")
}

// claudePermissionDeniedSummary composes the notice's one-line sentence.
// The wire has no display sentence of its own for this subtype (the CLI
// renders it only in its debug log), so the copy is ours; the deciding
// component's words go on the detail line instead.
func claudePermissionDeniedSummary(toolName, reasonType string, boundary bool) string {
	if toolName == "" {
		toolName = "A tool call"
	}
	if boundary {
		return toolName + " was denied — the path is outside this workspace's allowed directories"
	}
	return toolName + " was denied by " + claudeDenialDecider(reasonType)
}

// claudeDenialDecider names WHO refused, from the
// `PermissionDecisionReason` discriminator. Unknown values fall through
// to the generic phrase: the union is open, and a novel type must still
// produce a sentence.
func claudeDenialDecider(reasonType string) string {
	switch strings.TrimSpace(reasonType) {
	case "rule":
		return "a permission rule"
	case "mode":
		return "the current permission mode"
	case "classifier":
		return "the permission classifier"
	case "hook":
		return "a PreToolUse hook"
	case "safetyCheck":
		return "a safety check"
	case "subcommandResults":
		return "a safety check on one of its subcommands"
	case "sandboxOverride":
		return "the sandbox policy"
	case "permissionPromptTool":
		return "the permission-prompt tool"
	case "asyncAgent":
		return "the agent runtime — it needs interactive approval, which this session cannot offer"
	default:
		return "the permission system"
	}
}

// parsePermissionRetryEvent handles `system/permission_retry`.
//
// Wire shape (2.1.237). Producer:
//
//	function u$m(e){return{type:"system",subtype:"permission_retry",
//	 content:`Allowed ${e.join(", ")}`,commands:e,level:"info",
//	 isMeta:!1,timestamp:…,uuid:…}}
//
// and the schema (`hu1`): `{type, subtype, content, commands: string[]
// ("Display names of the commands that were allowed."), uuid,
// session_id}`, described as "@internal Emitted when tool execution
// retries after a permission-mode change allowed previously-denied
// commands."
//
// Two things it does NOT carry, both worth stating because they are the
// natural guesses: there is **no tool_use_id** and **no attempt count**.
// The retry is per-COMMAND-NAME, not per tool call, so this notice
// cannot be attached to a row the way permission_denied can, and it
// stays a plain timeline notice.
//
// Its only producer in 2.1.237 is the REPL's "retry denials" dialog
// (`onRetryDenials`), which appends the message to the local transcript
// — so a headless stream-json session is unlikely to ever see it. It is
// parsed anyway because it is cheap, wire-typed, and the alternative is
// a silent drop if a future build routes it through the SDK path.
func parsePermissionRetryEvent(
	threadID string,
	raw map[string]json.RawMessage,
	now time.Time,
	line []byte,
) ([]provider.ProviderEvent, error) {
	commands := normalizePermissionRetryCommands(raw["commands"])
	summary := truncate(strings.TrimSpace(readRawString(raw["content"])), maxClaudePermissionReasonRunes)
	if summary == "" {
		if len(commands) == 0 {
			summary = "Retrying — a permission-mode change allowed previously-denied commands"
		} else {
			summary = "Retrying — allowed " + strings.Join(commands, ", ")
		}
	}
	fields := map[string]any{
		"kind": permissionRetryNoticeKind,
	}
	if len(commands) > 0 {
		fields["commands"] = commands
	}
	meta, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("parse permission_retry: marshal meta: %w", err)
	}
	return []provider.ProviderEvent{{
		Kind:      provider.EventNotification,
		ThreadID:  threadID,
		Content:   summary,
		Meta:      meta,
		Timestamp: now,
		Raw:       line,
	}}, nil
}

// normalizePermissionRetryCommands trims, bounds and caps the display
// names. A malformed array yields nil — the notice still renders from
// `content`.
func normalizePermissionRetryCommands(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var names []string
	if json.Unmarshal(raw, &names) != nil {
		return nil
	}
	out := make([]string, 0, min(len(names), maxClaudePermissionCommands))
	for _, name := range names {
		if len(out) >= maxClaudePermissionCommands {
			break
		}
		name = boundedClaudeFallbackField(name, maxClaudePermissionCommandRunes)
		if name == "" {
			continue
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func apiRetryPayload(raw map[string]json.RawMessage) json.RawMessage {
	if len(raw["data"]) > 0 {
		return raw["data"]
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	return encoded
}

// parseTaskStartedEvent handles `system/task_started`: the CLI's statement
// that a tool call has become a TASK, which is what makes the task_id ↔
// tool_use_id mapping available. Fires for EVERY Bash and Task, not just
// backgrounded ones.
//
// A sibling of parseTaskLifecycleEvent / parseTaskNotificationEvent rather
// than an inline case, because it is the only member of the family that
// carries real logic — the async-agent RESUME rebind (claude-wire.md §E6)
// and the background-carrier marking that follows from it.
func (p *Parser) parseTaskStartedEvent(
	threadID string,
	raw map[string]json.RawMessage,
	now time.Time,
) ([]provider.ProviderEvent, error) {
	taskID := readRawString(raw["task_id"])
	toolUseID := firstNonEmpty(readRawString(raw["tool_use_id"]), readRawString(raw["toolUseId"]))
	if taskID == "" || toolUseID == "" {
		return nil, nil
	}
	taskType := readRawString(raw["task_type"])

	// Resume-rebind detection (local_agent only). Claude's harness
	// lets the model resume an idle async agent via a follow-up
	// tool call (observed: SendMessage, `input.to: <agentId>`). On
	// resume the CLI re-fires task_started with the SAME task_id
	// but a DIFFERENT tool_use_id — the resuming tool's own call —
	// carrying the ORIGINAL agent's description/subagent_type. See
	// claude-wire.md §E6 and local_agent_async_resume.ndjson
	// (captured from AO thread 9941d40f, 2026-07-02).
	//
	// Two detection paths, both scoped to task_type=="local_agent"
	// (Bash's "local_bash" task_type never rebinds like this):
	//  1. In-memory: taskID is already bound to a DIFFERENT
	//     tool_use_id than this envelope carries.
	//  2. Reconnect: taskID has NO binding at all (a parser restart
	//     lost it), but toolUseID was never observed as an
	//     Agent-launch tool_use either (see isAgentLaunchTool) — the
	//     only way a "local_agent" task_started binds to a
	//     non-launch tool is a resume whose original launch
	//     predates this parser instance.
	var resumesToolUseID string
	var isResume bool
	if taskType == "local_agent" {
		existingRef := p.taskToolUseRef(taskID)
		switch {
		case existingRef.ToolUseID != "" && existingRef.ToolUseID != toolUseID:
			resumesToolUseID = existingRef.ToolUseID
			isResume = true
		case existingRef.ToolUseID == "" && !p.isAgentLaunchTool(toolUseID):
			// No binding at all: the original launch predates this
			// parser instance. Real trigger: a fresh process
			// resumed the session and the model re-resumed an
			// agent from its transcript (SendMessage to a dead
			// agent replies "resumed from transcript" — observed
			// live, thread 9941d40f). Mark the carrier below, but
			// resumesToolUseID stays "" since there is no original
			// tool_use_id to point triage at.
			//
			// Inferring "resume" from ABSENT state is sound here
			// because a fresh launch cannot reach this branch:
			// parser lifetime == CLI process lifetime (stdio), the
			// CLI emits task_started only AFTER the assistant
			// envelope carrying the tool_use, and ParseLine is
			// sequential — so any launch tool_use whose
			// task_started this process sees is already in
			// agentLaunchToolUses. local_agent tasks die with
			// their CLI process, so no pre-restart in-flight agent
			// can re-emit task_started on the new process either.
			// The only in-process loss window is the bounded-map
			// wholesale reset at parserTaskMapCap (1024 live
			// launches between a tool_use and its ~ms-later
			// task_started — practically unreachable).
			isResume = true
		}
	}

	p.rememberTaskToolUse(taskID, toolUseID)
	taskRef := p.taskToolUseRef(taskID)

	if isResume {
		// Route this tool_use through the SAME is_background
		// mechanism run_in_background launches use (parser.go
		// markBackground/isBackground). Fix B's async-ack path then
		// naturally emits this tool_use's EventToolComplete with
		// is_background:true, and triage's keep-running flip
		// (tool_lifecycle.go) makes it the resumed round's
		// background carrier — the row that keeps
		// ListRunningBackgroundToolCalls non-empty (reaper
		// protection) while the resumed agent runs quiet. Wire
		// order guarantees this works: assistant tool_use ->
		// task_started (here) -> tool_result ack. See
		// claude-wire.md §E6.
		p.markBackground(toolUseID)
	}

	// Re-emit an EventToolStart carrying task_id so triage can
	// persist the task_id ↔ tool_use_id mapping into items.meta.
	// On reconnect with a fresh in-memory parser, a later
	// task_updated carries only task_id; persisted meta lets triage
	// correlate back to the original tool_use item. The event is
	// minimal (no toolName/input) — triage merges task_id into the
	// existing item meta without clobbering the launch summary.
	metaFields := map[string]any{
		"task_id": taskID,
	}
	if taskType != "" {
		metaFields["task_type"] = taskType
	}
	if taskRef.ParentToolUseID != "" {
		metaFields["parent_tool_use_id"] = taskRef.ParentToolUseID
	}
	if isResume {
		// resumes_tool_use_id + description tie the carrier back to
		// the agent it's resuming — triage's keep-running flip uses
		// them to rewrite the carrier's Summary to the agent-centric
		// form ("Agent: <description>") instead of "SendMessage: …".
		// Only stamped on the resume path so a normal launch's
		// meta-update never gains an unrelated description field.
		if resumesToolUseID != "" {
			metaFields["resumes_tool_use_id"] = resumesToolUseID
		}
		if desc := readRawString(raw["description"]); desc != "" {
			metaFields["description"] = desc
		}
	}
	meta, _ := json.Marshal(metaFields)
	return []provider.ProviderEvent{{
		Kind:            provider.EventToolStart,
		ThreadID:        threadID,
		ItemID:          toolUseID,
		Meta:            meta,
		ParentToolUseID: taskRef.ParentToolUseID,
		Timestamp:       now,
	}}, nil
}

// parseTaskLifecycleEvent handles `system/task_updated`. A terminal
// `patch.status` (`completed`, `failed`, `killed`) emits
// `EventBackgroundTaskTerminal` for the backgrounded task — the
// authoritative basic-terminal signal for the task lifecycle. A later
// TaskOutput enrichment for the same task idempotently upserts through
// triage with richer payload. Non-terminal `patch.status` values
// (`pending`, `running`) are no-ops; dedup is triage's job.
// See docs/references/claude-wire.md §task_updated and
// docs/architecture/turn-lifecycle.md §Task lifecycle.
func (p *Parser) parseTaskLifecycleEvent(threadID string, raw map[string]json.RawMessage, now time.Time) ([]provider.ProviderEvent, error) {
	taskID := readRawString(raw["task_id"])
	if taskID == "" {
		return nil, nil
	}

	var patch map[string]json.RawMessage
	if json.Unmarshal(raw["patch"], &patch) != nil {
		return nil, nil
	}
	status := NormalizeTaskTerminalStatus(firstNonEmpty(
		readRawString(patch["status"]),
		readRawString(raw["status"]),
	))
	if status == "" {
		return nil, nil
	}

	// An empty tool_use_id here means the in-memory map is empty (fresh
	// adapter session after reconnect) AND the event did not echo the
	// id inline. Emit a terminal keyed only by task_id so triage can
	// look the row up via items.meta.task_id. If triage finds no match
	// the event is dropped there.
	taskRef := p.taskToolUseRef(taskID)
	toolUseID := firstNonEmpty(taskRef.ToolUseID, readRawString(raw["tool_use_id"]), readRawString(raw["toolUseId"]))
	parentToolUseID := firstNonEmpty(taskRef.ParentToolUseID, readRawString(raw["parent_tool_use_id"]), readRawString(raw["parentToolUseId"]))

	metaFields := map[string]any{
		"task_id": taskID,
		"status":  status,
		"source":  "task_updated",
	}
	if toolUseID != "" {
		metaFields["tool_use_id"] = toolUseID
	}
	if parentToolUseID != "" {
		metaFields["parent_tool_use_id"] = parentToolUseID
	}
	if status != "completed" {
		metaFields["is_error"] = true
	}
	if endTime, ok := readIntAtAnyKey(raw["patch"], "end_time", "endTime"); ok {
		metaFields["end_time"] = endTime
	}
	meta, _ := json.Marshal(metaFields)

	return []provider.ProviderEvent{{
		Kind:            provider.EventBackgroundTaskTerminal,
		ThreadID:        threadID,
		ItemID:          toolUseID,
		Content:         firstNonEmpty(readRawString(patch["description"]), readRawString(raw["summary"])),
		Meta:            meta,
		ParentToolUseID: parentToolUseID,
		Timestamp:       now,
	}}, nil
}

// parseTaskNotificationEvent surfaces Claude's non-lifecycle
// `system/task_notification` attention signal. This event must never be
// interpreted as task completion; triage persists it as a lightweight
// notification row and may read the referenced output_file into SQLite
// for later expansion on an already-terminal sibling row.
func (p *Parser) parseTaskNotificationEvent(threadID string, raw map[string]json.RawMessage, now time.Time) ([]provider.ProviderEvent, error) {
	taskID := readRawString(raw["task_id"])
	if taskID == "" {
		return nil, nil
	}
	fields := backgroundTaskNotificationFields{
		TaskID:          taskID,
		ToolUseID:       firstNonEmpty(readRawString(raw["tool_use_id"]), readRawString(raw["toolUseId"])),
		ParentToolUseID: firstNonEmpty(readRawString(raw["parent_tool_use_id"]), readRawString(raw["parentToolUseId"])),
		Status:          strings.TrimSpace(firstNonEmpty(readRawString(raw["status"]), readRawString(raw["patch"]))),
		OutputFile:      firstNonEmpty(readRawString(raw["output_file"]), readRawString(raw["outputFile"])),
		Summary:         readRawString(raw["summary"]),
	}
	return []provider.ProviderEvent{p.buildBackgroundTaskNotificationEvent(threadID, fields, now)}, nil
}

// backgroundTaskNotificationFields is the shared input shape both the
// structured `system/task_notification` path and the synthetic
// `<task-notification>` XML path in `parseUserReplay` use to build an
// EventBackgroundTaskNotification. Keeping a single builder eliminates
// drift between the two paths.
//
// ParentToolUseID is the wire-provided hint; an empty value falls back
// to the parser's task_id ↔ tool_use_id map. The synthetic XML path
// always passes "" because the wrapper doesn't expose it; the
// structured path defensively reads `parent_tool_use_id` /
// `parentToolUseId` from the envelope.
type backgroundTaskNotificationFields struct {
	TaskID          string
	ToolUseID       string
	ParentToolUseID string
	Status          string
	OutputFile      string
	Summary         string
}

// buildBackgroundTaskNotificationEvent assembles the
// EventBackgroundTaskNotification. The parser's task_id ↔ tool_use_id
// map resolves the tool_use_id and parent_tool_use_id when the wire
// caller didn't carry them inline. Callers are responsible for
// guaranteeing a non-empty TaskID — without it triage can't route the
// event.
func (p *Parser) buildBackgroundTaskNotificationEvent(threadID string, fields backgroundTaskNotificationFields, now time.Time) provider.ProviderEvent {
	taskRef := p.taskToolUseRef(fields.TaskID)
	toolUseID := firstNonEmpty(fields.ToolUseID, taskRef.ToolUseID)
	parentToolUseID := firstNonEmpty(fields.ParentToolUseID, taskRef.ParentToolUseID)

	metaFields := map[string]any{
		"task_id": fields.TaskID,
		"source":  "task_notification",
	}
	if toolUseID != "" {
		metaFields["tool_use_id"] = toolUseID
	}
	if parentToolUseID != "" {
		metaFields["parent_tool_use_id"] = parentToolUseID
	}
	if fields.Status != "" {
		metaFields["status"] = fields.Status
	}
	if fields.OutputFile != "" {
		metaFields["output_file"] = fields.OutputFile
	}
	meta, _ := json.Marshal(metaFields)

	return provider.ProviderEvent{
		Kind:            provider.EventBackgroundTaskNotification,
		ThreadID:        threadID,
		ItemID:          toolUseID,
		Content:         fields.Summary,
		Meta:            meta,
		ParentToolUseID: parentToolUseID,
		Timestamp:       now,
	}
}

// NormalizeTaskTerminalStatus maps a raw task status to the canonical
// terminal value (completed / killed / failed) or "" when the status is
// non-terminal or absent. Exported so the claude-tui provider can gate
// its reconstructed task_updated synthesis on exactly what this parser
// will accept as a terminal: a statusless `<task-notification>` (a stall
// progress ping) normalises to "" and must not synthesise a completion.
func NormalizeTaskTerminalStatus(status string) string {
	switch status {
	case "completed":
		return "completed"
	case "killed":
		// Preserve `killed` distinctly so triage can render a gray "Stopped"
		// badge instead of the generic red "Failed" bucket. The CLI emits
		// this on the follow-up task_updated that fires after a successful
		// stop_task control_request.
		return "killed"
	case "failed", "error", "errored", "interrupted", "stopped":
		return "failed"
	default:
		return ""
	}
}

// buildClaudeAPIRetryMeta normalizes a `system.api_retry` OR
// `system.api_error` payload into the shared {attempt, max_retries,
// error} EventAPIRetry meta shape. The two are wire twins for the same
// retryable failure (2.1.237) and disagree on every counter's spelling,
// so each counter accepts the whole alias set.
//
// Claude has also emitted both a nested `data` object and top-level retry
// fields across versions. The SDK's nested `data.error` is an object
// whose `.message` field carries the human-readable copy (`api_error`
// adds `.formatted`, the display string the CLI's own retry banner
// renders); newer top-level payloads can carry `error` as a flat string.
// Missing fields stay zero-valued so the triage handler treats them as
// "unknown" rather than fabricating a label.
func buildClaudeAPIRetryMeta(rawData json.RawMessage) json.RawMessage {
	fields := map[string]any{}
	if len(rawData) == 0 {
		return nil
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(rawData, &data) != nil {
		return rawData
	}
	// `attempt` is the SDK `api_retry` spelling; `retry_attempt` /
	// `retryAttempt` are the `api_error` twin's (schema and internal
	// object respectively).
	if attempt, ok := readIntValue(data, "attempt", "retry_attempt", "retryAttempt"); ok {
		fields["attempt"] = attempt
	}
	if maxRetries, ok := readIntValue(data, "max_retries", "maxRetries"); ok {
		fields["max_retries"] = maxRetries
	}
	if errMsg := readNestedErrorMessage(data); errMsg != "" {
		fields["error"] = errMsg
	}
	// `retry_delay_ms` is the SDK `api_retry` key, `retry_in_ms` /
	// `retryInMs` the `api_error` twin's. All three name the same
	// backoff, so they normalize onto one field.
	if retryAfter, ok := readIntValue(data,
		"retry_after_ms", "retryAfterMs",
		"retry_delay_ms", "retryDelayMs",
		"retry_in_ms", "retryInMs",
	); ok {
		fields["retry_after_ms"] = retryAfter
	}
	fields["wire"] = rawData
	out, err := json.Marshal(fields)
	if err != nil {
		return rawData
	}
	return out
}

// claudeAPIRetryFailure classifies the retryable failure behind an
// `api_retry` / `api_error` payload. `error_status` is the flat SDK
// spelling; the `api_error` twin nests the same value as `error.status`,
// so both are read — otherwise an overloaded-upstream 529 arriving only
// as the richer twin would lose its transient classification.
func claudeAPIRetryFailure(rawData json.RawMessage) *provider.FailureMeta {
	var data struct {
		ErrorStatus int             `json:"error_status"`
		Error       json.RawMessage `json:"error"`
	}
	if json.Unmarshal(rawData, &data) != nil {
		return nil
	}
	var detail struct {
		Status     int `json:"status"`
		Connection struct {
			Code string `json:"code"`
		} `json:"connection"`
	}
	hasDetail := json.Unmarshal(data.Error, &detail) == nil
	if data.ErrorStatus == 529 || (hasDetail && detail.Status == 529) {
		return &provider.FailureMeta{Class: provider.FailureTransient}
	}
	if hasDetail && detail.Connection.Code == "ECONNRESET" {
		return &provider.FailureMeta{Class: provider.FailureTransient}
	}
	return nil
}

// readNestedErrorMessage pulls the human copy out of a Claude
// `system.api_retry` / `system.api_error` error field. Nested payloads
// use `error:{message:string,name?:string}` — `api_error` additionally
// carries `formatted`, described upstream as "human-readable display
// string for the error", which is what the CLI's own retry banner shows,
// so it is preferred over the raw `message`. Top-level payloads observed
// in Claude 2.1.139 use a flat `error` string.
func readNestedErrorMessage(data map[string]json.RawMessage) string {
	raw, ok := data["error"]
	if !ok {
		return ""
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil && asString != "" {
		return asString
	}
	var asObj map[string]json.RawMessage
	if json.Unmarshal(raw, &asObj) != nil {
		return ""
	}
	return firstNonEmpty(readRawString(asObj["formatted"]), readRawString(asObj["message"]))
}

// extractSessionInfo reads fields from the init message top level.
func extractSessionInfo(raw map[string]json.RawMessage) provider.SessionInfo {
	var info provider.SessionInfo

	if v, ok := raw["session_id"]; ok {
		json.Unmarshal(v, &info.SessionID)
	}
	if v, ok := raw["model"]; ok {
		json.Unmarshal(v, &info.Model)
	}
	if v, ok := raw["cwd"]; ok {
		json.Unmarshal(v, &info.CWD)
	}
	if v, ok := raw["tools"]; ok {
		json.Unmarshal(v, &info.Tools)
	}
	if v, ok := raw["claude_code_version"]; ok {
		json.Unmarshal(v, &info.Version)
	}
	if v, ok := raw["mcp_servers"]; ok {
		var list []provider.MCPServer
		if json.Unmarshal(v, &list) == nil {
			info.MCPServers = list
		}
	}
	// `system/init` restates the fast-mode report at every session /
	// resume boundary, which is the only place a thread learns it before
	// its first turn ends. Left nil when the binary said nothing — see
	// extractFastModeStatus.
	if status, ok := extractFastModeStatus(raw); ok {
		info.FastMode = status
	}
	// Command discovery. All three lists are optional and version-dependent:
	// absence is NO SIGNAL, never "this session has no commands". A decode
	// failure leaves the field nil for the same reason — the init envelope's
	// job is the session id and the model, and a malformed cosmetic array must
	// not take those down with it.
	if v, ok := raw["slash_commands"]; ok {
		var names []string
		if json.Unmarshal(v, &names) == nil {
			info.SlashCommands = normalizeCommandNames(names)
		}
	}
	if v, ok := raw["skills"]; ok {
		var names []string
		if json.Unmarshal(v, &names) == nil {
			info.Skills = normalizeCommandNames(names)
		}
	}
	if v, ok := raw["plugins"]; ok {
		var plugins []provider.PluginInfo
		if json.Unmarshal(v, &plugins) == nil {
			info.Plugins = normalizePlugins(plugins)
		}
	}
	// `output_style` is the CLI's echo of the style this session launched
	// with (`"default"` when none is selected). AO sends the style through
	// the `--settings` block, but a project or user settings file can carry
	// one too, so the echo is the only statement of what actually applied.
	if v, ok := raw["output_style"]; ok {
		json.Unmarshal(v, &info.OutputStyle)
	}
	// `mcp_server_errors` (2.1.237): servers whose `--mcp-config` entry
	// failed validation and were SKIPPED. They are absent from
	// `mcp_servers[]`, so without this the app can only infer "never
	// connected" for a name it can see in the config — this is the only
	// wire surface carrying the reason. Same absence rule as the arrays
	// above; a decode failure leaves the field nil rather than taking the
	// session id and model down with it.
	if v, ok := raw["mcp_server_errors"]; ok {
		var errs []provider.MCPServerError
		if json.Unmarshal(v, &errs) == nil {
			info.MCPServerErrors = normalizeMCPServerErrors(errs)
		}
	}
	// `capabilities` (2.1.237): the protocol feature tokens this CLI
	// advertises. The init literal spreads it conditionally
	// (`...e.capabilities&&{capabilities:[...e.capabilities]}`), so an
	// ABSENT key is the normal shape on a build that has none — same
	// absence rule as the discovery arrays above, and a decode failure
	// leaves the field nil rather than taking the session id and model
	// down with it. See normalizeInitCapabilities for the bounds and the
	// area guide for the known tokens.
	if v, ok := raw["capabilities"]; ok {
		var names []string
		if json.Unmarshal(v, &names) == nil {
			info.Capabilities = normalizeInitCapabilities(names)
		}
	}

	return info
}

// Bounds for `system/init.capabilities`. The set is provider-authored
// and open-ended, and every token is held for the process's life and
// re-sent on every init, so both axes are capped.
const (
	maxClaudeInitCapabilities    = 64
	maxClaudeInitCapabilityRunes = 64
)

// normalizeInitCapabilities trims, drops empties, DEDUPES (init is
// re-emitted before every turn, and nothing downstream should have to
// care whether a build ever repeats a token) and caps the list. Order is
// the wire's, first occurrence wins, so the debug log reads like the
// envelope.
func normalizeInitCapabilities(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	out := make([]string, 0, min(len(names), maxClaudeInitCapabilities))
	seen := make(map[string]struct{}, min(len(names), maxClaudeInitCapabilities))
	for _, name := range names {
		if len(out) >= maxClaudeInitCapabilities {
			break
		}
		name = boundedClaudeFallbackField(name, maxClaudeInitCapabilityRunes)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// maxClaudeMCPServerErrorMessageRunes bounds the CLI-authored explanation
// before it becomes user-facing state. It is boundary input rendered in a
// status popup, and the cache holds it for the process's life.
const maxClaudeMCPServerErrorMessageRunes = 500

// normalizeMCPServerErrors trims and bounds each entry and drops the
// nameless ones: the name is the cache key, so an entry without one
// cannot be attributed to a server and would only be a mystery row.
func normalizeMCPServerErrors(errs []provider.MCPServerError) []provider.MCPServerError {
	out := make([]provider.MCPServerError, 0, len(errs))
	for _, e := range errs {
		name := strings.TrimSpace(e.Name)
		if name == "" {
			continue
		}
		out = append(out, provider.MCPServerError{
			Name:    name,
			Type:    boundedClaudeFallbackField(e.Type, maxClaudeFallbackLabelRunes),
			Message: truncate(strings.TrimSpace(e.Message), maxClaudeMCPServerErrorMessageRunes),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
