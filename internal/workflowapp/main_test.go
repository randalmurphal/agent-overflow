package workflowapp

import (
	"os"
	"testing"

	"agent-overflow/internal/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Run(m)) }
