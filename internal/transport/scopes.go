package transport

// Scope names the capability one bound method exercises. It is the
// vocabulary the `//ao:scope` source annotation is written in, the
// vocabulary `methodgen` validates against, and the vocabulary the
// generated table carries (docs/specs/remote-access.md §5).
//
// The set is the ten scope names a session can be granted, plus `host`
// — which is a method property, never a grant: it marks a call that
// acts on the host desktop or reconfigures the host itself and has no
// remote form at all.
//
// Restated, not imported. `internal/identity` declares the same ten
// names as the audit and persistence vocabulary, and this package must
// not import it: identity persists into `internal/store`, and transport
// stays store-free (see identity's package comment for the direction).
// The spellings are pinned to identity's constants from `internal/app`,
// which imports both — `TestScopeVocabularyMatchesIdentity`, which fails
// in both directions, so a name added on either side without the other
// is a build failure rather than an annotation nobody can satisfy.
type Scope string

const (
	// ScopeThreadsRead covers thread, timeline, and payload reads, and
	// usage accounting. Observe tier.
	ScopeThreadsRead Scope = "threads:read"
	// ScopeFilesRead covers diffs, file content, context lines, and
	// search. Observe tier.
	ScopeFilesRead Scope = "files:read"
	// ScopeThreadsOperate covers send, steer, queue, and start, plus the
	// thread and project bookkeeping around them.
	ScopeThreadsOperate Scope = "threads:operate"
	// ScopeApprovalsRespond covers answering tool-use approvals, which
	// authorizes host command execution and therefore does not share a
	// scope with "send a message".
	ScopeApprovalsRespond Scope = "approvals:respond"
	// ScopeThreadsAutonomy covers moving a thread into auto /
	// auto-accept-edits / full-access, and running workflows and
	// automations.
	ScopeThreadsAutonomy Scope = "threads:autonomy"
	// ScopeTerminalOperate covers PTY create/attach/write/replay and
	// worktree-setup output.
	ScopeTerminalOperate Scope = "terminal:operate"
	// ScopeGitOperate covers git mutations, worktrees, and the PR
	// surface.
	ScopeGitOperate Scope = "git:operate"
	// ScopeAttachmentsWrite covers uploads; reads ride payload auth.
	ScopeAttachmentsWrite Scope = "attachments:write"
	// ScopeSettingsWrite covers user- and device-tier settings,
	// excluding host-tier keys and the step-up set.
	ScopeSettingsWrite Scope = "settings:write"
	// ScopeAccessAdmin covers the device list, revocation, audit read,
	// and the provider-account surface (billing identity).
	ScopeAccessAdmin Scope = "access:admin"
	// ScopeHost marks a call with no remote form: it acts on the host
	// desktop or reconfigures the host itself. Not a grant — no session
	// holds it, and `internal/identity` deliberately does not declare it,
	// so a session row cannot claim it.
	ScopeHost Scope = "host"
)

// Scopes is every declared scope, in the order §5's table lists them,
// with `host` last because it is the one value that is not a grant.
var Scopes = []Scope{
	ScopeThreadsRead, ScopeFilesRead, ScopeThreadsOperate, ScopeApprovalsRespond,
	ScopeThreadsAutonomy, ScopeTerminalOperate, ScopeGitOperate,
	ScopeAttachmentsWrite, ScopeSettingsWrite, ScopeAccessAdmin,
	ScopeHost,
}

// ScopeTier is the enforced boundary the scope names resolve to. Scope
// names are the audit vocabulary — what a refusal says, what a grant
// records — while the tier is what a gate compares.
//
// Ordinals start at 1 so the zero value is "no tier", which is what an
// undeclared scope resolves to: a scope nobody placed in the table
// enforces as nothing, so make it enforce as unreachable instead.
type ScopeTier uint8

const (
	// TierObserve is read-only access to history and content. The only
	// tier a viewer link or a peer backend can reach.
	TierObserve ScopeTier = iota + 1
	// TierExecute is anything that changes state, drives a provider
	// session, or reaches the host.
	TierExecute
	// TierHost is the residue with no remote form at all. Its own tier
	// rather than a flag on execute: an execute-tier call is one a
	// remote owner device is expected to make, and a host-tier call is
	// one nobody can make from anywhere but this desktop.
	TierHost
)

// scopeTiers is §5's Tier column, declared once. Two observe entries,
// one host entry, and execute for everything else.
var scopeTiers = map[Scope]ScopeTier{
	ScopeThreadsRead:      TierObserve,
	ScopeFilesRead:        TierObserve,
	ScopeThreadsOperate:   TierExecute,
	ScopeApprovalsRespond: TierExecute,
	ScopeThreadsAutonomy:  TierExecute,
	ScopeTerminalOperate:  TierExecute,
	ScopeGitOperate:       TierExecute,
	ScopeAttachmentsWrite: TierExecute,
	ScopeSettingsWrite:    TierExecute,
	ScopeAccessAdmin:      TierExecute,
	ScopeHost:             TierHost,
}

// Tier reports the enforced tier of a scope. An undeclared scope
// answers the zero tier, which no gate treats as observe.
func (s Scope) Tier() ScopeTier { return scopeTiers[s] }

// Valid reports whether s is a declared scope. `methodgen` refuses an
// annotation naming anything else, so a valid generated table cannot
// carry an invalid scope; this answers the question for hand-built
// values.
func (s Scope) Valid() bool { return scopeTiers[s] != 0 }
