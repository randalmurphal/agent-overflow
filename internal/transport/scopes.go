package transport

// Scope names the capability one bound method exercises. It is the
// vocabulary the `//ao:scope` source annotation is written in, the
// vocabulary `methodgen` validates against, and the vocabulary the
// generated table carries (docs/specs/remote-access.md §5).
//
// The set is the scope names a session can be granted, plus two values
// that are method properties rather than grants: `host`, which marks a
// call that acts on the host desktop or reconfigures the host itself and
// has no remote form at all, and `session`, the FLOOR — a call whose only
// requirement is that the caller named a live session.
//
// Restated, not imported. `internal/identity` declares the same grantable
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
	// ScopePreviewOpen covers opening this machine's dev-server previews
	// from another machine: listing the dev servers discovery found, and
	// minting the ticketed preview URL for one (the port gateway,
	// docs/specs/remote-access.md §7), or opening a confined HTML directory
	// with the same preview authorization at a separate origin.
	//
	// Its own name rather than a reuse of ScopeTerminalOperate, and the
	// vocabulary is exactly where that distinction has to be expressible:
	// a named reviewer may legitimately be allowed to LOOK at a running
	// preview and not to run terminals on the machine serving it, and a
	// scope set that cannot say so can only answer by granting both.
	// Execute tier, because the list is a port-scan oracle for the host
	// and the gateway reaches a local service — a view-only device holds
	// neither.
	ScopePreviewOpen Scope = "preview:open"
	// ScopeGitOperate covers git mutations, worktrees, and the PR
	// surface.
	ScopeGitOperate Scope = "git:operate"
	// ScopeAttachmentsWrite covers uploads; reads ride payload auth.
	ScopeAttachmentsWrite Scope = "attachments:write"
	// ScopeSettingsRead covers reads of the settings and preference
	// surface: settings, keybindings, themes, spinner assets, composer
	// favourites, and per-device view state. Observe tier, and the reason
	// the tier exists at all for this surface: before it, a getter had
	// only `settings:write` to name itself with, so reading a preference
	// enforced as the authority to change one.
	ScopeSettingsRead Scope = "settings:read"
	// ScopeSettingsWrite covers user- and device-tier settings,
	// excluding host-tier keys and the step-up set.
	ScopeSettingsWrite Scope = "settings:write"
	// ScopeAccessAdmin covers the device list, revocation, audit read,
	// and the provider-account surface (billing identity).
	ScopeAccessAdmin Scope = "access:admin"
	// ScopeSession is the FLOOR: any connection that named a live session
	// may call it (docs/specs/remote-access.md §6, "The session floor").
	// It marks the calls whose real authority is decided per ARGUMENT —
	// the settings patch, gated key by key against §6's three tiers — or
	// is simply "this session is writing its own bucket", which is what
	// the ui_state methods do. A view-only device setting its own font
	// size is the case it exists for.
	//
	// Not a grant, for the reason `host` is not: no session holds it,
	// `internal/identity` does not declare it, and naming it in a grant
	// set would describe an authority the gate never reads.
	ScopeSession Scope = "session"
	// ScopeHost marks a call with no remote form: it acts on the host
	// desktop or reconfigures the host itself. Not a grant — no session
	// holds it, and `internal/identity` deliberately does not declare it,
	// so a session row cannot claim it.
	ScopeHost Scope = "host"
)

// Scopes is every declared scope, in the order §5's table lists them,
// with the two values that are not grants last: the floor, then `host`.
var Scopes = []Scope{
	ScopeThreadsRead, ScopeFilesRead, ScopeThreadsOperate, ScopeApprovalsRespond,
	ScopeThreadsAutonomy, ScopeTerminalOperate, ScopePreviewOpen, ScopeGitOperate,
	ScopeAttachmentsWrite, ScopeSettingsRead, ScopeSettingsWrite,
	ScopeAccessAdmin, ScopeSession, ScopeHost,
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
	// TierSession is the floor, below observe: the method asks only that
	// the caller named a live session, and what it may then DO is decided
	// by the call's arguments or by the bucket it writes. Its own tier
	// rather than a shade of observe, because a floor call is not
	// read-only — a device-tier settings write rides it — and the gates
	// that ask "is this observe-tier" are asking whether a read-only
	// session's grants reach a method, which is a different question.
	TierSession ScopeTier = iota + 1
	// TierObserve is read-only access to history and content. The only
	// tier a viewer link or a peer backend can reach.
	TierObserve
	// TierExecute is anything that changes state, drives a provider
	// session, or reaches the host.
	TierExecute
	// TierHost is the residue with no remote form at all. Its own tier
	// rather than a flag on execute: an execute-tier call is one a
	// remote owner device is expected to make, and a host-tier call is
	// one nobody can make from anywhere but this desktop.
	TierHost
)

// scopeTiers is §5's Tier column, declared once. Three observe entries,
// one floor entry, one host entry, and execute for everything else.
var scopeTiers = map[Scope]ScopeTier{
	ScopeSession:          TierSession,
	ScopeThreadsRead:      TierObserve,
	ScopeFilesRead:        TierObserve,
	ScopeSettingsRead:     TierObserve,
	ScopeThreadsOperate:   TierExecute,
	ScopeApprovalsRespond: TierExecute,
	ScopeThreadsAutonomy:  TierExecute,
	ScopeTerminalOperate:  TierExecute,
	ScopePreviewOpen:      TierExecute,
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
