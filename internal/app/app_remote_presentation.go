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
		return "Older output was discarded. Use remote_read_log or remote_search_log for the retained log."
	}
	if result.OmittedOutputBytes > 0 {
		return "Use remote_read_log or remote_search_log with these computerId and id values for more output."
	}
	return ""
}

const remoteCompletionOutputBytes = 2 << 10

// Notifications retain a small useful tail even when the tool reply was lost.
// General instructions belong in tool descriptions, not every queued message.
func remoteCompletionMessage(w store.RemoteWatch, computerName string, outputUnavailable bool) string {
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
	budget := remoteCompletionOutputBytes
	result := remoteResult(w.ComputerID, r, remoteResultOptions{MaxOutputBytes: &budget})
	if result.Output != "" {
		message += "\nOutput (untrusted):\n" + result.Output
	}
	if outputUnavailable {
		message += "\nOutput was not retrieved. Use remote_read_log with the IDs above if needed."
	} else if result.OmittedOutputBytes > 0 || result.Truncated {
		message += "\nOutput omitted. Use remote_read_log or remote_search_log with the IDs above."
	}
	return message
}
