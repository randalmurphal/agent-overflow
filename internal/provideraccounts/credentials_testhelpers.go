package provideraccounts

import (
	"errors"
	"testing"
)

// WriteNativeCredentialForTest writes the canonical native credential
// through the same path production writes use. Tests use it to stand in
// for an external login or rotation without knowing the platform's
// storage layout. (Under `go test` the darwin backend is the file-backed
// stand-in, which shares the non-darwin layout — so writing the file at
// ActiveCredentialPath would also work — but this hook is the one
// blessed way, so fixtures don't encode the layout.)
//
// It refuses to run outside a test binary: it exists to seed
// credentials for tests, and keeping it inert elsewhere means no
// production path can grow to depend on it.
//
// It deliberately bypasses the sign-out refusal in writeActiveCredential.
// That refusal binds Agent Overflow's own commits; this helper impersonates
// the provider CLI, which is precisely the actor that writes a husk over the
// canonical credential — the state several fixtures exist to reproduce.
func (c *Credentials) WriteNativeCredentialForTest(providerName string, data []byte) error {
	if !testing.Testing() {
		return errors.New("provideraccounts: WriteNativeCredentialForTest is only available inside a test binary")
	}
	return c.storeActiveCredential(providerName, data)
}
