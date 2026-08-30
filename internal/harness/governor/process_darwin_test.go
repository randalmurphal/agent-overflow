//go:build darwin

package governor

import (
	"os"
	"testing"
)

func TestDarwinProcessTreeRSSSamplesCurrentProcess(t *testing.T) {
	rss, err := (darwinProcesses{}).RSS(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if rss == 0 {
		t.Fatal("current process tree RSS is zero")
	}
}
