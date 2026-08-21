package claude

import (
	"log"
	"sort"
	"strings"
)

// `system/init.capabilities` — the CLI's own feature-detection surface.
//
// The published schema (2.1.237) describes it as: "Protocol capabilities
// this CLI supports, so SDK consumers can feature-detect instead of
// version-sniffing. Open set — ignore unknown values; check each
// capability for exactly the behavior you use." Nothing in AO is keyed
// on it yet; it exists so the NEXT version gate can ask the process what
// it does instead of parsing `claude_code_version`, which answers for a
// binary a settings `env` block can repoint.
//
// Known tokens, and what each one certifies (all quoted from the 2.1.237
// schema):
//
//   - interrupt_receipt_v1        — "the interrupt control_response
//     success payload carries still_queued (uuids of async user messages
//     that survive the interrupt)". Older CLIs answer an empty success
//     with no still_queued field.
//   - interrupt_cancel_queued_v1  — "the interrupt control_request honors
//     cancel_queued:true (queued and pending-dispatch commands are
//     cancelled alongside the abort, listed on the response's cancelled
//     field)". Older CLIs ignore the field.
//   - msg_lifecycle_v1            — the `command_lifecycle` envelope
//     family (queued / started / completed / cancelled / discarded).
//   - queued_notifications        — "the CLI accepts inbound
//     queued_notification stream messages and drains them via
//     ReadNotifications". Remote-control backend only.
//
// ⚠ The list a build ADVERTISES is narrower than the list it IMPLEMENTS.
// On 2.1.237 the stream-json engine — the transport AO uses — passes a
// two-element constant (`lvE=[JOl,ZOl]` = interrupt_receipt_v1,
// msg_lifecycle_v1); the three-element constant that also names
// interrupt_cancel_queued_v1 (`ssh=[JOl,beE,ZOl]`) is declared and never
// referenced, and `queued_notifications` appears only in the schema
// text. So absence of a token is NOT proof the behaviour is missing, and
// a gate written the other way round — "refuse unless advertised" —
// would disable a working feature on this exact build. Prefer a
// capability check to a version parse when the token is present; keep
// the version floor as the fallback when it is not.

// noteCapabilities records what a `system/init` envelope advertised.
//
// Idempotent by construction, which is load-bearing: the CLI re-emits
// `system/init` before every turn, so this runs once per turn for the
// life of the process. A repeat with the same tokens changes nothing and
// logs nothing; only the FIRST init that carries a non-empty set logs,
// and the one-shot latch is never cleared while the session lives.
//
// An EMPTY set is "the envelope said nothing" (older CLI, or a build with
// no tokens), never "this session lost its capabilities" — the stored set
// is left alone, exactly like the slash-command list.
func (s *Session) noteCapabilities(names []string) {
	if s == nil || len(names) == 0 {
		return
	}
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			set[name] = struct{}{}
		}
	}
	if len(set) == 0 {
		return
	}

	s.liveStateMu.Lock()
	s.capabilities = set
	shouldLog := !s.capabilitiesLogged
	s.capabilitiesLogged = true
	s.liveStateMu.Unlock()

	if shouldLog {
		log.Printf("claude: session %s advertises capabilities: %s",
			s.threadID, strings.Join(sortedCapabilityNames(set), ", "))
	}
}

// HasCapability reports whether the running CLI advertised the named
// protocol capability on `system/init`.
//
// FALSE means "not advertised", which — see the note above — is weaker
// than "not supported": on 2.1.237 the stream-json engine omits a token
// for behaviour it implements. Use this to LIGHT UP a newer path, never
// to refuse an older one, and keep the version floor as the fallback.
// Nil-session and pre-init calls both answer false.
func (s *Session) HasCapability(name string) bool {
	if s == nil {
		return false
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	s.liveStateMu.Lock()
	defer s.liveStateMu.Unlock()
	_, ok := s.capabilities[name]
	return ok
}

// Capabilities returns a sorted copy of the advertised token set, for
// diagnostics. Empty until the first `system/init` lands.
func (s *Session) Capabilities() []string {
	if s == nil {
		return nil
	}
	s.liveStateMu.Lock()
	defer s.liveStateMu.Unlock()
	return sortedCapabilityNames(s.capabilities)
}

func sortedCapabilityNames(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
