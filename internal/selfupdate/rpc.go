package selfupdate

// The launcher reports install-directive outcomes back to the backend over
// the transport RPC surface. Both sides of the wire import these so the
// method name and stage vocabulary cannot drift.
const (
	// RPCReportStatus is the App-bound method the launcher invokes:
	// ReportUpdateInstallStatus(stage, version, message).
	RPCReportStatus = "ReportUpdateInstallStatus"

	// StatusProceeding acknowledges a directive: the staged file exists and
	// the launcher is about to install and quit. Message is empty.
	StatusProceeding = "proceeding"

	// StatusFailed reports a terminal install failure; the launcher stays
	// alive on the old version. Message carries the human-readable reason.
	StatusFailed = "failed"
)
