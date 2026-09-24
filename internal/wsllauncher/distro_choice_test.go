package wsllauncher

import (
	"flag"
	"io"
	"testing"
)

func TestChooseDistro(t *testing.T) {
	distros := []Distro{
		{Name: "Ubuntu-24.04", Default: true, Version: 2, State: "Running"},
		{Name: "Debian", Version: 2, State: "Stopped"},
	}
	solo := distros[:1]
	for _, c := range []struct {
		name          string
		override      string
		remember      bool
		saved         string
		distros       []Distro
		wantChosen    string
		wantTransient bool
	}{
		{"override is transient", "Debian", false, "Ubuntu-24.04", distros, "Debian", true},
		{"a remembered override is saved", "Debian", true, "Ubuntu-24.04", distros, "Debian", false},
		{"override beats the saved choice", "Debian", false, "Debian", distros, "Debian", true},
		{"an unknown override shows the picker", "Fedora", false, "Ubuntu-24.04", distros, "", false},
		{"an unknown remembered override shows the picker", "Fedora", true, "Ubuntu-24.04", distros, "", false},
		{"the saved choice", "", false, "Ubuntu-24.04", distros, "Ubuntu-24.04", false},
		{"a stale saved choice shows the picker", "", false, "Removed", distros, "", false},
		{"nothing saved shows the picker", "", false, "", distros, "", false},
		{"no distributions", "Ubuntu", false, "", nil, "", false},
		{"the only distribution", "", false, "", solo, "Ubuntu-24.04", false},
		{"the only distribution over a stale saved choice", "", false, "Removed", solo, "Ubuntu-24.04", false},
		{"override beats the only distribution", "Ubuntu-24.04", false, "", solo, "Ubuntu-24.04", true},
		{"an unknown override never falls to the only distribution", "Fedora", false, "", solo, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			chosen, transient := ChooseDistro(c.override, c.remember, c.saved, c.distros)
			if chosen != c.wantChosen || transient != c.wantTransient {
				t.Fatalf("ChooseDistro = %q, %v; want %q, %v", chosen, transient, c.wantChosen, c.wantTransient)
			}
		})
	}
}

// A launcher started with DistroArgs chooses what the launch that started
// it chose, and saves it exactly when that launch would have: a picker or
// saved choice stays one, and an override stays transient.
func TestDistroArgsCarryTheChoiceAcrossARelaunch(t *testing.T) {
	distros := []Distro{{Name: "Ubuntu-24.04"}, {Name: "Debian"}}
	for _, transient := range []bool{false, true} {
		fs := flag.NewFlagSet("launcher", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		distro := fs.String(DistroFlag, "", "")
		remember := fs.Bool(RememberDistroFlag, false, "")
		if err := fs.Parse(DistroArgs("Debian", transient)); err != nil {
			t.Fatal(err)
		}
		// The relaunched launcher's saved choice is another distro, which
		// a picker choice not yet saved must not lose to.
		chosen, gotTransient := ChooseDistro(*distro, *remember, "Ubuntu-24.04", distros)
		if chosen != "Debian" || gotTransient != transient {
			t.Fatalf("transient=%v: relaunch chose %q, transient %v", transient, chosen, gotTransient)
		}
	}
}
