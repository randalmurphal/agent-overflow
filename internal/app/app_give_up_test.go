package app

import (
	"testing"
	"time"
)

// A give-up limit is a test's own value when it set one, else the test
// process's patience, else the product value: with no patience, as in
// production, every limit a test did not set keeps its product value.
func TestGiveUpBoundPrecedence(t *testing.T) {
	t.Parallel()
	const set, patience, product = time.Millisecond, time.Minute, 5 * time.Second
	for _, c := range []struct {
		name                   string
		set, patience, product time.Duration
		want                   time.Duration
	}{
		{"a test's value wins over patience", set, patience, product, set},
		{"patience wins over the product value", 0, patience, product, patience},
		{"no patience keeps the product value", 0, 0, product, product},
		{"a test's value wins with no patience", set, 0, product, set},
	} {
		if got := giveUpBoundWith(c.set, c.patience, c.product); got != c.want {
			t.Errorf("%s: giveUpBoundWith(%v, %v, %v) = %v, want %v", c.name, c.set, c.patience, c.product, got, c.want)
		}
	}
	if got := giveUpBound(0, product); got != giveUpBoundWith(0, giveUpPatience, product) {
		t.Errorf("giveUpBound(0, %v) = %v; want the test process's patience %v applied", product, got, giveUpPatience)
	}
	if giveUpPatience <= 0 {
		t.Error("TestMain set no patience: a limit a test does not own can fire on a busy machine")
	}
}
