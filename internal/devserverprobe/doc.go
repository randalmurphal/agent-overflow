// Package devserverprobe answers whether a loopback HTTP(S) URL a
// command announced currently has a listener, backing the dev-server
// chip's liveness gate.
//
// The chip claims "a server is running here that you can open".
// Triage's textual scan (internal/triage/dev_server_url.go) only
// proves the output MENTIONED a loopback URL — a `tail` of a file
// containing "http://localhost:5173" produces the same meta as a Vite
// startup banner. The TCP dial here is the ground truth that separates
// the two; detection stays a candidate generator.
package devserverprobe
