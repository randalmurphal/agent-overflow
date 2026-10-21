// Package secret holds credential bytes so that no formatter, encoder or
// logger can print them.
//
// A Bytes prints "<redacted>" through every fmt verb, JSON and text
// marshaling, and slog. Only Reveal returns the bytes.
//
// The methods alone are not enough: fmt cannot call methods on a value it
// reaches through an unexported struct field and walks that value by
// reflection instead, and its bad-verb path ("%!s(...)") prints even a
// nested pointer's target in full. So the bytes are not stored as a slice
// that reflection could render. A Bytes keeps an unsafe.Pointer to them,
// which every fmt path prints as an address, wherever the Bytes is held.
//
// Zero and Reveal are not safe for concurrent use; the owner serializes
// them. Copies of a Bytes share its bytes, so Zero through one is seen by
// all.
package secret

import (
	"fmt"
	"log/slog"
	"unsafe"
)

// Redacted is what every formatter prints in place of a secret.
const Redacted = "<redacted>"

// Bytes is one secret value.
type Bytes struct {
	p unsafe.Pointer // first byte of the secret; nil when empty or zeroed
	n int
}

// New copies b into a new secret. The caller still owns b and should clear
// it when it no longer needs it.
func New(b []byte) *Bytes {
	return Adopt(append([]byte(nil), b...))
}

// Adopt wraps b without copying. The caller hands b over and must not use
// or retain it afterwards.
func Adopt(b []byte) *Bytes {
	if len(b) == 0 {
		return &Bytes{}
	}
	return &Bytes{p: unsafe.Pointer(unsafe.SliceData(b)), n: len(b)}
}

// Reveal returns the secret's bytes. The slice aliases the secret: callers
// must not modify or retain it beyond the call that needs it. A nil, empty
// or zeroed secret reveals nil.
func (s *Bytes) Reveal() []byte {
	if s == nil || s.p == nil {
		return nil
	}
	return unsafe.Slice((*byte)(s.p), s.n)
}

// Len reports the secret's length without revealing it.
func (s *Bytes) Len() int {
	if s == nil {
		return 0
	}
	return s.n
}

// Zero overwrites the secret and drops it. Every later Reveal returns nil.
func (s *Bytes) Zero() {
	if s == nil {
		return
	}
	clear(s.Reveal())
	s.p, s.n = nil, 0
}

func (Bytes) String() string   { return Redacted }
func (Bytes) GoString() string { return Redacted }

// Format prints Redacted for every verb and flag, %q and %x included.
func (Bytes) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(Redacted)) }

func (Bytes) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }
func (Bytes) MarshalText() ([]byte, error) { return []byte(Redacted), nil }
func (Bytes) LogValue() slog.Value         { return slog.StringValue(Redacted) }
