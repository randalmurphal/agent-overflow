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
// The service receives its dependencies as capabilities (Deps) and owns
// its locks and its stop: Stop closes every client and waits for the
// calls in flight. Nothing it returns, reports or announces carries a
// token. It logs nothing; failures are returned, or held as the
// connection's problem when they are a tracker's answer.
package trackerapp
