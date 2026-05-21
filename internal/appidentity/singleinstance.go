package appidentity

// SingleInstanceID returns the stable Wails single-instance ID for an
// app entry point. Mode is "dev" for developer launches and anything
// else for production.
func SingleInstanceID(kind, mode string) string {
	if mode == "dev" {
		return "com.agentoverflow." + kind + ".dev"
	}
	return "com.agentoverflow." + kind
}
