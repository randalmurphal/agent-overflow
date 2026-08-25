package main

import (
	"os"
	"testing"

	"agent-overflow/internal/store/storetest"
)

// This package's fixtures clone one migrated template instead of replaying the
// 69-migration chain per store. Nearly every one of the ~1900 tests here builds
// an App around a store, so without it the suite pays for the whole chain that
// many times, which is what pushed the -race run past its gate timeout.
func TestMain(m *testing.M) { os.Exit(storetest.Run(m)) }
