package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// The `get_settings` control_request: the CLI's own answer to "what config is
// this process actually running", read on demand from a live session.
//
// It exists here because the alternative is worse. AO's live `/effort` and
// `/fast` applies are confirmed by MATCHING THE CLI'S ENGLISH REPLY TEXT
// ("Set effort level to high (this session only): …"), which is a UI string
// with no compatibility contract — a reworded reply reads as a rejection and
// costs the user a restart. `applied` is the structured statement of the same
// fact: "runtime-resolved values after env overrides, session state, and
// model-specific defaults are applied … what will actually be sent to the
// API" (schema doc, 2.1.237).
//
// Two things it is NOT:
//
//   - Not a poll. It is issued once per live apply that needs confirming (and
//     once more for the model read-back), never on a timer.
//   - Not a source of truth for FAST MODE. `applied` carries model, effort,
//     advisor and ultracode only; fast mode stays on its command reply plus
//     the passive `fast_mode_state` key every result envelope carries.
//
// Verified against the 2.1.237 bundle's Zod schema and its stdin-transport
// handler; the same schema and handler are present in 2.1.219 and 2.1.214.

// AppliedSettings is the `applied` object of a get_settings response: the
// runtime-resolved values, as opposed to `effective` (the raw disk merge).
type AppliedSettings struct {
	// Model is the model that will actually be sent to the API. This is the
	// one place the CLI states a FAMILY ALIAS STEP-DOWN: `set_model` answers
	// a bare success even when it resolved "sonnet" to a different concrete
	// model, so the request AO sent and the model running can legitimately
	// differ by more than normalization.
	Model string
	// Effort is the resolved reasoning tier, or "" when the model declares
	// none (the wire sends explicit null, which is a real answer — "this
	// model has no effort" — not a missing field).
	Effort string
	// Advisor is the advisor model attached to API requests, "" for none.
	// Optional on the wire: absent on workers predating the field.
	Advisor string
	// Ultracode reports the session's ultracode state (xhigh plus standing
	// dynamic-workflow orchestration). Optional on the wire.
	Ultracode bool
}

// SettingsSource is one entry of the `sources` array: the raw settings a
// single layer contributed, before merging. Ordered low-to-high priority.
type SettingsSource struct {
	// Source is one of userSettings, projectSettings, localSettings,
	// flagSettings, policySettings. Passed through as data, not an enum: a
	// layer added by a future release must not make the response undecodable.
	Source string
	// Settings is that layer's raw JSON object.
	Settings map[string]json.RawMessage
}

// SettingsError is one entry of the optional `errors` array. A file listed
// here was SKIPPED during the merge, so its settings are reflected in neither
// `effective` nor `sources`.
type SettingsError struct {
	File    string `json:"file"`
	Path    string `json:"path"`
	Message string `json:"message"`
}

// SettingsSnapshot is a decoded get_settings response.
type SettingsSnapshot struct {
	// Effective is the merged on-disk view. Kept raw — AO reads specific
	// keys, and the CLI's settings vocabulary is far wider than anything
	// modelled here.
	Effective map[string]json.RawMessage
	Sources   []SettingsSource
	// Applied is absent on a worker predating the field; nil means "this CLI
	// cannot tell us what it resolved", which is a fallback condition, not a
	// value.
	Applied *AppliedSettings
	Errors  []SettingsError
}

// The two `sources` keys AO also requests directly, and therefore the two a
// project-level settings file can silently outrank AO's intent on. The
// spellings are the CLI's own settings keys, NOT the wire names of AO's
// axes: `model` matches, effort is `effortLevel`.
const (
	settingsKeyModel  = "model"
	settingsKeyEffort = "effortLevel"
)

// projectScopedSettingsSources are the layers that come from the WORKSPACE
// rather than the user or from AO's own flags: `.claude/settings.json` and
// `.claude/settings.local.json`. A value here that disagrees with what AO
// asked for is a repository overriding the thread's config — worth surfacing,
// and never worth "fixing" by fighting it with more control requests.
var projectScopedSettingsSources = map[string]struct{}{
	"projectSettings": {},
	"localSettings":   {},
}

// SettingsOverrideNotice records one project-scoped settings value that
// differs from what AO requested for the session. Observation only: nothing
// acts on it, and the CLI's merge has already decided who wins.
type SettingsOverrideNotice struct {
	// Source is "projectSettings" or "localSettings".
	Source string
	// Field is the CLI settings key ("model" or "effortLevel").
	Field string
	// Requested is the value AO asked the session to run.
	Requested string
	// Configured is the value the project settings file carries.
	Configured string
	// ObservedAt stamps the read-back that found it.
	ObservedAt time.Time
}

// ErrGetSettingsUnsupported is returned by GetSettings when this CLI does not
// implement the subtype. Callers fall back to their pre-structured
// confirmation path; the session remembers the answer so it is asked once.
var ErrGetSettingsUnsupported = errors.New("claude: get_settings is not supported by this CLI")

// GetSettings reads the live session's effective settings and the values it
// actually resolved. Out of band: no turn is consumed and no API call is
// made.
//
// MUST NOT be called from the read-loop goroutine. The response arrives on
// that loop, so a synchronous round-trip issued from an event callback
// deadlocks until the control timeout fires.
//
// A CLI that does not implement the subtype answers `control_response`
// `subtype:"error"` with "Unsupported control request subtype: …". That is
// remembered per session (getSettingsUnsupported) and every later call short
// -circuits to ErrGetSettingsUnsupported without touching the wire — the
// answer cannot change while the process lives, and re-asking would put a
// failing request on stdin before every confirmation.
func (s *Session) GetSettings(ctx context.Context) (*SettingsSnapshot, error) {
	if s.GetSettingsUnsupported() {
		return nil, ErrGetSettingsUnsupported
	}
	res, err := s.sendControlRequest(ctx, "get_settings", map[string]any{
		"subtype": "get_settings",
	})
	if err != nil {
		return nil, err
	}
	if !res.ok {
		if isUnsupportedSubtypeError(res.errMsg) {
			s.markGetSettingsUnsupported()
			return nil, ErrGetSettingsUnsupported
		}
		if res.errMsg == "" {
			return nil, fmt.Errorf("claude: get_settings: provider returned unspecified error")
		}
		return nil, fmt.Errorf("claude: get_settings: %s", res.errMsg)
	}
	snapshot, err := ParseSettingsSnapshot(res.payload)
	if err != nil {
		return nil, err
	}
	s.recordSettingsSnapshot(snapshot)
	return snapshot, nil
}

// unsupportedSubtypeErrorPrefix is how the CLI reports a control_request
// subtype it does not implement ("Unsupported control request subtype: x").
// Matched case-insensitively on the prefix only: the suffix is the subtype
// name and the casing is not a contract.
const unsupportedSubtypeErrorPrefix = "unsupported control request subtype"

func isUnsupportedSubtypeError(msg string) bool {
	return strings.Contains(strings.ToLower(msg), unsupportedSubtypeErrorPrefix)
}

// ParseSettingsSnapshot decodes the `response.response` object of a
// successful get_settings round-trip. Exported so the shape can be tested
// without standing up a subprocess.
//
// Decoding is deliberately permissive everywhere except the envelope itself:
// unknown sources, unknown settings keys, and an absent `applied` are all
// normal across versions, and none of them should turn a usable answer into
// an error.
func ParseSettingsSnapshot(payload json.RawMessage) (*SettingsSnapshot, error) {
	if len(payload) == 0 {
		return nil, fmt.Errorf("claude: get_settings: empty response payload")
	}
	var decoded struct {
		Effective map[string]json.RawMessage `json:"effective"`
		Sources   []struct {
			Source   string                     `json:"source"`
			Settings map[string]json.RawMessage `json:"settings"`
		} `json:"sources"`
		Applied *struct {
			Model     string  `json:"model"`
			Effort    *string `json:"effort"`
			Advisor   *string `json:"advisor"`
			Ultracode *bool   `json:"ultracode"`
		} `json:"applied"`
		Errors []SettingsError `json:"errors"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, fmt.Errorf("claude: get_settings: decode response: %w", err)
	}

	snapshot := &SettingsSnapshot{
		Effective: decoded.Effective,
		Errors:    decoded.Errors,
	}
	for _, src := range decoded.Sources {
		snapshot.Sources = append(snapshot.Sources, SettingsSource{
			Source:   src.Source,
			Settings: src.Settings,
		})
	}
	if decoded.Applied != nil {
		applied := &AppliedSettings{Model: decoded.Applied.Model}
		if decoded.Applied.Effort != nil {
			applied.Effort = *decoded.Applied.Effort
		}
		if decoded.Applied.Advisor != nil {
			applied.Advisor = *decoded.Applied.Advisor
		}
		if decoded.Applied.Ultracode != nil {
			applied.Ultracode = *decoded.Applied.Ultracode
		}
		snapshot.Applied = applied
	}
	return snapshot, nil
}

// ProjectOverrides reports the project-scoped settings values that disagree
// with what AO requested for this session. wantModel / wantEffort are AO's
// requested values; an empty one is not compared (AO expressed no intent).
//
// The comparison is on the RAW settings value, not on who won the merge: a
// project file naming a different model is worth knowing about even when a
// higher-priority layer overrode it, because the next spawn's layering can
// differ. `[1m]` markers are trimmed off the model before comparing so an
// extended-context request does not read as a project override of itself.
func (s *SettingsSnapshot) ProjectOverrides(wantModel, wantEffort string, now time.Time) []SettingsOverrideNotice {
	var notices []SettingsOverrideNotice
	for _, src := range s.Sources {
		if _, ok := projectScopedSettingsSources[src.Source]; !ok {
			continue
		}
		if wantModel != "" {
			if got, ok := settingsString(src.Settings, settingsKeyModel); ok &&
				provider.NormalizeModelSlug(string(provider.Claude), got) !=
					provider.NormalizeModelSlug(string(provider.Claude), wantModel) {
				notices = append(notices, SettingsOverrideNotice{
					Source:     src.Source,
					Field:      settingsKeyModel,
					Requested:  wantModel,
					Configured: got,
					ObservedAt: now,
				})
			}
		}
		if wantEffort != "" {
			if got, ok := settingsString(src.Settings, settingsKeyEffort); ok && got != wantEffort {
				notices = append(notices, SettingsOverrideNotice{
					Source:     src.Source,
					Field:      settingsKeyEffort,
					Requested:  wantEffort,
					Configured: got,
					ObservedAt: now,
				})
			}
		}
	}
	return notices
}

// settingsString reads one string-valued key out of a raw settings map. A
// key present with a non-string value is reported as absent: the CLI's own
// schema `.catch(void 0)`s a malformed `effortLevel`, so a wrong-typed value
// means the same thing to it as no value at all.
func settingsString(settings map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := settings[key]
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	return value, true
}

// GetSettingsUnsupported reports whether this session has already answered a
// get_settings request with an unsupported-subtype error. Callers use it to
// skip the round-trip entirely rather than putting a request they know will
// fail on stdin; GetSettings itself short-circuits on the same flag.
//
// Exported directly rather than through an unexported twin: one accessor for
// one boolean, so a reader does not have to establish that the two agree.
func (s *Session) GetSettingsUnsupported() bool {
	s.liveStateMu.Lock()
	defer s.liveStateMu.Unlock()
	return s.getSettingsUnsupported
}

func (s *Session) markGetSettingsUnsupported() {
	s.liveStateMu.Lock()
	s.getSettingsUnsupported = true
	s.liveStateMu.Unlock()
}

// recordSettingsSnapshot stores the applied values from a successful
// read-back, plus any project override the snapshot names against the
// session's own launch intent. Both are observation surfaces only.
func (s *Session) recordSettingsSnapshot(snapshot *SettingsSnapshot) {
	if snapshot == nil {
		return
	}
	s.configModelMu.Lock()
	wantModel, wantEffort := s.configModel, s.requestedEffort
	s.configModelMu.Unlock()

	notices := snapshot.ProjectOverrides(wantModel, wantEffort, time.Now())

	s.liveStateMu.Lock()
	if snapshot.Applied != nil {
		applied := *snapshot.Applied
		s.appliedSettings = &applied
	}
	s.settingsOverrides = notices
	s.liveStateMu.Unlock()
}

// AppliedSettingsSnapshot returns the last `applied` object read back from
// the CLI, or nil when none has been read (or the CLI predates the field).
func (s *Session) AppliedSettingsSnapshot() *AppliedSettings {
	s.liveStateMu.Lock()
	defer s.liveStateMu.Unlock()
	if s.appliedSettings == nil {
		return nil
	}
	applied := *s.appliedSettings
	return &applied
}

// SettingsOverrides returns the project-scoped settings values the last
// read-back found disagreeing with AO's requested config. Empty when none
// were found or nothing has been read yet.
func (s *Session) SettingsOverrides() []SettingsOverrideNotice {
	s.liveStateMu.Lock()
	defer s.liveStateMu.Unlock()
	return append([]SettingsOverrideNotice(nil), s.settingsOverrides...)
}
