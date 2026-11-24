package store

import (
	"strings"
	"testing"
)

// A session write that requires its device to be unrevoked looks that one
// device up by its key. Activation runs under the computer pairing book's
// settle lock, so a read of every device there would grow every approval
// and status read with the device table.
func TestASessionWriteLooksUpOnlyItsOwnDevice(t *testing.T) {
	s, _, _ := seedOwnerDevice(t)
	for name, statement := range map[string]string{
		"activation":     activateSessionSQL,
		"the bare check": `UPDATE sessions SET last_seen_at = ? WHERE id = ? AND ` + sessionDeviceLive,
	} {
		args := []any{int64(1), int64(2), "sess", int64(1)}
		if name == "the bare check" {
			args = []any{int64(1), "sess"}
		}
		plan := explainWakeCarrierPlan(t, s.db, statement, args...)
		if strings.Contains(plan, "SCAN d") || strings.Contains(plan, "SCAN devices") || !strings.Contains(plan, "SEARCH d USING") {
			t.Errorf("%s plan:\n%s\nwant the device searched by its key, never scanned", name, plan)
		}
	}
}
