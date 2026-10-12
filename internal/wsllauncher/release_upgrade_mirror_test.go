package wsllauncher

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/supervise"
)

// TestReleaseUpgradeCheckMirrorsTheLaunchersLimits pins the constants the
// release upgrade check (e2e/tests/upgrade-corpus-config.ts) holds a
// migration to against the production values it mirrors. The check judges
// a trial's progress reports and a boot's starting reports the way this
// package and the supervisor do, so a product change to a window or a
// ceiling fails here until the mirror follows.
func TestReleaseUpgradeCheckMirrorsTheLaunchersLimits(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "e2e", "tests", "upgrade-corpus-config.ts"))
	if err != nil {
		t.Fatalf("read the check's config: %v", err)
	}
	want := map[string]time.Duration{
		"TRIAL_STALL_WINDOW_MS":          startupprogress.StallWindow,
		"TRIAL_CEILING_MS":               supervise.TrialCeiling,
		"UPDATE_COMMAND_STALL_WINDOW_MS": UpdateCommandStallWindow,
		"UPDATE_COMMAND_CEILING_MS":      UpdateCommandCeiling,
		"BOOT_PROBE_DEADLINE_MS":         bootstrapProbeDeadline,
	}
	for name, value := range want {
		got, ok := tsConstantMillis(string(source), name)
		if !ok {
			t.Errorf("%s is not declared as a number in upgrade-corpus-config.ts", name)
			continue
		}
		if got != value.Milliseconds() {
			t.Errorf("%s = %d in upgrade-corpus-config.ts, the launcher uses %d", name, got, value.Milliseconds())
		}
	}
}

// tsConstantMillis evaluates `export const NAME = <expr>;` where expr is a
// product of integer literals and previously declared constants, which is
// how the config spells a derived limit.
func tsConstantMillis(source, name string) (int64, bool) {
	declaration := regexp.MustCompile(`(?m)^export const ` + name + ` = ([^;]+);`)
	match := declaration.FindStringSubmatch(source)
	if match == nil {
		return 0, false
	}
	total := int64(0)
	for _, term := range strings.Split(match[1], "+") {
		product := int64(1)
		for _, factor := range strings.Split(term, "*") {
			factor = strings.TrimSpace(factor)
			if n, err := strconv.ParseInt(strings.ReplaceAll(factor, "_", ""), 10, 64); err == nil {
				product *= n
				continue
			}
			value, ok := tsConstantMillis(source, factor)
			if !ok {
				return 0, false
			}
			product *= value
		}
		total += product
	}
	return total, true
}
