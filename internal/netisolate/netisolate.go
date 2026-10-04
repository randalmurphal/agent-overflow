// Package netisolate runs a command inside new user and network namespaces
// that hold only loopback and a private LAN interface (LANName at LANAddress)
// with no route off it. Test suites run there so a test that binds every
// interface, dials out or probes the LAN reaches nothing beyond the
// namespace, whatever the host's networking mode.
//
// An executable that isolates commands must dispatch HelperArg before
// anything else in main (and in TestMain for tests):
//
//	if len(os.Args) > 1 && os.Args[1] == netisolate.HelperArg {
//		os.Exit(netisolate.RunHelper(os.Args[2:], os.Stderr))
//	}
package netisolate

// HelperArg is argv[1] when an executable re-runs itself as the first
// process inside the namespaces.
const HelperArg = "__ao-netisolate-helper"
