package workflowapp

import (
	"os"
	"testing"

	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/testutil/providerpath"
)

func TestMain(m *testing.M) { os.Exit(providerpath.Guard(func() int { return storetest.Run(m) })) }
