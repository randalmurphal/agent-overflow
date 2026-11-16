package git

import (
	"testing"

	"agent-overflow/internal/transport"
)

// One git, gh or glab command is the step every bound method without a
// budget of its own may take (transport.DefaultMethodBudget); a longer
// default would let such a method outlast its clients' wait.
func TestDefaultTimeoutFitsTheDefaultMethodBudget(t *testing.T) {
	if defaultTimeout > transport.DefaultMethodBudget {
		t.Fatalf("defaultTimeout %s is past transport.DefaultMethodBudget %s", defaultTimeout, transport.DefaultMethodBudget)
	}
}
