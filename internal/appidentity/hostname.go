package appidentity

import (
	"os"
	"strings"
)

// HostDisplayName is the name this machine is shown under: its hostname,
// because that is what a person recognises about the machine in front of
// them and the machine they are pairing to.
//
// One function for every surface that needs it. Two callers today and
// they must agree, because they name the SAME backend from opposite
// ends: the pairing payload's display name is what a device shows while
// it decides whether to trust the offer, and the hello frame's
// `backendName` is what the page then labels that backend with in the
// machine picker (docs/specs/remote-access.md §10, "Machine name"). A
// second os.Hostname call would be a second answer to one question.
//
// Convenience only, and never matched against anything: nothing
// authorizes on this string, so an unreadable hostname is an empty
// answer rather than a failure. Callers render the empty case as
// "unknown machine" or fall back to the backend id.
func HostDisplayName() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(name)
}
