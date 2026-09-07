package wsllauncher

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestPayloadFingerprintStreamsExactBinaryBytes(t *testing.T) {
	for _, payload := range []string{"", "\x7fELF\x00\xff", strings.Repeat("\x00\x7f\xffembedded SPA", 5000)} {
		want := fmt.Sprintf("%x", sha256.Sum256([]byte(payload)))
		if got := PayloadFingerprint(payload); got != want {
			t.Fatalf("fingerprint of %d bytes: %s != %s", len(payload), got, want)
		}
	}
}

func BenchmarkPayloadFingerprint(b *testing.B) {
	payload := strings.Repeat("\x00embedded backend and SPA", 1<<20)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		PayloadFingerprint(payload)
	}
}
