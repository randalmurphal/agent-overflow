package appidentity

// Name is the product name every window title is built from. Exported
// because the isolated windowed harness/soak titles
// ("Agent Overflow (harness · <id>)") are composed outside this package
// but must not re-spell the product.
const Name = "Agent Overflow"

// AppTitle returns the title shown for a running app instance. Mode is
// "dev" for developer launches, "soak" for the soak rig (see profile.go),
// and anything else for production. The title is how a human tells two
// simultaneously visible windows apart, so it tracks the same axis the
// machine-readable identity below does.
func AppTitle(mode string) string {
	switch mode {
	case ModeDev:
		return Name + " (dev)"
	case ModeSoak:
		return Name + " (soak)"
	default:
		return Name
	}
}

// SingleInstanceID returns the stable Wails single-instance ID for an
// app entry point. Mode is "dev" for developer launches, "soak" for the
// soak rig, and anything else for production. Distinct IDs are what let
// a soak instance run alongside the developer's instance instead of
// being bounced into (or bouncing) it.
func SingleInstanceID(kind, mode string) string {
	switch mode {
	case ModeDev:
		return "com.agentoverflow." + kind + ".dev"
	case ModeSoak:
		return "com.agentoverflow." + kind + ".soak"
	default:
		return "com.agentoverflow." + kind
	}
}
