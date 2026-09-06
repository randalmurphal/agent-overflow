package settings

//go:generate go run ./gendefaults

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// The frontend used to hand-mirror DefaultSettings in three places, kept
// in sync only by comments. This file is the one source: it reflects the
// Settings struct's json tags, reads each field's value out of
// DefaultSettings, and writes the TypeScript literal the frontend
// imports. Regenerate with `go generate ./internal/settings`;
// TestFrontendDefaultsSourceIsCheckedIn fails if you forget.
//
// Zero values are materialized EXPLICITLY (`omitempty` is ignored here):
// Go drops them on the wire, and the frontend's merge is what puts them
// back, so a key missing from the generated object would leave the store
// holding undefined for a field the backend considers set.

// FrontendDefaultsRelPath is where the generated module lives, relative
// to this package's directory.
const FrontendDefaultsRelPath = "../../frontend/src/lib/generated/settingsDefaults.ts"

// FrontendDefaultsRegenCommand is what a failing check tells you to run.
const FrontendDefaultsRegenCommand = "go generate ./internal/settings"

// frontendDefaultsDenied lists the json field names that are NOT emitted,
// each with the reason it stays out. The emitted set plus this set must
// cover every field on the struct — TestFrontendDefaultsDenyListIsTotal
// enforces that, so a new setting forces a conscious choice here.
//
// Two different reasons live in this list, and the difference matters:
// a field that has no TypeScript counterpart at all, and a field the TS
// Settings type declares OPTIONAL and the frontend deliberately leaves
// undefined. Materializing the second kind would change merge semantics
// for every consumer that tests presence, so it is not a free win.
var frontendDefaultsDenied = map[string]string{
	// No TypeScript counterpart: on-disk shape versioning, owned by Go.
	"$schemaVersion": "on-disk schema version; never a user preference",
	// No TypeScript counterpart: written from window move/resize events.
	"window": "desktop window placement, owned by Go, absent from the TS Settings type",
	// No TypeScript counterpart.
	"editor": "open-in-editor preference, absent from the TS Settings type",

	// Optional in the TS Settings type and deliberately left undefined by
	// the frontend: each treats absence as "the provider decides", so a
	// materialized zero value would be a different answer than no answer.
	"claudePromptOverrides":       "optional; absent means no override, not an empty override list",
	"codexPromptOverrides":        "optional; absent means no override, not an empty override list",
	"claudeDisabledTools":         "optional; absent means the provider's own tool set",
	"codexDisabledTools":          "optional; absent means the provider's own tool set",
	"claudeTodoRemindersDisabled": "optional; absent means the CLI's own nudge behavior",
	"claudeOutputStyle":           "optional Claude session axis; empty means 'say nothing'",
	"claudeCrossSession":          "optional Claude session axis; empty means 'say nothing'",
	"claudeSubagentLimits":        "optional Claude session axis; empty means 'say nothing'",
	"claudeToolMemoryLimit":       "optional Claude session axis; empty means 'say nothing'",
	"claudeThinking":              "optional Claude session axis; empty means 'Claude Code decides'",
}

// FrontendDefaultsSource renders the generated TypeScript module.
func FrontendDefaultsSource() string {
	var b strings.Builder
	b.WriteString("// Generated from internal/settings.DefaultSettings by internal/settings/gendefaults.\n")
	b.WriteString("// Do not edit; regenerate with `" + FrontendDefaultsRegenCommand + "`.\n")
	b.WriteString("//\n")
	b.WriteString("// Zero values are materialized explicitly: Go's `omitempty` drops them on\n")
	b.WriteString("// the wire, and mergeSettingsWithDefaults fills them back in from here.\n")
	b.WriteString("// Fields the TS Settings type leaves optional and the frontend deliberately\n")
	b.WriteString("// leaves undefined are on the generator's deny-list, not missing by accident.\n\n")
	b.WriteString("import type { Settings } from '../types/settings';\n\n")
	b.WriteString("export const SETTINGS_DEFAULTS = ")
	b.WriteString(emitStruct(reflect.ValueOf(DefaultSettings), "", frontendDefaultsDenied))
	b.WriteString(" satisfies Settings;\n")
	b.WriteString("\n// Frontend-owned preferences. Backend persistence remains a migration/notification mirror.\n")
	b.WriteString("export const FRONTEND_SETTINGS_KEYS = [\n")
	var keys []string
	for key, tier := range tierByKey {
		if tier == TierDevice || key == "confirmArchive" || key == "confirmDelete" || key == "autoPinNewThreads" || key == "projectSortMode" || key == "defaultThreadEnvMode" || key == "claudeHiddenModels" || key == "codexHiddenModels" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		b.WriteString("  " + strconv.Quote(key) + ",\n")
	}
	b.WriteString("] as const satisfies readonly (keyof Settings)[];\n")
	b.WriteString("\n// Keys mirrored to each connection's device-scoped backend bucket.\n")
	b.WriteString("export const FRONTEND_DEVICE_SETTINGS_KEYS = [\n")
	for _, key := range keys {
		if tierByKey[key] == TierDevice {
			b.WriteString("  " + strconv.Quote(key) + ",\n")
		}
	}
	b.WriteString("] as const satisfies readonly (keyof Settings)[];\n")

	emitFrontendConstraints(&b)
	return b.String()
}

// emitStruct renders a struct value as a TS object literal. deny is
// consulted at the top level only; nested shapes emit whole.
func emitStruct(v reflect.Value, indent string, deny map[string]string) string {
	t := v.Type()
	inner := indent + "  "
	var b strings.Builder
	b.WriteString("{\n")
	for i := 0; i < t.NumField(); i++ {
		name := jsonFieldName(t.Field(i))
		if name == "" {
			continue
		}
		if _, skip := deny[name]; skip {
			continue
		}
		b.WriteString(inner)
		b.WriteString(tsKey(name))
		b.WriteString(": ")
		b.WriteString(emitValue(v.Field(i), inner))
		b.WriteString(",\n")
	}
	b.WriteString(indent)
	b.WriteString("}")
	return b.String()
}

func emitValue(v reflect.Value, indent string) string {
	switch v.Kind() {
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(v.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'g', -1, 64)
	case reflect.String:
		return tsString(v.String())
	case reflect.Slice, reflect.Array:
		// A nil slice emits [], not null: the frontend's merge treats the
		// absent list as empty and every consumer indexes into it.
		if v.Len() == 0 {
			return "[]"
		}
		inner := indent + "  "
		var b strings.Builder
		b.WriteString("[\n")
		for i := 0; i < v.Len(); i++ {
			b.WriteString(inner)
			b.WriteString(emitValue(v.Index(i), inner))
			b.WriteString(",\n")
		}
		b.WriteString(indent)
		b.WriteString("]")
		return b.String()
	case reflect.Struct:
		return emitStruct(v, indent, nil)
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return "null"
		}
		return emitValue(v.Elem(), indent)
	default:
		panic(fmt.Sprintf("settings: gendefaults cannot emit %s", v.Kind()))
	}
}

// jsonFieldName is the same tag walk knownSettingsFieldNames does.
func jsonFieldName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" || tag == "-" {
		return ""
	}
	if idx := strings.Index(tag, ","); idx >= 0 {
		tag = tag[:idx]
	}
	return tag
}

func tsKey(name string) string {
	if isTSIdentifier(name) {
		return name
	}
	return tsString(name)
}

func isTSIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_', r == '$':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// tsString renders a Go string as a TS string literal. json.Marshal's
// escaping is a subset of what TS accepts, so its output is a valid
// literal AND keeps union-typed fields (timestampFormat, paneDensity)
// type-checking against their string literal types.
func tsString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic("settings: gendefaults cannot quote string: " + err.Error())
	}
	return string(b)
}

// Export the existing validator tables so offline preferences obey the same
// rules as their server mirror. New options change in one place.
func emitFrontendConstraints(b *strings.Builder) {
	options := map[string]map[string]struct{}{
		"timestampFormat": allowedTimestampFormats, "sansFont": allowedFonts,
		"monoFont": allowedFonts, "defaultThreadEnvMode": allowedThreadEnvModes,
		"paneDensity": allowedPaneDensities, "activityRunDefault": allowedActivityRunDefaults,
		"notifyQuietWhen": allowedNotifyQuietWhen, "projectSortMode": allowedProjectSortModes,
		"usagePeriod": allowedUsagePeriods,
	}
	b.WriteString("\nexport const FRONTEND_SETTING_OPTIONS: Partial<Record<keyof Settings, readonly string[]>> = {\n")
	keys := make([]string, 0, len(options))
	for key := range options {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		values := make([]string, 0, len(options[key]))
		for value := range options[key] {
			values = append(values, value)
		}
		sort.Strings(values)
		encoded, _ := json.Marshal(values)
		fmt.Fprintf(b, "  %s: %s,\n", strconv.Quote(key), encoded)
	}
	b.WriteString("};\n")
	b.WriteString("\nexport const FRONTEND_SETTING_RANGES: Partial<Record<keyof Settings, readonly [number, number]>> = {\n")
	fmt.Fprintf(b, "  fontSize: [%d, %d],\n", MinFontSize, MaxFontSize)
	fmt.Fprintf(b, "  activityRunWindowRows: [%d, %d],\n", MinActivityRunWindowRows, MaxActivityRunWindowRows)
	b.WriteString("};\n")
}
