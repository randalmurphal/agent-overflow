package testparallel

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if err := CapDefault(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// A default at or under the cap stays, and so does any -parallel given,
// under it as well as over it, and a fuzzing run's.
func TestCapDefaultLowersOnlyADefaultOverTheCap(t *testing.T) {
	CheckApplied(t)
	for _, c := range []struct {
		gomaxprocs string
		args       []string
		want       string
	}{
		{"2", nil, "2"},
		{"4", nil, "4"},
		{"8", []string{"-test.parallel=1"}, "1"},
		{"8", []string{"-test.fuzz=FuzzNothing"}, "8"},
	} {
		if got := Settled(t, c.gomaxprocs, c.args...); got != c.want {
			t.Errorf("GOMAXPROCS %s %v: parallel %s, want %s", c.gomaxprocs, c.args, got, c.want)
		}
	}
}
