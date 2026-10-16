package wake

import (
	"os"
	"testing"

	"agent-overflow/internal/testutil/providerpath"
)

func TestMain(m *testing.M) { os.Exit(providerpath.Run(m)) }
