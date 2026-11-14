// Package accountswitch decides when the provider-wide selected account
// should change and which threads to continue after a usage limit. It owns
// refusal marks and waiting threads, both kept in memory.
//
// Every effect (reading usage, selecting an account, queueing a message,
// reporting) goes through the capability functions in Deps. The package
// imports no app, store, session or account package. The product contract is
// docs/specs/account-auto-switch.md.
package accountswitch
