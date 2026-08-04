package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/codexskills"
)

// skillsListMethod enumerates the skills visible from a set of working
// directories.
//
// `SkillsList => "skills/list"` with `serialization:
// global_shared_read("config")`
// (codex-rs/app-server-protocol/src/protocol/common.rs, rust-v0.146.0-alpha.4),
// so it is global: no thread, no turn, no model tokens, and no
// `#[experimental]` gate. It has shipped since codex 0.73.0, far below AO's
// 0.143 provider floor, so there is no runtime capability probe.
//
// Skills REPLACED custom prompts, which upstream removed in 0.118 — there
// is no `customPrompts/list` to fall back to.
const skillsListMethod = "skills/list"

// skillsChangedMethod is the app-server's invalidation signal for the
// skills list. Upstream types it as an EMPTY struct
// (`SkillsChangedNotification {}`) and documents it as "treat this as an
// invalidation signal and re-run `skills/list` with the client's current
// parameters" — it carries no cwd, no scope and no skill name, so a
// consumer cannot narrow the drop and must not pretend to.
const skillsChangedMethod = "skills/changed"

// DefaultSkillsListTimeout bounds the ephemeral read. `skills/list` is
// local — it walks directories and parses SKILL.md/SKILL.json — so the
// ceiling only has to cover a cold binary start plus a filesystem scan of
// the requested roots.
const DefaultSkillsListTimeout = 15 * time.Second

// skillsListResponse mirrors `SkillsListResponse`
// (codex-rs/app-server-protocol/src/protocol/v2/plugin.rs). Only the
// fields codexskills.Skill exposes are modelled; icons, brand colour and
// tool dependencies are on the wire but have no AO surface.
type skillsListResponse struct {
	Data []skillsListEntry `json:"data"`
}

type skillsListEntry struct {
	Cwd    string           `json:"cwd"`
	Skills []skillMetadata  `json:"skills"`
	Errors []skillErrorInfo `json:"errors"`
}

type skillMetadata struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// ShortDescription is the legacy SKILL.md field. Upstream's own comment
	// says to prefer interface.shortDescription, which is what
	// projectSkill does.
	ShortDescription string          `json:"shortDescription"`
	Interface        *skillInterface `json:"interface"`
	Path             string          `json:"path"`
	Scope            string          `json:"scope"`
	Enabled          bool            `json:"enabled"`
}

type skillInterface struct {
	DisplayName      string `json:"displayName"`
	ShortDescription string `json:"shortDescription"`
	DefaultPrompt    string `json:"defaultPrompt"`
}

type skillErrorInfo struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// buildSkillsListParams validates the request and shapes it for the wire.
//
// Every cwd must be absolute. Upstream resolves each entry with
// `AbsolutePathBuf::relative_to_current_dir`
// (app-server/src/request_processors/catalog_processor.rs), so a relative
// path silently means a different directory depending on which process
// answered — a live session's workspace versus an ephemeral fetcher's
// WorkDir. Rejecting it here is the only place that difference can be made
// impossible rather than merely unlikely.
//
// An empty cwds list is also rejected, even though the wire treats it as
// "the session's cwd": that default is a property of the answering
// process, and AO's whole point is that either process may answer.
func buildSkillsListParams(cwds []string, forceReload bool) (map[string]any, []string, error) {
	if len(cwds) == 0 {
		return nil, nil, fmt.Errorf("codex: %s: at least one cwd required", skillsListMethod)
	}
	cleaned := make([]string, 0, len(cwds))
	for _, cwd := range cwds {
		cwd = strings.TrimSpace(cwd)
		if cwd == "" {
			return nil, nil, fmt.Errorf("codex: %s: empty cwd", skillsListMethod)
		}
		if !filepath.IsAbs(cwd) {
			return nil, nil, fmt.Errorf("codex: %s: cwd %q must be absolute", skillsListMethod, cwd)
		}
		cleaned = append(cleaned, cwd)
	}
	params := map[string]any{"cwds": cleaned}
	if forceReload {
		params["forceReload"] = true
	}
	return params, cleaned, nil
}

// parseSkillsList projects the wire response onto the caller-facing type.
//
// A skill missing either `name` or `path` is dropped: both are required to
// invoke it (the `$name` text token and the structured input's path), so
// offering one would produce a menu entry that cannot be used. Dropping is
// silent by design here — the per-cwd `errors` array is Codex's own
// channel for "this directory did not load", and a well-formed entry with
// a blank name is a server bug, not a user-actionable one.
func parseSkillsList(raw json.RawMessage) ([]codexskills.CwdSkills, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("codex: %s: empty response", skillsListMethod)
	}
	var wire skillsListResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("codex: decode %s response: %w", skillsListMethod, err)
	}
	out := make([]codexskills.CwdSkills, 0, len(wire.Data))
	for _, entry := range wire.Data {
		projected := codexskills.CwdSkills{
			Cwd:    entry.Cwd,
			Skills: make([]codexskills.Skill, 0, len(entry.Skills)),
			Errors: make([]codexskills.LoadError, 0, len(entry.Errors)),
		}
		for _, skill := range entry.Skills {
			if projected2, ok := projectSkill(skill); ok {
				projected.Skills = append(projected.Skills, projected2)
			}
		}
		for _, loadErr := range entry.Errors {
			message := strings.TrimSpace(loadErr.Message)
			if message == "" {
				continue
			}
			projected.Errors = append(projected.Errors, codexskills.LoadError{
				Path:    strings.TrimSpace(loadErr.Path),
				Message: message,
			})
		}
		out = append(out, projected)
	}
	return out, nil
}

func projectSkill(wire skillMetadata) (codexskills.Skill, bool) {
	name := strings.TrimSpace(wire.Name)
	path := strings.TrimSpace(wire.Path)
	if name == "" || path == "" {
		return codexskills.Skill{}, false
	}
	skill := codexskills.Skill{
		Name:             name,
		Description:      strings.TrimSpace(wire.Description),
		ShortDescription: strings.TrimSpace(wire.ShortDescription),
		Path:             path,
		Scope:            strings.TrimSpace(wire.Scope),
		Enabled:          wire.Enabled,
	}
	if wire.Interface != nil {
		skill.DisplayName = strings.TrimSpace(wire.Interface.DisplayName)
		skill.DefaultPrompt = strings.TrimSpace(wire.Interface.DefaultPrompt)
		// Upstream's stated preference: SKILL.json's interface wins over
		// the legacy top-level short_description.
		if short := strings.TrimSpace(wire.Interface.ShortDescription); short != "" {
			skill.ShortDescription = short
		}
	}
	return skill, true
}

// ListSkills asks a LIVE session's app-server which skills are visible
// from each of the given directories.
//
// Preferred over the ephemeral fetcher whenever a Codex session is open:
// the method is global, touches no thread state and starts no turn, so it
// costs one JSON-RPC round trip instead of a subprocess plus a second
// handshake. It is safe mid-turn for the same reason.
//
// cwds must be non-empty and absolute — see buildSkillsListParams.
// forceReload bypasses the app-server's own on-disk skill cache; leave it
// false for ordinary reads.
func (s *Session) ListSkills(ctx context.Context, cwds []string, forceReload bool) ([]codexskills.CwdSkills, error) {
	params, _, err := buildSkillsListParams(cwds, forceReload)
	if err != nil {
		return nil, err
	}
	raw, err := s.sendRequest(ctx, skillsListMethod, params)
	if err != nil {
		return nil, fmt.Errorf("codex: %s: %w", skillsListMethod, err)
	}
	return parseSkillsList(raw)
}

// SkillsFetcher runs a short-lived `codex app-server`, performs the
// initialize handshake, calls `skills/list`, and tears the process down.
// Used when no Codex session is live.
//
// It spawns through provider.Spawn — the same path every other Codex
// read takes — so it inherits the CODEX_HOME unset and the per-provider
// environment pins. No `thread/start` is issued, so no turn is billed and
// no provider state is mutated.
type SkillsFetcher struct {
	Binary string
	// WorkDir is the subprocess's working directory. It does not decide
	// which directories are scanned (the request names those explicitly),
	// but Codex layers project configuration by walking up from its cwd,
	// so leaving it inherited would let the launch directory influence
	// which skills are enabled. Empty defaults to the first requested cwd,
	// which is the value that makes an ephemeral read match what a session
	// in that workspace would have answered.
	WorkDir string
	Env     map[string]string
	Timeout time.Duration // 0 → DefaultSkillsListTimeout
}

// Fetch reads the skills list from a throwaway app-server.
func (f *SkillsFetcher) Fetch(ctx context.Context, cwds []string, forceReload bool) ([]codexskills.CwdSkills, error) {
	if strings.TrimSpace(f.Binary) == "" {
		return nil, fmt.Errorf("codex: %s: binary path required", skillsListMethod)
	}
	params, cleanedCwds, err := buildSkillsListParams(cwds, forceReload)
	if err != nil {
		return nil, err
	}
	workDir := strings.TrimSpace(f.WorkDir)
	if workDir == "" {
		workDir = cleanedCwds[0]
	}
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = DefaultSkillsListTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// No experimentalApi: skills/list is not `#[experimental]`, and opting
	// a throwaway process into the experimental surface would only change
	// what the server is willing to emit. No notifications are awaited
	// either — the invalidation signal belongs to live sessions.
	client, err := startOneshotClient(ctx, oneshotSpec{
		Binary:     f.Binary,
		WorkDir:    workDir,
		Env:        f.Env,
		ClientName: "agent_overflow_skills",
		Label:      skillsListMethod,
	})
	if err != nil {
		return nil, err
	}
	defer client.close()

	raw, err := client.request(ctx, skillsListMethod, params)
	if err != nil {
		return nil, fmt.Errorf("codex: %s: %w", skillsListMethod, err)
	}
	return parseSkillsList(raw)
}

// SkillsChangedHandler observes the app-server's skills invalidation
// signal. It takes no arguments because the notification carries none.
type SkillsChangedHandler func()

// SetSkillsChangedHandler installs an observer for `skills/changed`.
// Registered by app_session.go right after NewSession; the notification
// only fires on a filesystem change the watcher sees, so a handler
// installed immediately after the handshake cannot miss one that matters.
func (s *Session) SetSkillsChangedHandler(h SkillsChangedHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.skillsChangedHandler = h
}

// dispatchSkillsChanged forwards the invalidation signal. Params are
// deliberately ignored: upstream types the notification as an empty struct,
// so decoding it would only invent a shape to depend on.
func (s *Session) dispatchSkillsChanged(json.RawMessage) {
	s.mu.Lock()
	handler := s.skillsChangedHandler
	s.mu.Unlock()
	if handler == nil {
		return
	}
	handler()
}
