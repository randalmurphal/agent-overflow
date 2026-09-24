package git

import (
	"fmt"
	"slices"
)

// forgeCLINames are the executables the forge implementations run. Every
// name here must be one an isolated Core redirects to its fake; adding a
// forge CLI means adding it here, or an isolated boot refuses it.
var forgeCLINames = []string{"gh", "glab"}

// ForgeCLINameEnv tells the fake which forge CLI it is standing in for
// ("gh" or "glab"). argv[0] carries the same name, but an interpreter
// replaces argv[0] with the script path, so a fake must read this instead.
const ForgeCLINameEnv = "AO_FORGE_CLI"

// CoreOption configures a Core at construction.
type CoreOption func(*Core)

// WithIsolatedForgeCLIs makes the Core unable to run a forge CLI from PATH.
//
// An isolated boot (--harness, --soak) must never reach the developer's
// real gh or glab, their login or the network behind them. With this
// option every forge CLI invocation runs fake instead: the executable at
// that absolute path, with argv[0] and ForgeCLINameEnv set to the
// impersonated name ("gh" or "glab") and env appended to its environment
// only. An empty fake leaves forge CLIs unconfigured, and every invocation
// fails with a ForgeCLIUnavailableError before anything is executed. Git
// itself still resolves on PATH; any other binary is refused.
func WithIsolatedForgeCLIs(fake string, env []string) CoreOption {
	return func(c *Core) {
		c.forgeCLIs = forgeCLIPolicy{
			isolated: true,
			fake:     fake,
			env:      slices.Clone(env),
		}
	}
}

// forgeCLIPolicy is the Core's answer to "what runs for this binary name".
// The zero value is the ordinary desktop behavior: every name resolves on
// PATH.
type forgeCLIPolicy struct {
	isolated bool
	fake     string
	env      []string
}

// ForgeCLIUnavailableError reports a forge CLI invocation an isolated Core
// refused because no fake is configured.
type ForgeCLIUnavailableError struct {
	Binary string
}

func (e *ForgeCLIUnavailableError) Error() string {
	return fmt.Sprintf("%s is disabled in this isolated boot: no fake forge CLI is configured (build ao-mockforge beside the backend, or pass --mock-forge)", e.Binary)
}

// commandTarget is what one spec executes: the program path, the argv[0]
// the child sees, and environment appended after everything else.
type commandTarget struct {
	path  string
	argv0 string
	env   []string
}

// resolve maps a binary name onto what actually runs. This is the only
// place a binary name becomes an executable, so the isolation guard
// cannot be bypassed by a runner variant.
func (p forgeCLIPolicy) resolve(binary string) (commandTarget, error) {
	if !p.isolated {
		return commandTarget{path: binary, argv0: binary}, nil
	}
	switch {
	case binary == "git":
		return commandTarget{path: binary, argv0: binary}, nil
	case slices.Contains(forgeCLINames, binary):
		if p.fake == "" {
			return commandTarget{}, &ForgeCLIUnavailableError{Binary: binary}
		}
		env := append(slices.Clone(p.env), ForgeCLINameEnv+"="+binary)
		return commandTarget{path: p.fake, argv0: binary, env: env}, nil
	default:
		return commandTarget{}, fmt.Errorf("%s is not a command an isolated boot may run", binary)
	}
}
