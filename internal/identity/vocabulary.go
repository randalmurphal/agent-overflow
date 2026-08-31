// Package identity is the session core: it mints session credentials,
// verifies a presentation against both halves of a session, answers the
// per-RPC liveness question, and revokes.
//
// Layering. This package sits above internal/store (it persists identity
// rows there) and below internal/transport, which must NOT import it —
// transport stays store-free, and the two meet through narrow interfaces
// each side declares for itself (LiveConns here, the session hook there).
// Keeping that direction is what lets internal/store's own value sets be
// cross-checked from here without an import cycle.
//
// What this package does NOT decide: what a scope permits. The names below
// are the audit and persistence vocabulary; enforcement is phase 3
// (docs/specs/remote-access.md §5), and nothing here consults a scope to
// admit or refuse a call.
package identity

import (
	"fmt"
	"slices"
)

// DeviceClass is what kind of client instance a device row describes. The
// set is closed and mirrored by the `devices.class` CHECK
// (internal/store migration v75); TestDeclaredValueSetsMatchTheSchemaChecks
// drives every value through a real store and fails in both directions.
type DeviceClass string

const (
	// DeviceDesktop is a native desktop app instance.
	DeviceDesktop DeviceClass = "desktop"
	// DeviceBrowser is one browser profile. It is the only class with a
	// script-execution surface, which is the distinction that matters when
	// a person narrows what a device may do — never the device's size.
	DeviceBrowser DeviceClass = "browser"
	// DevicePhone is a phone or tablet app instance.
	DevicePhone DeviceClass = "phone"
	// DeviceCLI is a command-line client on some host.
	DeviceCLI DeviceClass = "cli"
	// DeviceBackendPeer is another backend enrolled for team sharing.
	DeviceBackendPeer DeviceClass = "backend-peer"
)

// DeviceClasses is every declared class, in the order the schema CHECK
// spells them.
var DeviceClasses = []DeviceClass{
	DeviceDesktop, DeviceBrowser, DevicePhone, DeviceCLI, DeviceBackendPeer,
}

// Valid reports whether c is a declared class.
func (c DeviceClass) Valid() bool { return slices.Contains(DeviceClasses, c) }

// BindingClass is how strongly a credential is tied to a device
// (docs/specs/remote-access.md §2). It travels with the credential, not
// with the socket: a session ever issued key-bound is never accepted as a
// plain bearer on any listener, loopback included.
//
// This package records the class. Enforcing what each class may do on
// which listener is phase 3.
type BindingClass string

const (
	// BindingLoopbackOnly is minted for the embedded webview, the WSL
	// launcher relay, and the local CLI. Accepted on loopback listeners
	// only, so a copy of one carries no remote capability at all.
	BindingLoopbackOnly BindingClass = "loopback-only"
	// BindingDeviceBound is minted for a paired device that holds a key or
	// a passkey. Accepted on any listener.
	BindingDeviceBound BindingClass = "device-bound"
	// BindingPublic is minted for sessions used over a tunnel. Accepted on
	// any listener, with a per-request proof.
	BindingPublic BindingClass = "public"
)

// BindingClasses is every declared binding class, in the order the schema
// CHECK spells them.
var BindingClasses = []BindingClass{
	BindingLoopbackOnly, BindingDeviceBound, BindingPublic,
}

// Valid reports whether b is a declared binding class.
func (b BindingClass) Valid() bool { return slices.Contains(BindingClasses, b) }

// Scope names what a principal may ask for. The ten values are the audit
// vocabulary of docs/specs/remote-access.md §5, persisted on a session row
// as a JSON array.
//
// They are NAMES here and nothing more. No function in this package reads
// a scope to admit or refuse anything: the enforced boundary (observe vs
// execute, crossed with binding class) and the generated method table are
// phase 3. They are declared now because a session cannot be stored
// without a scope set, and an unvalidated set would let a typo persist as
// a grant nobody can ever satisfy.
//
// `host` is deliberately absent. It is a value the phase-3 method
// annotation carries, marking a call with no remote form; it is not
// something a session is granted, so putting it here would invite a
// session row that claims it.
type Scope string

const (
	// ScopeThreadsRead covers thread, timeline, payload, and usage reads.
	ScopeThreadsRead Scope = "threads:read"
	// ScopeFilesRead covers diffs, file content, context lines, and search.
	ScopeFilesRead Scope = "files:read"
	// ScopeThreadsOperate covers send, steer, queue, and start — in
	// read-only or approval-required modes only.
	ScopeThreadsOperate Scope = "threads:operate"
	// ScopeApprovalsRespond covers answering tool-use approvals, which
	// authorizes host command execution and therefore does not share a
	// scope with "send a message".
	ScopeApprovalsRespond Scope = "approvals:respond"
	// ScopeThreadsAutonomy covers moving a thread into auto /
	// auto-accept-edits / full-access, and running workflows.
	ScopeThreadsAutonomy Scope = "threads:autonomy"
	// ScopeTerminalOperate covers PTY create/attach/write/replay and
	// worktree-setup output.
	ScopeTerminalOperate Scope = "terminal:operate"
	// ScopeGitOperate covers git mutations, worktrees, and the PR surface.
	ScopeGitOperate Scope = "git:operate"
	// ScopeAttachmentsWrite covers uploads; reads ride payload auth.
	ScopeAttachmentsWrite Scope = "attachments:write"
	// ScopeSettingsWrite covers user- and device-tier settings, excluding
	// host-tier keys and the step-up set.
	ScopeSettingsWrite Scope = "settings:write"
	// ScopeAccessAdmin covers the device list, revocation, and audit read.
	ScopeAccessAdmin Scope = "access:admin"
)

// Scopes is every declared scope, in the order the spec's table lists
// them.
var Scopes = []Scope{
	ScopeThreadsRead, ScopeFilesRead, ScopeThreadsOperate, ScopeApprovalsRespond,
	ScopeThreadsAutonomy, ScopeTerminalOperate, ScopeGitOperate,
	ScopeAttachmentsWrite, ScopeSettingsWrite, ScopeAccessAdmin,
}

// Valid reports whether s is a declared scope.
func (s Scope) Valid() bool { return slices.Contains(Scopes, s) }

// ValidateScopes returns the set as the strings a session row stores,
// refusing any name that is not declared.
//
// Refusing rather than dropping. A silently discarded scope produces a
// session that works for months and then refuses one call, with no record
// of the typo that caused it; an error at mint time is read by whoever
// made it.
func ValidateScopes(scopes []Scope) ([]string, error) {
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		if !scope.Valid() {
			return nil, fmt.Errorf("identity: %q is not a declared scope", string(scope))
		}
		out = append(out, string(scope))
	}
	return out, nil
}

// AuditEvent names one kind of credential event in `auth_audit`. The set
// is closed in Go rather than by a CHECK: it grows every phase, and SQLite
// cannot widen a CHECK in place, so a new event kind would otherwise cost
// a table rebuild for a log column.
//
// Only kinds this package actually writes are declared. An event nothing
// produces is a name a reader would search the log for and never find.
type AuditEvent string

const (
	// AuditSessionMinted records a session credential being issued.
	AuditSessionMinted AuditEvent = "session-minted"
	// AuditSessionRevoked records one session being revoked.
	AuditSessionRevoked AuditEvent = "session-revoked"
	// AuditDeviceRevoked records a device and every session it held being
	// revoked together.
	AuditDeviceRevoked AuditEvent = "device-revoked"
	// AuditVerificationRefused records a presentation this backend refused,
	// carrying the typed Reason.
	AuditVerificationRefused AuditEvent = "verification-refused"
	// AuditRecoveryCodesMinted records a fresh set replacing the unspent
	// codes of one account.
	AuditRecoveryCodesMinted AuditEvent = "recovery-codes-minted"
	// AuditRecoveryCodeConsumed records one code being spent.
	AuditRecoveryCodeConsumed AuditEvent = "recovery-code-consumed"
	// AuditRecoveryCodeRefused records a code that matched no unspent row —
	// a replay of a spent code and a code that never existed both land
	// here, because the backend genuinely cannot tell them apart.
	AuditRecoveryCodeRefused AuditEvent = "recovery-code-refused"
)

// AuditEvents is every declared event kind.
var AuditEvents = []AuditEvent{
	AuditSessionMinted, AuditSessionRevoked, AuditDeviceRevoked,
	AuditVerificationRefused, AuditRecoveryCodesMinted,
	AuditRecoveryCodeConsumed, AuditRecoveryCodeRefused,
}
