package codex

import (
	"encoding/json"
	"testing"
)

// TestDecodeProbeResponseKnownStatuses covers the four known
// ThreadStatus.type values from the Codex schema. A regression here
// would flip the reconciler's strategy silently; pinning the mapping
// keeps the wire contract load-bearing.
func TestDecodeProbeResponseKnownStatuses(t *testing.T) {
	cases := []struct {
		name       string
		payload    string
		wantStatus ThreadStatusKind
		wantFlags  int
	}{
		{
			name:       "idle",
			payload:    `{"thread":{"status":{"type":"idle"}}}`,
			wantStatus: ThreadStatusIdle,
			wantFlags:  0,
		},
		{
			name:       "notLoaded",
			payload:    `{"thread":{"status":{"type":"notLoaded"}}}`,
			wantStatus: ThreadStatusNotLoaded,
			wantFlags:  0,
		},
		{
			name:       "systemError",
			payload:    `{"thread":{"status":{"type":"systemError"}}}`,
			wantStatus: ThreadStatusSystemError,
			wantFlags:  0,
		},
		{
			name:       "active_with_flags",
			payload:    `{"thread":{"status":{"type":"active","activeFlags":["runningBackground","waitingForUser"]}}}`,
			wantStatus: ThreadStatusActive,
			wantFlags:  2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := decodeProbeResponse(json.RawMessage(tc.payload))
			if err != nil {
				t.Fatalf("decodeProbeResponse: %v", err)
			}
			if result.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", result.Status, tc.wantStatus)
			}
			if len(result.ActiveFlags) != tc.wantFlags {
				t.Fatalf("activeFlags len = %d, want %d", len(result.ActiveFlags), tc.wantFlags)
			}
		})
	}
}

// TestDecodeProbeResponseUnknownStatusPreservesLiteral verifies that a
// wire value outside the known set is returned verbatim. The caller's
// reconciler falls back to "treat as systemError" rather than letting
// a typo silently match an enum variant. Returning the literal keeps
// diagnostics possible.
func TestDecodeProbeResponseUnknownStatusPreservesLiteral(t *testing.T) {
	payload := []byte(`{"thread":{"status":{"type":"completelyNewShape"}}}`)
	result, err := decodeProbeResponse(payload)
	if err != nil {
		t.Fatalf("decodeProbeResponse: %v", err)
	}
	if string(result.Status) != "completelyNewShape" {
		t.Fatalf("unknown status = %q, want literal passthrough", result.Status)
	}
	if result.Status == ThreadStatusIdle || result.Status == ThreadStatusActive {
		t.Fatalf("unknown status should not match a known kind, got %q", result.Status)
	}
}

// TestDecodeProbeResponseMissingStatusErrors guards against silent
// success when the response is missing the `thread.status.type` field
// — the reconciler would otherwise treat an empty status as "idle" by
// default, which is the opposite of safe.
func TestDecodeProbeResponseMissingStatusErrors(t *testing.T) {
	cases := [][]byte{
		[]byte(`{}`),
		[]byte(`{"thread":{}}`),
		[]byte(`{"thread":{"status":{}}}`),
	}
	for i, payload := range cases {
		_, err := decodeProbeResponse(payload)
		if err == nil {
			t.Fatalf("case %d: expected error for missing status.type; got nil", i)
		}
	}
}
