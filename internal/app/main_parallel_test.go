package app

import (
	"testing"

	"agent-overflow/internal/testutil/testparallel"
)

func TestTheDefaultParallelIsCapped(t *testing.T) {
	t.Parallel()
	testparallel.CheckApplied(t)
}
