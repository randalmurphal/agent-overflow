package workflowhost

import (
	"os"
	"testing"

	"agent-overflow/internal/store/storetest"
)

// One migrated template DB per package, byte-copied per test by
// storetest.Clone, so a store-backed runner test does not replay the migration
// chain. See internal/store/storetest.
func TestMain(m *testing.M) { os.Exit(storetest.Run(m)) }
