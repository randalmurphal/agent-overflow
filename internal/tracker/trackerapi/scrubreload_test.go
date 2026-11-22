package trackerapi

import (
	"bytes"
	"strings"
	"testing"

	"agent-overflow/internal/secret"
)

// A scrubber loaded again holds only the last credential's forms: a
// credential too long for its inline buffer, a lost one and a loaded one
// each leave nothing for the next load to read.
func TestScrubberReload(t *testing.T) {
	t.Parallel()
	const smallToken = "token-small-0001"
	long := Key(secret.New([]byte(strings.Repeat("x", 1100))))
	small := Key(secret.New([]byte(smallToken)))
	t.Cleanup(long.Zero)
	t.Cleanup(small.Zero)
	body := []byte(`{"v":"` + smallToken + `"}`)
	scrubs := func(t *testing.T, s *scrubber) {
		t.Helper()
		out, err := s.body(body)
		if err != nil || bytes.Contains(out, []byte(smallToken)) {
			t.Fatalf("the credential last loaded survives: %q %v", out, err)
		}
	}

	t.Run("after a long credential", func(t *testing.T) {
		var s scrubber
		s.load(long)
		held := s.big
		s.load(small)
		scrubs(t, &s)
		if s.big != nil || bytes.ContainsRune(held, 'x') {
			t.Fatal("the long credential's copy is still held or not cleared")
		}
	})
	t.Run("after a lost credential", func(t *testing.T) {
		s := scrubber{lost: true}
		s.load(small)
		if s.lost {
			t.Fatal("a loaded scrubber still reports its credential lost")
		}
		scrubs(t, &s)
	})
	t.Run("a hop that carried none", func(t *testing.T) {
		var s scrubber
		s.load(small)
		(&Client{}).loadScrubber(&s, sentWith{})
		if s.n != 0 || s.lost || bytes.Contains(s.buf[:], []byte(smallToken)) {
			t.Fatal("a hop that carried no credential left the last one's forms")
		}
		if out, err := s.body(body); err != nil || &out[0] != &body[0] {
			t.Fatalf("a hop that carried no credential scrubs nothing: %q %v", out, err)
		}
	})
}
