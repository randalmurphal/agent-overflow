package wsllauncher

// The Windows launcher's distro choice (cmd/agent-overflow-windows). A
// launch starts the --distro override, then the saved choice, then the only
// installed distribution; with none of those the picker runs. An override is
// transient: the launch does not save it, so `make dev-wsl --distro X` keeps
// the user's own choice. A launcher that starts another to continue its
// launch (the relaunch around an update) passes its choice on with
// DistroArgs, so a picker choice stays one the new launcher saves.

const (
	// DistroFlag names the distro a launcher starts.
	DistroFlag = "distro"
	// RememberDistroFlag makes a --distro choice one the launch saves after
	// a successful boot, as a picker choice is.
	RememberDistroFlag = "remember-distro"
)

// ChooseDistro picks the distro a launch starts from override (--distro),
// remember (--remember-distro), the saved choice and the installed
// distributions. transient is true when the launch must not save its
// choice. chosen is "" when the picker, or the page for a missing WSL, runs
// instead; an override that names no installed distribution is that case,
// never a fall-back to the saved choice.
func ChooseDistro(override string, remember bool, saved string, distros []Distro) (chosen string, transient bool) {
	if override != "" {
		for _, d := range distros {
			if d.Name == override {
				return d.Name, !remember
			}
		}
		return "", false
	}
	if saved != "" {
		for _, d := range distros {
			if d.Name == saved {
				return d.Name, false
			}
		}
	}
	// Nothing to choose between. The choice is saved on success, so a
	// second distribution installed later does not bring the picker back.
	if len(distros) == 1 {
		return distros[0].Name, false
	}
	return "", false
}

// DistroArgs is the argv that has a launcher this one starts launch distro
// with this launch's choice: transient, or saved after a successful boot.
func DistroArgs(distro string, transient bool) []string {
	args := []string{"--" + DistroFlag, distro}
	if !transient {
		args = append(args, "--"+RememberDistroFlag)
	}
	return args
}
