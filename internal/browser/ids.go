package browser

// The identifiers only this package uses, one defined type per kind, so a
// value of one kind cannot be passed where another is meant. Each is
// converted from or to a plain string once, where it enters or leaves the
// package; a kind the subpackages share is in internal/browser/ids.

// frameID is an engine's frame identity, which a download start and a
// request's owners name: a CDP frame id, whose main frame's is its page's
// target id; a WebKit or fake page's handle, its only frame; or, on the
// hosted engine, the launcher page id a download report names.
type frameID string
