// Package trackerapp is the application service for issue trackers: this
// computer's tracker connections, their key files and the clients built
// over them. The design is docs/architecture/tracker-backend.md.
//
// A connection is a row in tracker_connections and a key file beside the
// app's other credentials holding its token alone. The service validates
// and normalizes what a connection names, makes and removes the key file
// with the row, builds each connection's client on first use with a
// credential source that reads the key file, closes it when the
// connection is turned off or removed, verifies a token with one identity
// call, and holds each connection's last problem in memory. Provider
// hands the tracker.Provider of a connection to the parts built on it.
//
// The service also fills and reads each connection's mirror (the store's
// tracker_cache_* tables): mirror_map.go converts the neutral model to
// the store's types and owns the data blobs; resources.go holds the one
// read per mirrored resource that sync and writes share; a scope's tick
// (tick.go) runs the units of sync it is asked for (sync_*.go), each a
// bounded amount of work, under a lock per scope, and returns, starting
// no goroutine; views*.go are the pane's reads as the DTOs the app
// binds. Those read the mirror alone, except IssueDetail: a stored detail
// is current only when the tracker says so in that call, and
// MirroredDetail is the stored copy for a first paint. Turning a
// connection off detaches its mirror, and a tick that outlives its mirror,
// or names a board no project lists, stores nothing and ends (unit.go);
// the cleanup drains what a detach, an unlisted board or an unlinked issue
// leaves (cleanup.go).
//
// Clients subscribe (subscriptions.go): each subscribed board and each
// open ticket has one pump, a goroutine that ticks on Cadence while a
// handle shows it (pump_board.go, pump_ticket.go). What every tick stores
// passes through one change funnel (funnel.go), which turns it into the
// events each subscription needs, sent through Deps callbacks; a client
// patches the sections it holds from an event with one read (patch.go).
// One loop for the service syncs linked issues and finishes the mirror
// cleanup (linked_loop.go). New starts nothing: StartLinkedSync arms the
// loop, and a subscribe starts its pump.
//
// The service receives its dependencies as capabilities (Deps) and owns
// its locks and its stop: Stop ends every pump and the loop, closes every
// client and waits for the calls in flight and the goroutines. Nothing it
// returns, reports or announces carries a token. Failures are returned,
// held as the connection's problem when they are a tracker's answer, sent
// to subscribers as a pump's failure, or reported to Deps.LoopFailed; it
// logs what has no caller: a mirror drain that failed after the change
// that caused it succeeded, a pump's failure, and a failed lookup of a
// change's boards.
package trackerapp
