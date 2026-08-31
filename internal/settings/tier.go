package settings

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Tier says WHOSE preference a settings key is: the machine running the
// backend, the person using it, or the screen it is being read on. It is the
// taxonomy docs/specs/remote-access.md §6 describes ("One mechanism, three
// tiers"), stated here because `settings:updated` carries it (§8) and a
// consumer that must decide "does this change concern me?" cannot answer from
// the key name alone.
//
// STORAGE IS NOT THE TIER. Every key below lives in settings.json today, which
// §6 calls the host-tier store. The tier a key carries here is the tier it
// BELONGS to; phase 4 moves the user- and device-tier keys into `ui_state`
// scopes without changing what any of them answer here. Keeping the answer in
// one map now is what makes that move a storage change rather than a wire
// change.
type Tier string

const (
	// TierHost configures the backend machine: the listeners it binds, the
	// provider binaries and environment it spawns with, what it retains, what
	// it reports, and the authorities it grants a provider session. §6: "it
	// configures the backend before identity or the DB matter, and must be
	// hand-editable when the UI is unreachable."
	TierHost Tier = "host"
	// TierUser is the person's working preference — it should follow them to
	// whatever machine they are driving: confirmations, commit-message style,
	// text-generation routing, hidden models, agent behaviour axes.
	TierUser Tier = "user"
	// TierDevice is a property of the screen and the installation in front of
	// the user: fonts, density, motion, sort order, the recent list, window
	// geometry, and the endpoints this installation attaches to.
	TierDevice Tier = "device"
)

// schemaVersionKey is file bookkeeping, not a setting. writeSparse stamps
// CurrentSchemaVersion on every save, so a file written before versioning
// existed would report `$schemaVersion` as a changed key on the next
// unrelated save — a change no consumer can act on. It is excluded from the
// change set and deliberately carries no tier.
const schemaVersionKey = "$schemaVersion"

// tierByKey answers Tier for every JSON key on Settings.
//
// The keys §6 names explicitly are placed where it puts them. The rest were
// classified by the same question §6 asks: does this configure the BACKEND
// MACHINE (host), the PERSON (user), or the SCREEN (device)?
//
// TestEverySettingsKeyHasATier fails on any Settings field missing from this
// map, so a new setting cannot ship without a tier decision.
var tierByKey = map[string]Tier{
	// ---- host: what this backend machine is and what it may do ----------
	"network":                      TierHost,
	"claudeBinaryPath":             TierHost,
	"codexBinaryPath":              TierHost,
	"claudeCustomEnv":              TierHost,
	"codexCustomEnv":               TierHost,
	"claudeEnabled":                TierHost,
	"codexEnabled":                 TierHost,
	"claudeTuiEnabled":             TierHost,
	"retention":                    TierHost,
	"observabilityEventLogEnabled": TierHost,
	"observabilityOtlpEndpoint":    TierHost,
	"observabilityTracingEnabled":  TierHost,
	// The browser keys grant a provider session an authority over this
	// machine (a managed browser process, its persisted site data, and
	// filesystem reach outside the workspace). Host, like the binaries.
	"browserEnabled":               TierHost,
	"browserPersistSiteData":       TierHost,
	"browserAllowOutsideWorkspace": TierHost,
	// Keep-awake inhibits THIS machine's sleep, and workflowPaused is the
	// backend engine's own run state.
	"keepAwakeEnabled": TierHost,
	"keepAwakeScreen":  TierHost,
	"workflowPaused":   TierHost,

	// ---- user: the person's working preferences -------------------------
	"confirmArchive":                   TierUser,
	"confirmDelete":                    TierUser,
	"autoPinNewThreads":                TierUser,
	"commitMessageStyle":               TierUser,
	"commitMessageStyleCustom":         TierUser,
	"textGenerationProvider":           TierUser,
	"textGenerationModel":              TierUser,
	"textGenerationReasoningEffort":    TierUser,
	"claudeHiddenModels":               TierUser,
	"codexHiddenModels":                TierUser,
	"defaultThreadEnvMode":             TierUser,
	"worktreeBranchPrefix":             TierUser,
	"claudeAutoCompactStandardPercent": TierUser,
	"claudeAutoCompactExtendedPercent": TierUser,
	"codexAutoCompactStandardPercent":  TierUser,
	"codexAutoCompactExtendedPercent":  TierUser,
	"gitlabSelfHostedHosts":            TierUser,
	// How the person wants an agent to behave — prompt and tool overrides,
	// thinking budget, cross-session recall, subagent and memory caps. These
	// travel with the person, not with the machine that happens to spawn the
	// process.
	"claudeThinking":              TierUser,
	"claudeCrossSession":          TierUser,
	"claudeOutputStyle":           TierUser,
	"claudeTodoRemindersDisabled": TierUser,
	"claudeToolMemoryLimit":       TierUser,
	"claudeSubagentLimits":        TierUser,
	"claudeDisabledTools":         TierUser,
	"codexDisabledTools":          TierUser,
	"claudePromptOverrides":       TierUser,
	"codexPromptOverrides":        TierUser,

	// ---- device: this screen and this installation ----------------------
	"lowPowerMode":          TierDevice,
	"sansFont":              TierDevice,
	"monoFont":              TierDevice,
	"fontSize":              TierDevice,
	"paneDensity":           TierDevice,
	"activityRunWindowRows": TierDevice,
	"activityRunDefault":    TierDevice,
	"streamingEnabled":      TierDevice,
	"diffWordWrap":          TierDevice,
	"collapseDiffPreviews":  TierDevice,
	"timestampFormat":       TierDevice,
	"editor":                TierDevice,
	"backgroundGitFetch":    TierDevice,
	"projectSortMode":       TierDevice,
	"usagePeriod":           TierDevice,
	"recentWorkspaces":      TierDevice,
	"remoteEndpoints":       TierDevice,
	"window":                TierDevice,
	// Spinner appearance is display, like fonts and motion.
	"spinnerVerbsEnabled":         TierDevice,
	"spinnerAnimationsEnabled":    TierDevice,
	"spinnerBuiltinVerbsDisabled": TierDevice,
	"spinnerCustomVerbs":          TierDevice,
	"spinnerDisabledAnimations":   TierDevice,
	"spinnerCompactionAnimation":  TierDevice,
}

// TierForKey reports the tier a settings key belongs to. An unknown key
// answers TierHost and false: the caller learns the key was never classified
// rather than silently receiving a plausible-looking answer, and the
// fail-closed default is the most restrictive of the three (§6 gives host-tier
// writes the step-up requirement).
func TierForKey(key string) (Tier, bool) {
	tier, ok := tierByKey[key]
	if !ok {
		return TierHost, false
	}
	return tier, true
}

// keyValues projects a Settings value onto its per-key JSON encoding, which is
// what change detection compares. Marshalling produces independent strings, so
// a probe taken before a mutation survives an in-place edit of the value it
// was taken from (UpdateRemoteEndpoint assigns into the slice it was handed).
func keyValues(current Settings) (map[string]string, error) {
	encoded, err := json.Marshal(current)
	if err != nil {
		return nil, fmt.Errorf("settings: encode for change detection: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &raw); err != nil {
		return nil, fmt.Errorf("settings: decode for change detection: %w", err)
	}
	out := make(map[string]string, len(raw))
	for key, value := range raw {
		out[key] = string(value)
	}
	return out, nil
}

// changedKeys reports the keys whose value differs between two projections,
// sorted. A key present on one side only counts as changed — that is a field
// crossing its omitempty boundary, which is a real change.
func changedKeys(before, after map[string]string) []string {
	var keys []string
	for key, afterValue := range after {
		if key == schemaVersionKey {
			continue
		}
		if beforeValue, ok := before[key]; !ok || beforeValue != afterValue {
			keys = append(keys, key)
		}
	}
	for key := range before {
		if key == schemaVersionKey {
			continue
		}
		if _, ok := after[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}
