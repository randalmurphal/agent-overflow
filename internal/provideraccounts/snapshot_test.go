package provideraccounts

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A snapshot prints as the redaction marker wherever it is formatted,
// including through an unexported field on fmt's bad-verb path, which
// prints a nested value's contents in full.
func TestCredentialSnapshotNeverFormatsItsBytes(t *testing.T) {
	const raw = `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-live"}}`
	snapshot := NewCredentialSnapshot([]byte(raw))
	type holder struct {
		snapshot CredentialSnapshot
		pointer  *CredentialSnapshot
		Exported CredentialSnapshot
	}
	h := holder{snapshot: snapshot, pointer: &snapshot, Exported: snapshot}
	encoded, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	outputs := []string{string(encoded), fmt.Sprint(snapshot, &snapshot, h, &h)}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		outputs = append(outputs, fmt.Sprintf(verb, any(snapshot)), fmt.Sprintf(verb, any(h)), fmt.Sprintf(verb, any(&h)))
	}
	for _, out := range outputs {
		if strings.Contains(out, "sk-ant-oat01-live") || strings.Contains(out, fmt.Sprintf("%x", raw)) {
			t.Fatalf("snapshot bytes leaked: %q", out)
		}
	}
	if string(snapshot.Bytes()) != raw {
		t.Fatal("Bytes must return the credential")
	}
}
