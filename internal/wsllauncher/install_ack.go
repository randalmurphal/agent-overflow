package wsllauncher

import "errors"

// InstallAckOutcome is what the result of a StatusProceeding report means for
// the swap the launcher is about to perform. The three cases are not
// interchangeable, and collapsing any two of them produces a user-visible
// contradiction, so the decision lives here where it is testable off-Windows
// rather than inside the launcher's Windows-only driver.
type InstallAckOutcome int

const (
	// InstallAckAccepted: the backend recorded the acknowledgement and is
	// holding its side open for the swap. Proceed.
	InstallAckAccepted InstallAckOutcome = iota

	// InstallAckRefused: the backend answered and rejected the report, so it
	// provably did not take effect. Either its own acknowledgement deadline
	// already won the race and unwound the install (marker cleared, busy fence
	// released, error shown to the user), or the directive is stale. Swapping
	// now would replace the binary moments after the user was told the update
	// failed, so the install must be abandoned instead.
	InstallAckRefused

	// InstallAckUndelivered: no answer came back — a timeout, a disconnect, or
	// no connection at all. This is ambiguous, not negative: the report may
	// have been delivered with only its response lost, in which case the
	// backend accepted it and is waiting to be torn down. Proceed. Worst case
	// the user sees a spurious "the launcher did not respond" error followed
	// by a successful restart into the new version; abandoning instead risks
	// stranding a backend that is holding its fence for a swap that never
	// comes.
	InstallAckUndelivered
)

func (o InstallAckOutcome) String() string {
	switch o {
	case InstallAckAccepted:
		return "accepted"
	case InstallAckRefused:
		return "refused"
	case InstallAckUndelivered:
		return "undelivered"
	}
	return "unknown"
}

// ClassifyInstallAck maps the error from a StatusProceeding report to what the
// launcher should do next. Any server-answered rejection is a refusal — that
// includes version skew (method_not_found, bad_params) as well as the
// backend's semantic refusals, since none of them ran.
func ClassifyInstallAck(err error) InstallAckOutcome {
	if err == nil {
		return InstallAckAccepted
	}
	var refused *RPCRefusedError
	if errors.As(err, &refused) {
		return InstallAckRefused
	}
	return InstallAckUndelivered
}
