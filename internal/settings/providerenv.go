package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"

	"agent-overflow/internal/provider"
)

// ProviderEnvVar is one user-defined environment variable applied to the
// provider subprocesses Agent Overflow spawns for that provider — the chat
// session, the account/identity/rate-limit probes, and the text-generation
// CLI. Use cases are backend redirection (`ANTHROPIC_BASE_URL`) and corporate
// proxy configuration (`HTTPS_PROXY`, `NO_PROXY`, custom CA bundles).
//
// SECURITY: Value is persisted verbatim in settings.json, in plaintext, like
// every other field on Settings — see the SECURITY NOTE on the Settings
// struct. Sensitive=true is a DISCLOSURE control, not an encryption one: it
// keeps the value off the `GetSettings` wire shape (see RedactProviderEnvVars)
// so a LAN-attached token-holder can't harvest it, and the UI renders it
// masked. It does not make settings.json a safe place for a long-lived
// credential.
//
// List, not map[string]string, deliberately:
//
//   - A map cannot carry the per-variable Sensitive flag without becoming a
//     map of structs, at which point it has a list's shape and a map's
//     drawbacks.
//   - A map silently collapses duplicate names at JSON decode (last wins), so
//     `PROXY` typed twice would be applied without the user ever learning one
//     of their two entries was dropped. A list carries both to the validator,
//     which rejects the pair with a message naming the collision.
//   - Order is the user's; a map has none, so the settings UI would reshuffle
//     rows on every save.
//
// Application order is irrelevant to behaviour because duplicates (including
// case-insensitive ones) are rejected before anything is spawned.
type ProviderEnvVar struct {
	Name      string `json:"name"`
	Value     string `json:"value"`
	Sensitive bool   `json:"sensitive,omitempty"`
}

const (
	// MaxProviderEnvVars caps the per-provider list. Real configurations use a
	// handful (base URL, proxy pair, CA bundle); anything past this is a
	// corrupt or hand-mangled file.
	MaxProviderEnvVars = 32
	// MaxProviderEnvNameLength is generous next to every real env name and
	// still bounds what a hand-edited file can push into an argv-adjacent
	// buffer.
	MaxProviderEnvNameLength = 128
	// MaxProviderEnvValueLength leaves room for a PEM-encoded CA bundle
	// inlined as a value, which is the largest legitimate use we know of.
	MaxProviderEnvValueLength = 8192
)

// The names a custom environment may not set are provider's table
// (internal/provider/envtable.go): every variable an Agent Overflow spawn path
// sets or clears deliberately is declared there, so a user override that would
// be silently discarded, or would defeat a documented invariant, is refused
// here from the same list. Rejecting at validation time is the point — a name
// that is silently dropped at spawn time looks like a bug in the feature.

// providerEnvSettingsField returns the settings JSON key that stores the
// provider's custom environment, and the canonical provider key used for
// reserved-name lookups. claude-tui shares Claude's list: one binary, one backend,
// so a base URL that moves one must move the other.
func providerEnvSettingsField(providerName string) (field, key string, err error) {
	switch strings.TrimSpace(providerName) {
	case "claude", "claude-tui":
		return "claudeCustomEnv", "claude", nil
	case "codex":
		return "codexCustomEnv", "codex", nil
	case "agy":
		return "agyCustomEnv", "agy", nil
	default:
		return "", "", fmt.Errorf("settings: %q has no custom environment", providerName)
	}
}

// ProviderEnvVars returns the stored custom environment for a provider, in the
// user's order. Unknown providers get nil rather than an error: read callers
// are spawn paths that must keep working for a provider without the feature.
func (s Settings) ProviderEnvVars(providerName string) []ProviderEnvVar {
	_, key, err := providerEnvSettingsField(providerName)
	if err != nil {
		return nil
	}
	switch key {
	case "claude":
		return s.ClaudeCustomEnv
	case "codex":
		return s.CodexCustomEnv
	case "agy":
		return s.AgyCustomEnv
	}
	return nil
}

// ProviderEnvMap returns the provider's custom environment as the map
// provider.BuildEnvironment takes in EnvInput.Custom. nil when nothing is
// configured.
func (s Settings) ProviderEnvMap(providerName string) map[string]string {
	vars := s.ProviderEnvVars(providerName)
	if len(vars) == 0 {
		return nil
	}
	out := make(map[string]string, len(vars))
	for _, v := range vars {
		out[v.Name] = v.Value
	}
	return out
}

// setProviderEnvVars replaces the stored list for a provider. Validation runs
// at the Settings level (validateSettings), so this stays a plain assignment.
func (s *Settings) setProviderEnvVars(providerKey string, vars []ProviderEnvVar) {
	if len(vars) == 0 {
		vars = nil
	}
	switch providerKey {
	case "claude":
		s.ClaudeCustomEnv = vars
	case "codex":
		s.CodexCustomEnv = vars
	case "agy":
		s.AgyCustomEnv = vars
	}
}

// RedactProviderEnvVars returns a copy of the list with every sensitive
// variable's Value cleared. This is the projection GetSettings applies before
// the list crosses the transport boundary: GetSettings is reachable from a
// LAN-attached client, and a sensitive value is exactly the kind of material
// that must not cross that wire.
//
// The copy is deep enough to leave the Service's cached Settings untouched —
// the slice shares backing memory with the cache, so clearing in place would
// corrupt it for every later reader.
func RedactProviderEnvVars(vars []ProviderEnvVar) []ProviderEnvVar {
	if len(vars) == 0 {
		return nil
	}
	out := make([]ProviderEnvVar, len(vars))
	for i, v := range vars {
		out[i] = v
		if v.Sensitive {
			out[i].Value = ""
		}
	}
	return out
}

// ValidateProviderEnvVarName is the single name rule, exported so the App-level
// mutators can reject a bad name before touching the settings file and surface
// the same message the strict save path would.
//
// POSIX-ish shape: a leading letter or underscore, then letters, digits, and
// underscores. `env` accepts more than this and shells accept less; the
// intersection is what a user can reliably set and a provider CLI can
// reliably read, and it rules out the `=`-in-name shape that would silently
// re-split into a different variable at exec time.
func ValidateProviderEnvVarName(providerName, name string) error {
	_, key, err := providerEnvSettingsField(providerName)
	if err != nil {
		return err
	}
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return fmt.Errorf("environment variable name is empty")
	}
	if len(trimmed) > MaxProviderEnvNameLength {
		return fmt.Errorf(
			"environment variable name is %d characters, max is %d",
			len(trimmed), MaxProviderEnvNameLength,
		)
	}
	if strings.Contains(trimmed, "=") {
		return fmt.Errorf("environment variable name %q must not contain %q", trimmed, "=")
	}
	for i, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return fmt.Errorf(
				"environment variable name %q contains invalid character %q (use letters, digits, and underscore; no leading digit)",
				trimmed, r,
			)
		}
	}
	if refusal, refused := provider.ReservedEnvRefusal(key, trimmed); refused {
		return reservedProviderEnvNameError{name: trimmed, tableName: refusal.Name, reason: refusal.Reason}
	}
	return nil
}

// reservedProviderEnvNameError is ValidateProviderEnvVarName's refusal of a
// well-formed reserved name: tableName is the reserved table's spelling of it,
// "" for a name refused for its prefix (provider.EnvRefusal).
type reservedProviderEnvNameError struct{ name, tableName, reason string }

func (e reservedProviderEnvNameError) Error() string {
	return fmt.Sprintf("%q is reserved: %s", e.name, e.reason)
}

// validateProviderEnvVar checks one entry's name and value.
func validateProviderEnvVar(providerName string, v ProviderEnvVar) (ProviderEnvVar, error) {
	if err := ValidateProviderEnvVarName(providerName, v.Name); err != nil {
		return ProviderEnvVar{}, err
	}
	v.Name = strings.TrimSpace(v.Name)
	// The value is stored verbatim: leading/trailing whitespace can be
	// meaningful (a path list, a header value), and trimming it would silently
	// change what the provider sees.
	if len(v.Value) > MaxProviderEnvValueLength {
		return ProviderEnvVar{}, fmt.Errorf(
			"value for %q is %d characters, max is %d",
			v.Name, len(v.Value), MaxProviderEnvValueLength,
		)
	}
	if strings.ContainsRune(v.Value, 0) {
		return ProviderEnvVar{}, fmt.Errorf("value for %q contains a NUL byte", v.Name)
	}
	return v, nil
}

// validateProviderEnvVars strictly validates one provider's list on save.
// Duplicate names are an error rather than a silent last-wins collapse: the
// user typed both, and only one of them was ever going to reach the process.
// Empty / nil round-trip as nil so sparse JSON omits the key.
func validateProviderEnvVars(providerName string, vars []ProviderEnvVar) ([]ProviderEnvVar, error) {
	field, _, err := providerEnvSettingsField(providerName)
	if err != nil {
		return nil, err
	}
	if len(vars) == 0 {
		return nil, nil
	}
	if len(vars) > MaxProviderEnvVars {
		return nil, fmt.Errorf(
			"%s has %d entries, max is %d",
			field, len(vars), MaxProviderEnvVars,
		)
	}
	out := make([]ProviderEnvVar, 0, len(vars))
	seen := make(map[string]string, len(vars))
	for _, raw := range vars {
		v, err := validateProviderEnvVar(providerName, raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		// Case-insensitive, although the child receives `Proxy` and `PROXY`
		// as two variables: a list whose names differ only in case is a
		// mistake to show the user, not two settings to apply.
		upper := strings.ToUpper(v.Name)
		if first, dup := seen[upper]; dup {
			if first == v.Name {
				return nil, fmt.Errorf("%s: %q is listed twice", field, v.Name)
			}
			return nil, fmt.Errorf(
				"%s: %q and %q differ only in case and would collide in the process environment",
				field, first, v.Name,
			)
		}
		seen[upper] = v.Name
		out = append(out, v)
	}
	return out, nil
}

// sanitizeProviderEnvVars is the lenient load-time counterpart. A hand-edited
// or downgraded file with one bad entry keeps its remaining variables instead
// of stranding the whole list, and every drop is logged (the variable the user
// expected simply not being set is the confusing failure this avoids).
func sanitizeProviderEnvVars(providerName string, vars []ProviderEnvVar) []ProviderEnvVar {
	kept, _ := walkSavedProviderEnv(providerName, vars, true)
	return kept
}

// walkSavedProviderEnv is the load rule for one provider's saved list: the
// entries it keeps, and the reserved table's names (each once) of the
// entries it drops as reserved. It logs every drop when logDrops is set; the
// load logs, and SavedReservedProviderEnv, which reads the same list again,
// does not.
func walkSavedProviderEnv(providerName string, vars []ProviderEnvVar, logDrops bool) (kept []ProviderEnvVar, reserved []string) {
	field, _, err := providerEnvSettingsField(providerName)
	if err != nil || len(vars) == 0 {
		return nil, nil
	}
	logf := func(format string, args ...any) {
		if logDrops {
			log.Printf(format, args...)
		}
	}
	out := make([]ProviderEnvVar, 0, len(vars))
	seen := make(map[string]struct{}, len(vars))
	for _, raw := range vars {
		v, err := validateProviderEnvVar(providerName, raw)
		if err != nil {
			var denied reservedProviderEnvNameError
			if errors.As(err, &denied) && denied.tableName != "" && !slices.Contains(reserved, denied.tableName) {
				reserved = append(reserved, denied.tableName)
			}
			logf("settings: dropping invalid %s entry %q: %v", field, raw.Name, err)
			continue
		}
		upper := strings.ToUpper(v.Name)
		if _, dup := seen[upper]; dup {
			logf("settings: dropping duplicate %s entry %q", field, v.Name)
			continue
		}
		seen[upper] = struct{}{}
		out = append(out, v)
		if len(out) >= MaxProviderEnvVars {
			logf("settings: %s truncated to %d entries", field, MaxProviderEnvVars)
			break
		}
	}
	if len(out) == 0 {
		return nil, reserved
	}
	return out, reserved
}

// ReservedProviderEnv is a variable saved in a provider's custom environment
// whose name is in provider's reserved table, so load drops it: a value
// saved before the name was reserved.
type ReservedProviderEnv struct {
	// Provider is the settings key: claude, codex or agy.
	Provider string
	// Name is the reserved name as the table spells it.
	Name string
}

// SavedReservedProviderEnv reads settings.json as it is now and returns the
// reserved variables its custom environments still hold, in provider order,
// each once. It decides "reserved" by the load's own rule
// (walkSavedProviderEnv), so it names exactly the entries load drops as
// reserved. The load does not keep what it dropped, so this decodes the
// three custom-environment keys of the file again; it keeps nothing between
// calls. A missing or malformed file reports nothing: the loader already
// handles and logs it.
func (s *Service) SavedReservedProviderEnv() []ReservedProviderEnv {
	s.mu.RLock()
	data, err := os.ReadFile(s.path)
	s.mu.RUnlock()
	if err != nil {
		return nil
	}
	var saved struct {
		Claude []ProviderEnvVar `json:"claudeCustomEnv"`
		Codex  []ProviderEnvVar `json:"codexCustomEnv"`
		Agy    []ProviderEnvVar `json:"agyCustomEnv"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil
	}
	var found []ReservedProviderEnv
	for _, list := range []struct {
		provider string
		vars     []ProviderEnvVar
	}{{"claude", saved.Claude}, {"codex", saved.Codex}, {"agy", saved.Agy}} {
		_, reserved := walkSavedProviderEnv(list.provider, list.vars, false)
		for _, name := range reserved {
			found = append(found, ReservedProviderEnv{Provider: list.provider, Name: name})
		}
	}
	return found
}

// maxReservedProviderEnvNamed is how many reserved variables the notice names
// before it counts the rest, which bounds its length.
const maxReservedProviderEnvNamed = 6

// ReservedProviderEnvNotice is the sentence that tells the person which
// saved variables are reserved and no longer applied, and how to clear them:
// a save of any provider's custom environment rewrites settings.json from
// the loaded settings, which no longer hold them (Service.mutate,
// writeSparse). It names variables only by
// their table name, never a value, and at most
// maxReservedProviderEnvNamed of them. "" for none.
func ReservedProviderEnvNotice(found []ReservedProviderEnv) string {
	if len(found) == 0 {
		return ""
	}
	labels := map[string]string{"claude": "Claude", "codex": "Codex", "agy": "Antigravity"}
	named := make([]string, 0, min(len(found), maxReservedProviderEnvNamed)+1)
	for _, entry := range found[:min(len(found), maxReservedProviderEnvNamed)] {
		named = append(named, labels[entry.Provider]+" "+entry.Name)
	}
	if rest := len(found) - maxReservedProviderEnvNamed; rest > 0 {
		named = append(named, fmt.Sprintf("%d more", rest))
	}
	list := named[0]
	switch len(named) {
	case 1:
	case 2:
		list = named[0] + " and " + named[1]
	default:
		list = strings.Join(named[:len(named)-1], ", ") + ", and " + named[len(named)-1]
	}
	verb := "is"
	pronoun := "it"
	if len(found) > 1 {
		verb, pronoun = "are", "them"
	}
	return fmt.Sprintf("%s %s reserved, so the provider runs without %s. Adding or removing any provider environment variable in Settings removes the saved %s.", list, verb, pronoun, map[bool]string{true: "entries", false: "entry"}[len(found) > 1])
}

// SetProviderEnvVar upserts one variable, preserving list position when the
// name already exists (case-insensitively). Value is always written — there is
// no "keep the existing value" mode, because the redacted read shape means the
// caller may not know the existing value, and a sentinel for "unchanged" would
// make an empty value unrepresentable.
//
// Custom environment goes through this method and DeleteProviderEnvVar rather
// than through Update: Update merges top-level keys by wholesale assignment, so
// a `GetSettings -> mutate -> Update` round trip would write back the redacted
// (empty) sensitive values and destroy them. Update rejects the keys outright
// so that regression cannot be reintroduced by a future caller.
func (s *Service) SetProviderEnvVar(providerName, name, value string, sensitive bool) (Settings, error) {
	_, key, err := providerEnvSettingsField(providerName)
	if err != nil {
		return Settings{}, err
	}
	if err := ValidateProviderEnvVarName(providerName, name); err != nil {
		return Settings{}, fmt.Errorf("settings: %w", err)
	}
	name = strings.TrimSpace(name)

	return s.mutate("", DeviceDesktop, func(current Settings) (Settings, error) {
		next := append([]ProviderEnvVar(nil), current.ProviderEnvVars(key)...)
		replaced := false
		for i := range next {
			if strings.EqualFold(next[i].Name, name) {
				next[i] = ProviderEnvVar{Name: name, Value: value, Sensitive: sensitive}
				replaced = true
				break
			}
		}
		if !replaced {
			next = append(next, ProviderEnvVar{Name: name, Value: value, Sensitive: sensitive})
		}
		current.setProviderEnvVars(key, next)
		return validated(current)
	})
}

// DeleteProviderEnvVar removes one variable by name (case-insensitively).
// A missing name is an error so a UI acting on a stale list gets a signal
// instead of a silent no-op.
func (s *Service) DeleteProviderEnvVar(providerName, name string) (Settings, error) {
	_, key, err := providerEnvSettingsField(providerName)
	if err != nil {
		return Settings{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Settings{}, fmt.Errorf("settings: environment variable name required")
	}

	return s.mutate("", DeviceDesktop, func(current Settings) (Settings, error) {
		existing := current.ProviderEnvVars(key)
		next := make([]ProviderEnvVar, 0, len(existing))
		removed := false
		for _, v := range existing {
			if !removed && strings.EqualFold(v.Name, name) {
				removed = true
				continue
			}
			next = append(next, v)
		}
		if !removed {
			return Settings{}, fmt.Errorf("settings: %s has no environment variable %q", providerName, name)
		}
		current.setProviderEnvVars(key, next)
		return validated(current)
	})
}

// validated is the whole-struct validation both custom-environment mutators
// run on their way to disk, so neither can skip it. The remaining mutators
// validate only the fields they touch (see mutate).
func validated(current Settings) (Settings, error) {
	checked, err := validateSettings(current)
	if err != nil {
		return Settings{}, fmt.Errorf("settings: validate: %w", err)
	}
	return checked, nil
}
