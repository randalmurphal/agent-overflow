//go:build !ios

package main

// Stub main so `go build ./...` can compile the iOS helper package on
// non-iOS platforms. Real app entry still lives at the repo root.
func main() {}
