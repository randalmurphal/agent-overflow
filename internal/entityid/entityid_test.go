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

func TestValidRejectsAnythingButACanonicalRandomUUID(t *testing.T) {
	canonical := New()
	for _, tc := range []struct {
		name string
		ids  []string
	}{
		// The tripwire the package exists for: a short, sequential or
		// human-readable id must not pass, because it is exactly what
		// collides when one client holds two backends.
		{"short or readable", []string{
			"",
			"t-1",
			"thread-1",
			"1",
			"project",
			"0",
			"7f1c3a2e",
			"not-a-uuid-at-all-but-thirtysixchars!",
		}},
		// Spellings uuid.Parse accepts but no stored id ever carries: they
		// compare unequal to what the database holds, so an id in one of
		// these forms is unusable as a key even though it names a real UUID.
		{"non-canonical spelling", []string{
			"{" + canonical + "}",
			"urn:uuid:" + canonical,
			removeAll(canonical, '-'),
			upper(canonical),
		}},
		// A version-1 (time-ordered) UUID is still unique, but it is not
		// what New mints, and pinning the version is what makes a swap to a
		// scheme with structure a test failure rather than a silent change.
		{"another UUID version", []string{"b54adf00-7a1c-11ef-9cd2-0242ac120002"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, id := range tc.ids {
				if Valid(id) {
					t.Errorf("Valid(%q) = true, want false", id)
				}
			}
		})
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
