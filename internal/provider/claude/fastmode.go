// Package claude — extraction of the CLI's fast-mode report, which rides
// on two different envelopes (`system/init` and `result`) and therefore
// has one owner here rather than a copy in each parser file.

package claude

import (
	"encoding/json"
	"strings"

	"agent-overflow/internal/provider"
)

// maxFastModeFieldRunes bounds each fast-mode string before it leaves the
// parser. Both fields are short undocumented enums ("cooldown",
// "sdk_opt_in_required"); the cap only exists so a malformed or hostile
// envelope cannot push an unbounded string onto the live-state channel
// the composer renders.
const maxFastModeFieldRunes = 64

// extractFastModeStatus reads `fast_mode_state` / `fast_mode_disabled_reason`
// off an envelope's top level.
//
// Both keys are OPTIONAL and version-dependent: `fast_mode_disabled_reason`
// did not exist on CLI 2.1.105 (see testdata/real_output.ndjson, which
// carries `fast_mode_state` on `result` and no reason key) and does on
// 2.1.219. Absence therefore means "the binary said nothing", never "off"
// — hence the (status, ok) shape, and hence nil rather than a zero value
// reaching the event when neither key is present. Callers must not
// synthesise a default.
func extractFastModeStatus(raw map[string]json.RawMessage) (*provider.FastModeStatus, bool) {
	status := provider.FastModeStatus{
		State:          boundedFastModeField(readRawString(raw["fast_mode_state"])),
		DisabledReason: boundedFastModeField(readRawString(raw["fast_mode_disabled_reason"])),
	}
	if status.IsZero() {
		return nil, false
	}
	return &status, true
}

func boundedFastModeField(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > maxFastModeFieldRunes {
		return string(r[:maxFastModeFieldRunes])
	}
	return s
}
