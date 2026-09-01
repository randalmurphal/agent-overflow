// Package supervise is the stable half of a serve host's launch state:
// the process the platform service manager selects, the backend it runs as
// its child, and the vocabulary the two speak while an update is in flight.
//
// The shape is `docs/specs/remote-access.md` §7's, adopted from t3code's
// server-updates architecture and translated to a single Go binary: a
// supervisor owns which version runs, an update is a staged immutable
// version plus a quiescent SQLite snapshot plus a trial boot parked at an
// activation gate, and every outcome is durable before it is announced.
//
// Nothing here downloads, verifies a signature, or decides that an update
// SHOULD happen. It executes one that has already been staged.
package supervise
