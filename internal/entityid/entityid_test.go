package entityid

import "testing"

func TestNewMintsCanonicalRandomUUIDs(t *testing.T) {
	seen := make(map[string]struct{}, 64)
	for range 64 {
		id := New()
		if !Valid(id) {
			t.Fatalf("New() = %q, which Valid rejects", id)
		}
		if len(id) != 36 {
			t.Fatalf("New() = %q, want the 36-character canonical form", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("New() repeated %q", id)
		}
		seen[id] = struct{}{}
	}
}

// The tripwire the package exists for: a short, sequential or
// human-readable id must not pass, because it is exactly what collides
// when one client holds two backends.
func TestValidRejectsIDsThatCouldCollideAcrossBackends(t *testing.T) {
	for _, id := range []string{
		"",
		"t-1",
		"thread-1",
		"1",
		"project",
		"0",
		"7f1c3a2e",
		"not-a-uuid-at-all-but-thirtysixchars!",
	} {
		if Valid(id) {
			t.Errorf("Valid(%q) = true, want false", id)
		}
	}
}

// Spellings uuid.Parse accepts but no stored id ever carries: they
// compare unequal to what the database holds, so an id in one of these
// forms is unusable as a key even though it names a real UUID.
func TestValidRejectsNonCanonicalSpellings(t *testing.T) {
	canonical := New()
	for _, id := range []string{
		"{" + canonical + "}",
		"urn:uuid:" + canonical,
		removeAll(canonical, '-'),
		upper(canonical),
	} {
		if Valid(id) {
			t.Errorf("Valid(%q) = true, want false", id)
		}
	}
}

// A version-1 (time-ordered) UUID is still unique, but it is not what New
// mints, and pinning the version is what makes a swap to a scheme with
// structure a test failure rather than a silent change.
func TestValidRejectsAnotherUUIDVersion(t *testing.T) {
	const v1 = "b54adf00-7a1c-11ef-9cd2-0242ac120002"
	if Valid(v1) {
		t.Errorf("Valid(%q) = true, want false for a non-random UUID", v1)
	}
}

func removeAll(s string, drop byte) string {
	out := make([]byte, 0, len(s))
	for i := range len(s) {
		if s[i] != drop {
			out = append(out, s[i])
		}
	}
	return string(out)
}

func upper(s string) string {
	out := []byte(s)
	for i, b := range out {
		if b >= 'a' && b <= 'z' {
			out[i] = b - ('a' - 'A')
		}
	}
	return string(out)
}
