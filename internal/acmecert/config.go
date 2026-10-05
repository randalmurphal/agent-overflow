package acmecert

import "time"

// Config is what an issuance needs. Every field is required.
type Config struct {
	// Dir is the app's config root: where the account key and the issued
	// certificate live.
	Dir string

	// Domain is the canonical domain the certificate is for. Exactly one:
	// a certificate covering names the user did not ask for is a
	// certificate the CA logs under names they did not ask for.
	Domain string

	// Hook is the argv of the command that publishes and removes the
	// challenge TXT record. See the package doc for its contract.
	Hook []string

	// HookTimeout bounds ONE hook invocation. Zero means
	// DefaultHookTimeout.
	HookTimeout time.Duration
}
