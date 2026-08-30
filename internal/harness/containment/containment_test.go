package containment

import (
	"testing"
)

func TestPrepareRejectsZeroLimit(t *testing.T) {
	if _, err := Prepare(0); err == nil {
		t.Fatal("Prepare(0) succeeded")
	}
}
