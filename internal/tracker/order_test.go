package tracker

import (
	"cmp"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// orderSamples are the values an order can hold where encodings go wrong:
// zero of both signs, the smallest subnormals, the boundary between
// subnormal and normal, ones, the largest finite values and the
// infinities, plus each one's nearest neighbors.
func orderSamples() []float64 {
	base := []float64{
		0, math.Copysign(0, -1), math.SmallestNonzeroFloat64, -math.SmallestNonzeroFloat64,
		0x1p-1022, -0x1p-1022, 1, -1, 0.5, -0.5, 1e-300, -1e-300, 1e300, -1e300,
		math.MaxFloat64, -math.MaxFloat64, math.Inf(1), math.Inf(-1),
		-10, -1080, -1986, 65535.99999999999, 1 << 53, -(1 << 53),
	}
	var out []float64
	for _, v := range base {
		out = append(out, v, math.Nextafter(v, math.Inf(1)), math.Nextafter(v, math.Inf(-1)))
	}
	return out
}

func mustKey(t *testing.T, v float64) OrderKey {
	t.Helper()
	k, err := FloatOrderKey(v)
	if err != nil {
		t.Fatalf("FloatOrderKey(%v): %v", v, err)
	}
	return k
}

// TestFloatOrderKeyOrderIsNumericOrder is the property the mirror relies
// on: for any two values, comparing their keys bytewise gives the same
// answer as comparing the values.
func TestFloatOrderKeyOrderIsNumericOrder(t *testing.T) {
	values := orderSamples()
	r := rand.New(rand.NewPCG(1, 2))
	for range 4000 {
		v := math.Float64frombits(r.Uint64())
		if math.IsNaN(v) {
			continue
		}
		values = append(values, v, math.Nextafter(v, math.Inf(1)), math.Nextafter(v, math.Inf(-1)))
	}
	for range 2000 {
		// Values of the size Linear hands out, where neighbors are close.
		v := (r.Float64() - 0.5) * 1e6
		values = append(values, v, math.Nextafter(v, 0))
	}
	keyed := func(v float64) OrderKey {
		k := mustKey(t, v)
		if len(k) != floatKeyLen {
			t.Fatalf("key of %v is %q, not %d characters", v, k, floatKeyLen)
		}
		return k
	}
	same := func(a, b float64) {
		t.Helper()
		want := cmp.Compare(a, b)
		if got := strings.Compare(string(keyed(a)), string(keyed(b))); got != want {
			t.Fatalf("keys of %v and %v compare %d, values compare %d", a, b, got, want)
		}
	}
	// Adjacent values in numeric order prove the whole order by
	// transitivity; random pairs check it directly.
	slices.Sort(values)
	for i := 1; i < len(values); i++ {
		same(values[i-1], values[i])
	}
	for range 20000 {
		same(values[r.IntN(len(values))], values[r.IntN(len(values))])
	}
}

func TestFloatOrderKeyRoundTrips(t *testing.T) {
	for _, v := range orderSamples() {
		got, err := mustKey(t, v).Float()
		if err != nil {
			t.Fatalf("Float of the key of %v: %v", v, err)
		}
		want := v
		if v == 0 {
			want = 0 // negative zero decodes as zero
		}
		if math.Float64bits(got) != math.Float64bits(want) {
			t.Fatalf("key of %v decodes to %v", v, got)
		}
	}
	if mustKey(t, math.Copysign(0, -1)) != mustKey(t, 0) {
		t.Fatal("negative zero and zero have different keys")
	}
}

func TestFloatOrderKeyRefusesNaN(t *testing.T) {
	if _, err := FloatOrderKey(math.NaN()); err == nil {
		t.Fatal("NaN made a key")
	}
}

func TestOrderKeyFloatRefusesOtherText(t *testing.T) {
	nanKey := OrderKey("fff8000000000000") // the encoding of a NaN's bits
	negZero := OrderKey("7fffffffffffffff")
	one := mustKey(t, 1)
	for _, k := range []OrderKey{"", "0|i0005b:", "zzzzzzzzzzzzzzzz", OrderKey(strings.ToUpper(string(one))), one + "0", nanKey, negZero} {
		if v, err := k.Float(); err == nil {
			t.Errorf("Float(%q) = %v, want an error", k, v)
		}
	}
}
