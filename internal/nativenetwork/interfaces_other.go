//go:build !windows

package nativenetwork

import "net"

// The native bridge is Windows-only; portable loopback fixtures call relay
// internals directly and never enumerate or expose the developer's network.
func physicalInterface(net.Interface) bool { return false }
