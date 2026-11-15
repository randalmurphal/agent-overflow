package providerupdate

import "testing"

func TestNewerOrdersTheProvidersVersions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		candidate, installed string
		want                 bool
	}{
		{"2.1.325", "2.1.321", true},
		{"2.1.321", "2.1.325", false},
		{"2.1.10", "2.1.9", true}, // numeric, not lexical
		{"0.169.0", "0.169.0-alpha.5", true},
		{"0.169.0-alpha.5", "0.169.0", false}, // a prerelease sorts before its release
		{"0.171.0-alpha.5", "0.169.0", true},
		{"1.2.0", "1.2", false}, // equal
		{"1.2", "1.2.0", false},
		{"1.2.3+build.7", "1.2.3", false}, // build ignored
		{"0.0.2504291921", "0.0.2504291920", true},
		{" 2.1.325\n", "2.1.321", true},
	} {
		got, err := newer(tc.candidate, tc.installed)
		if err != nil || got != tc.want {
			t.Errorf("newer(%q, %q) = %v, %v; want %v", tc.candidate, tc.installed, got, err, tc.want)
		}
	}
}

func TestNewerRefusesWhatIsNotAVersion(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "v1.2.3", "latest", "<html>", "1..2", "1.2.3-", "1.2,abc"} {
		if _, err := newer(raw, "1.0.0"); err == nil {
			t.Errorf("candidate %q compared", raw)
		}
		if _, err := newer("1.0.0", raw); err == nil {
			t.Errorf("installed %q compared", raw)
		}
		if _, err := requireVersion(raw, "host"); err == nil {
			t.Errorf("requireVersion(%q) accepted", raw)
		}
	}
}

func TestSharedComponents(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ a, b, want string }{
		{"2.1.321", "2.1.325", "2.1."},
		{"2.1.3", "2.1.30", "2.1."}, // a character prefix would be "2.1.3"
		{"0.163.0", "0.169.0", "0."},
		{"0.16.0", "0.169.0", "0."},
		{"1.9.0", "2.0.0", ""},
		{"1.2", "1.2.1", "1.2."},
		{"1.2.3-beta", "1.2.3-beta", "1.2."},
	} {
		if got := sharedComponents(tc.a, tc.b); got != tc.want {
			t.Errorf("sharedComponents(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}
