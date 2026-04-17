package checkpoint

import (
	"strings"
	"testing"
)

func TestRefForThreadTurnUsesBase64URLEncoding(t *testing.T) {
	// UUID-style thread IDs contain dashes; those are legal in refs but we
	// still encode so arbitrary thread IDs (including ones with '/' or '@')
	// don't produce invalid ref path components.
	threadID := "abc-123/weird@id"
	ref := RefForThreadTurn(threadID, 0)

	if !strings.HasPrefix(ref, RefsPrefix+"/") {
		t.Errorf("ref should start with %q, got %q", RefsPrefix, ref)
	}
	if strings.Contains(ref, "/") == false {
		t.Fatalf("ref should contain slashes: %q", ref)
	}
	// Encoded portion must not contain '/' or '@' because it's base64url.
	encoded := EncodeThreadID(threadID)
	if strings.ContainsAny(encoded, "/+=@") {
		t.Errorf("encoded thread id should be base64url-safe, got %q", encoded)
	}
}

func TestRefForThreadTurnIncludesTurnSegment(t *testing.T) {
	ref := RefForThreadTurn("t1", 7)
	if !strings.HasSuffix(ref, "/turn/7") {
		t.Errorf("expected ref to end with /turn/7, got %q", ref)
	}
}

func TestRefForThreadTurnDifferentTurnsDifferentRefs(t *testing.T) {
	a := RefForThreadTurn("t1", 0)
	b := RefForThreadTurn("t1", 1)
	if a == b {
		t.Errorf("refs for different turns should differ: %q == %q", a, b)
	}
}

func TestThreadRefPrefixMatchesRefForThreadTurn(t *testing.T) {
	ref := RefForThreadTurn("thread-a", 3)
	if !strings.HasPrefix(ref, ThreadRefPrefix("thread-a")) {
		t.Errorf("ref %q should start with thread prefix %q", ref, ThreadRefPrefix("thread-a"))
	}
}

func TestIsThreadRefIdentifiesOwnThread(t *testing.T) {
	ref := RefForThreadTurn("thread-a", 0)
	if !IsThreadRef(ref, "thread-a") {
		t.Errorf("ref %q should be recognised for thread-a", ref)
	}
	if IsThreadRef(ref, "thread-b") {
		t.Errorf("ref %q should NOT belong to thread-b", ref)
	}
}

func TestEncodeThreadIDDeterministic(t *testing.T) {
	// Two calls should produce the same encoding.
	id := "some-uuid-12345"
	if EncodeThreadID(id) != EncodeThreadID(id) {
		t.Fatalf("encoding should be deterministic")
	}
}

func TestThreadRefPatternIsGlob(t *testing.T) {
	pattern := ThreadRefPattern("t1")
	if !strings.HasSuffix(pattern, "/**") {
		t.Errorf("pattern should end with '/**' for recursive match, got %q", pattern)
	}
}
