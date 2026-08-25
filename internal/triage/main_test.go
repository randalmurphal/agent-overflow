package triage

import (
	"os"
	"testing"

	"agent-overflow/internal/store/storetest"
)

// This package's fixtures clone one migrated template instead of replaying the
// 69-migration chain per test. Without it the ~670 store-backed tests here pay
// for the whole chain each time, which is what pushed the -race run past its
// gate timeout.
func TestMain(m *testing.M) { os.Exit(storetest.Run(m)) }
