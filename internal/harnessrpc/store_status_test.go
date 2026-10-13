package harnessrpc

import (
	"encoding/json"
	"testing"
)

// TestHarnessStoreStatusReportsTheOpenDatabase: the fixture store is this
// build's, so it reports the build's version applied, no deferred phase
// pending and no failure, and the result encodes the failure as an explicit
// null so a caller can tell "none" from "field missing".
func TestHarnessStoreStatusReportsTheOpenDatabase(t *testing.T) {
	h, _ := newHarnessTestHost(t)
	status, err := h.HarnessStoreStatus()
	if err != nil {
		t.Fatalf("HarnessStoreStatus: %v", err)
	}
	if status.SchemaVersion != status.BuildSchemaVersion || status.SchemaVersion == 0 {
		t.Fatalf("schema %d, build %d: want the build's version applied", status.SchemaVersion, status.BuildSchemaVersion)
	}
	if status.DeferredPending || status.DeferredWatermark < status.DeferredLatest {
		t.Fatalf("a fresh fixture reports deferred work pending: %+v", status)
	}
	if status.DeferredFailure != nil {
		t.Fatalf("a fresh fixture reports a deferred failure: %+v", *status.DeferredFailure)
	}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(decoded["deferredFailure"]) != "null" {
		t.Fatalf("deferredFailure encodes as %s, want null", decoded["deferredFailure"])
	}
}

// TestHarnessStoreStatusWithoutAStore: a harness whose host has no store
// answers with an error rather than zero versions a caller could mistake
// for an unmigrated database.
func TestHarnessStoreStatusWithoutAStore(t *testing.T) {
	var h *Harness
	if _, err := h.HarnessStoreStatus(); err == nil {
		t.Fatal("a nil harness answered a store status")
	}
}
