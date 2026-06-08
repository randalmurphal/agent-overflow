package appidentity

const appName = "Agent Overflow"

// AppTitle returns the title shown for a running app instance. Mode is
// "dev" for developer launches and anything else for production.
func AppTitle(mode string) string {
	if mode == "dev" {
		return appName + " (dev)"
	}
	return appName
}

// SingleInstanceID returns the stable Wails single-instance ID for an
// app entry point. Mode is "dev" for developer launches and anything
// else for production.
func SingleInstanceID(kind, mode string) string {
	if mode == "dev" {
		return "com.agentoverflow." + kind + ".dev"
	}
	return "com.agentoverflow." + kind
}
