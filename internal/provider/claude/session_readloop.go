package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// readLoop reads stdout NDJSON lines and dispatches them as ProviderEvents.
func (s *Session) readLoop() {
	defer func() {
		if s.readDone != nil {
			defer close(s.readDone)
		}

		// Release any control_request callers still parked on a pending
		// control_response. If the subprocess died on its own (io.EOF,
		// crash) Close won't be the path that drains the map, so the
		// caller would otherwise sit idle until its own timeout fires.
		// Signalling here surfaces "session closed before response"
		// within a handful of milliseconds of the subprocess exit.
		s.clearPendingControlRequests()

		// If the CLI exits while an approval or user-input request is
		// waiting, resolve it as lost so the frontend prompt does not linger.
		s.clearPendingApprovals()

		if !s.closing.Load() {
			// Any read-loop exit while we weren't the one closing is
			// abnormal — including a clean exit-code-0 without a
			// host-initiated close. Triage gates synthesizing the
			// truncated turn-complete on this "error" signal, so a
			// missed emission leaves the FE working indicator stuck.
			// WaitProcessExitErr can return nil for a clean exit or for
			// a 100ms reap timeout; MarshalProcessExitMeta handles both.
			exitErr := provider.WaitProcessExitErr(s.proc)
			s.onEvent(provider.ProviderEvent{
				Kind:      provider.EventSessionStatus,
				ThreadID:  s.threadID,
				Content:   "error",
				Meta:      provider.MarshalProcessExitMeta(exitErr, s.proc.StderrTail()),
				Timestamp: time.Now(),
			})
		}

		s.onEvent(provider.ProviderEvent{
			Kind:      provider.EventSessionStatus,
			ThreadID:  s.threadID,
			Content:   "disconnected",
			Timestamp: time.Now(),
		})
	}()

	for {
		line, err := s.proc.ReadLine()
		if err != nil {
			if err != io.EOF {
				meta, _ := json.Marshal(map[string]any{"fatal": true})
				s.onEvent(provider.ProviderEvent{
					Kind:      provider.EventError,
					ThreadID:  s.threadID,
					Content:   fmt.Sprintf("claude: read error: %v", err),
					Meta:      meta,
					Failure:   &provider.FailureMeta{Class: provider.FailureFatal, Boundary: provider.FailureBoundaryEvent},
					Timestamp: time.Now(),
				})
			}
			return
		}

		// Leaf tracking happens inside s.parser.ParseLine below, off the
		// same decoded map the parser builds — no separate unmarshal here.

		// Gate control_request pre-handling on a byte-prefix check so
		// every streaming text_delta line doesn't pay an extra
		// json.Unmarshal. ParseLine below still handles the line if
		// the gate skips this branch.
		if bytes.HasPrefix(line, controlRequestPrefix) {
			var raw controlRequestEnvelope
			if err := json.Unmarshal(line, &raw); err != nil {
				log.Printf("claude: control_request handling error: %v", err)
			} else if raw.Type == "control_request" {
				handled, fatalMessage, err := s.handleControlRequest(raw)
				if err != nil {
					if fatalMessage != "" {
						meta, _ := json.Marshal(map[string]any{"fatal": true})
						s.onEvent(provider.ProviderEvent{
							Kind:      provider.EventError,
							ThreadID:  s.threadID,
							Content:   fmt.Sprintf("%s: %v", fatalMessage, err),
							Meta:      meta,
							Failure:   &provider.FailureMeta{Class: provider.FailureFatal, Boundary: provider.FailureBoundaryEvent},
							Timestamp: time.Now(),
						})
						_ = s.proc.Close()
						return
					}
					log.Printf("claude: control_request handling error: %v", err)
				}
				if handled {
					continue
				}
			}
		}

		// Same prefix gating for control_response — the CLI emits these
		// only in reply to our outbound control_requests. Parse once
		// here and deliver to the waiting caller so we don't pay a
		// second json.Unmarshal on the streaming hot path.
		if bytes.HasPrefix(line, controlResponsePrefix) {
			s.handleControlResponseLine(line)
			continue
		}

		// control_cancel_request: the CLI is abandoning a prior
		// can_use_tool callback (typically because of an interrupt).
		// Drain our pending approval / user-input state so the
		// frontend panel clears immediately. We DO NOT write a
		// response — the CLI is not waiting for one.
		if bytes.HasPrefix(line, controlCancelRequestPrefix) {
			s.handleControlCancelRequestLine(line)
			continue
		}

		events, err := s.parser.ParseLine(s.threadID, line)
		if err != nil {
			log.Printf("claude: parse error: %v (line: %s)", err, string(line[:min(len(line), 200)]))
			continue
		}

		for _, evt := range events {
			if evt.Kind == provider.EventInit && evt.Meta != nil {
				var info provider.SessionInfo
				if json.Unmarshal(evt.Meta, &info) == nil {
					if info.SessionID != "" {
						s.sessionID = info.SessionID
					}
					// `claude_code_version` is the only in-session
					// statement of which binary is actually serving this
					// process (the spawn-time `--version` probe answers
					// for the PATH, which a settings env block can
					// repoint). Absence is "unknown", never "old" —
					// every version gate treats unknown as too-old and
					// takes the restart path.
					s.noteCLIVersion(info.Version)
					// `system/init` is re-emitted before EVERY turn, so
					// this must be idempotent and must not re-fire
					// one-shot work — see noteCapabilities.
					s.noteCapabilities(info.Capabilities)
					// Empty means the envelope said nothing (older CLI),
					// never "no commands" — leave the set as-is so a
					// commands_changed replacement isn't undone.
					if len(info.SlashCommands) > 0 {
						s.replaceAdvertisedCommands(info.SlashCommands)
					}
				}
			}
			if evt.Kind == provider.EventCommandsChanged && evt.Meta != nil {
				var changed provider.CommandsChangedMeta
				if json.Unmarshal(evt.Meta, &changed) == nil {
					// Unlike init, an empty commands_changed list is a real
					// replacement (claude-wire.md: replace, never merge).
					names := make([]string, 0, len(changed.Commands))
					for _, cmd := range changed.Commands {
						names = append(names, cmd.Name)
					}
					s.replaceAdvertisedCommands(names)
				}
			}
			if evt.Kind == provider.EventApprovalRequest && evt.ItemID != "" {
				s.trackPendingApproval(evt.ItemID, provider.EventApprovalResolved)
			}
			if evt.Kind == provider.EventUserInputRequest && evt.ItemID != "" {
				var request provider.UserInputRequest
				_ = json.Unmarshal(evt.Meta, &request)
				s.trackPendingApprovalWithQuestions(evt.ItemID, provider.EventUserInputResolved, request.Questions)
			}
			if evt.Kind == provider.EventTurnComplete && s.leafTracker != nil {
				s.leafTracker.markTurnComplete()
			}
			s.onEvent(evt)
			if evt.Kind == provider.EventUserText {
				s.verifyReplayParent(evt)
			}
		}
	}
}

// replaceAdvertisedCommands swaps in a new provider-executed command name
// set. Callers hold no lock; the read loop is the only writer.
func (s *Session) replaceAdvertisedCommands(names []string) {
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	s.advertisedCommandsMu.Lock()
	s.advertisedCommands = set
	s.advertisedCommandsMu.Unlock()
}

// supportsSlashCommand reports whether the session has advertised the named
// provider-executed command. False both for "advertised list lacks it" and
// for "no list has arrived yet" — callers treat either as "not available"
// and take the restart path, so a pre-init session is handled conservatively
// rather than optimistically.
func (s *Session) supportsSlashCommand(name string) bool {
	s.advertisedCommandsMu.Lock()
	defer s.advertisedCommandsMu.Unlock()
	_, ok := s.advertisedCommands[name]
	return ok
}

// noteCLIVersion records the `claude_code_version` string a `system/init`
// envelope carried. Called on the read loop at every session / resume
// boundary; an empty value is ignored so a resume envelope that omits the
// key cannot un-learn a version an earlier one stated.
func (s *Session) noteCLIVersion(version string) {
	version = strings.TrimSpace(version)
	if version == "" {
		return
	}
	s.liveStateMu.Lock()
	s.cliVersion = version
	s.liveStateMu.Unlock()
}

// CLIVersion is the version string this process reported on `system/init`,
// or "" when no init has landed yet (or the build predates the key).
func (s *Session) CLIVersion() string {
	s.liveStateMu.Lock()
	defer s.liveStateMu.Unlock()
	return s.cliVersion
}
