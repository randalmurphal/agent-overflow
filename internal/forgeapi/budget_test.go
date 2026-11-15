package forgeapi

import (
	"testing"

	"agent-overflow/internal/transport"
)

// One forge request is the step every bound method without a budget of its
// own may take (transport.DefaultMethodBudget); a longer default would let
// such a method outlast its clients' wait.
func TestDefaultReadTimeoutFitsTheDefaultMethodBudget(t *testing.T) {
	if defaultReadTimeout > transport.DefaultMethodBudget {
		t.Fatalf("defaultReadTimeout %s is past transport.DefaultMethodBudget %s", defaultReadTimeout, transport.DefaultMethodBudget)
	}
}
