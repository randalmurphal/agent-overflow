package tracker

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
)

// OrderKey is an issue's place on one board as text that sorts bytewise:
// the lower key comes first. The pane and the mirror compare keys and
// never interpret them; only the provider that made a key reads it.
// A tracker whose order is already such text (Jira's rank) uses it as it
// is; one whose order is a number encodes it with FloatOrderKey.
type OrderKey string

// floatKeyLen is the length of every key FloatOrderKey makes: sixteen
// hexadecimal digits.
const floatKeyLen = 16

// FloatOrderKey encodes a numeric order value so that bytewise order of
// the keys is numeric order of the values: equal values give equal keys
// (negative zero is zero) and a lower value a lower key. Every key is
// sixteen lowercase hexadecimal digits. NaN has no place in an order and
// is refused.
func FloatOrderKey(v float64) (OrderKey, error) {
	if math.IsNaN(v) {
		return "", fmt.Errorf("tracker: order value is NaN")
	}
	if v == 0 {
		v = 0 // negative zero sorts as zero
	}
	bits := math.Float64bits(v)
	if bits>>63 == 1 {
		bits = ^bits
	} else {
		bits |= 1 << 63
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], bits)
	return OrderKey(hex.EncodeToString(b[:])), nil
}

// Float decodes a key FloatOrderKey made. Any other text, a Jira rank or
// a key FloatOrderKey cannot make included, is an error.
func (k OrderKey) Float() (float64, error) {
	var b [8]byte
	if len(k) != floatKeyLen {
		return 0, fmt.Errorf("tracker: order key %q is not a numeric order key", string(k))
	}
	if _, err := hex.Decode(b[:], []byte(k)); err != nil {
		return 0, fmt.Errorf("tracker: order key %q is not a numeric order key", string(k))
	}
	bits := binary.BigEndian.Uint64(b[:])
	if bits>>63 == 1 {
		bits &^= 1 << 63
	} else {
		bits = ^bits
	}
	v := math.Float64frombits(bits)
	// Only the key FloatOrderKey makes for v is accepted: not uppercase
	// digits, not negative zero, not NaN.
	if again, err := FloatOrderKey(v); err != nil || again != k {
		return 0, fmt.Errorf("tracker: order key %q is not one FloatOrderKey makes", string(k))
	}
	return v, nil
}
