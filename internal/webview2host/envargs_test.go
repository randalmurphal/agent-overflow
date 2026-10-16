package webview2host

import "testing"

func TestPaneEnvironmentArgsLeadsWithTheHostsPortThenTheExtras(t *testing.T) {
	if got, want := paneEnvironmentArgs(9229, nil), "--remote-debugging-port=9229"; got != want {
		t.Fatalf("no extras: %q, want %q", got, want)
	}
	got := paneEnvironmentArgs(9229, []string{"--disable-gpu-vsync", "--disk-cache-size=1"})
	if want := "--remote-debugging-port=9229 --disable-gpu-vsync --disk-cache-size=1"; got != want {
		t.Fatalf("with extras: %q, want %q", got, want)
	}
}

func TestValidateExtraBrowserArgsRefusesWhatWouldBreakThePaneEnvironment(t *testing.T) {
	if err := ValidateExtraBrowserArgs(nil); err != nil {
		t.Fatalf("nil: %v", err)
	}
	if err := ValidateExtraBrowserArgs([]string{"--disable-gpu-vsync", "--a=b"}); err != nil {
		t.Fatalf("valid switches: %v", err)
	}
	for _, bad := range [][]string{
		{"--remote-debugging-port=1"},
		{"--remote-debugging-address=0.0.0.0"},
		{"disable-gpu-vsync"},
		{"--"},
		{"--a b"},
		{"--a\tb"},
		{""},
	} {
		if err := ValidateExtraBrowserArgs(bad); err == nil {
			t.Errorf("ValidateExtraBrowserArgs(%q) = nil, want an error", bad)
		}
	}
}
