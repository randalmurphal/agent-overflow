package highlight

import (
	"testing"

	"agent-overflow/internal/highlight/grammars"
)

func TestSchemaVersionStableAndNonEmpty(t *testing.T) {
	v := SchemaVersion()
	if v == "" {
		t.Fatal("SchemaVersion() must not be empty")
	}
	if v != SchemaVersion() {
		t.Fatal("SchemaVersion() must be stable within a process")
	}
}

func TestSchemaDigestDeterministic(t *testing.T) {
	if grammars.SchemaDigest() != grammars.SchemaDigest() {
		t.Fatal("SchemaDigest() must be deterministic")
	}
	if grammars.SchemaDigest() == 0 {
		t.Fatal("SchemaDigest() over embedded queries must not be zero")
	}
}
