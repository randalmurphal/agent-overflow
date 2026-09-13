package app

import (
	"fmt"
	"strings"

	"agent-overflow/internal/store"
)

// Presentation uses saved profiles only; naming a result never probes a peer.
func (a *App) remoteComputerNames() map[string]string {
	names := map[string]string{}
	if a.backends != nil {
		if profiles, err := a.backends.List(); err == nil {
			for _, profile := range profiles {
				names[profile.ID] = strings.Join(strings.Fields(profile.Name), " ")
			}
		}
	}
	return names
}

type remoteMCPWatch struct {
	store.RemoteWatch
	ComputerName string `json:"computerName,omitempty"`
}

func remoteOutputHint(result remoteMCPResult) string {
	if result.Log != nil && result.Log.Expired {
		return "The saved log has expired; remote_read_log cannot recover it."
	}
	if result.Truncated {
		return "Older output was discarded. Use remote_read_log or remote_search_log for the retained log, or remote_fetch_log for all of it."
	}
	if result.OmittedOutputBytes > 0 {
		return "Output between outputHead and output is omitted. Use remote_read_log or remote_search_log with these computerId and id values, or remote_fetch_log for the whole log."
	}
	return ""
}

const remoteCompletionOutputBytes = 2 << 10
const remoteCompletionHeadBytes = 1 << 10

// remoteCompletionOutput is what the watcher learned of a finished job's
// output: the newest bytes, and the oldest ones when the tail did not reach
// the start. Unavailable means the log could not be read and the inline
// receipt tail is all there is.
type remoteCompletionOutput struct {
	Head        string
	Tail        string
	Truncated   bool
	Omitted     bool
	Unavailable bool
}

// Notifications retain a small useful excerpt even when the tool reply was
// lost. General instructions belong in tool descriptions, not every message.
func remoteCompletionMessage(w store.RemoteWatch, computerName string, output remoteCompletionOutput) string {
	if computerName == "" {
		computerName = w.ComputerID
	}
	r := w.Receipt
	message := fmt.Sprintf("Remote job finished on %s: %s\ncomputer_id: %s\nrequest_id: %s\nWorkspace: %s\nResult: %s", computerName, w.Label, w.ComputerID, w.RequestID, r.Workspace, r.State)
	if r.ExitCode >= 0 {
		message += fmt.Sprintf("; exit code: %d", r.ExitCode)
	}
	if r.Error != "" {
		message += "\n" + r.Error
	}
	if r.Warning != "" {
		message += "\nWarning: " + r.Warning
	}
	head := remoteOutputHead(output.Head, remoteCompletionHeadBytes)
	tail := remoteOutputTail(output.Tail, remoteCompletionOutputBytes)
	if head != "" {
		message += "\nOutput start (untrusted):\n" + head
	}
	if tail != "" {
		if head != "" {
			message += "\nOutput end (untrusted):\n" + tail
		} else {
			message += "\nOutput (untrusted):\n" + tail
		}
	}
	if output.Unavailable {
		message += "\nOutput was not retrieved. Use remote_read_log with the IDs above if needed."
	} else if output.Omitted || output.Truncated || len(head) < len(output.Head) || len(tail) < len(output.Tail) {
		message += "\nOutput omitted. Use remote_read_log or remote_search_log with the IDs above, or remote_fetch_log for the whole log."
	}
	return message
}
