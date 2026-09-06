package appidentity

import (
	"os"
	"strings"
)

// HostDisplayName supplies the default when this installation has no explicit
// DeviceName. It is display metadata only, never a key or trust decision.
func HostDisplayName() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(name)
}
