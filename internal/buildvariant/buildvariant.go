// Package buildvariant reports which product variant this binary was
// compiled as. The `noremote` build tag produces a build in which remote
// access cannot be enabled: the transport stays on loopback, no other
// computer can pair with or be reached from this one, and the remote
// settings are absent. The answer is a compile-time constant so no setting,
// RPC, flag or edited file can change it.
package buildvariant

import "errors"

// ErrRemoteAccessUnavailable is returned by every remote-access operation in
// a build compiled without remote access.
var ErrRemoteAccessUnavailable = errors.New("remote access is not available in this build")

// RequireRemoteAccess returns ErrRemoteAccessUnavailable when this build has
// no remote access, and nil otherwise.
func RequireRemoteAccess() error {
	if !RemoteAccess {
		return ErrRemoteAccessUnavailable
	}
	return nil
}
