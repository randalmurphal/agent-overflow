package split

import (
	"fmt"
	"log"
	"os"
	"testing"
)

// TestMain logs, as the split packages' do: go test prints it among the
// names `go test -list` prints.
func TestMain(m *testing.M) {
	log.Print("fixture: set up")
	os.Exit(m.Run())
}

// The names fill four buckets, TestFails sharing one. TestA, TestAB and
// TestABC are prefixes of each other, so a -run that matched by prefix
// would run one twice.
func TestA(t *testing.T)   {}
func TestAB(t *testing.T)  {}
func TestABC(t *testing.T) {}
func TestC(t *testing.T)   {}
func TestD(t *testing.T)   {}
func TestE(t *testing.T)   {}

func TestFails(t *testing.T) {
	if os.Getenv("FIXTURE_FAIL") == "1" {
		t.Fatal("FIXTURE_FAIL is 1")
	}
}

func FuzzVersion(f *testing.F) {
	f.Add(Version)
	f.Fuzz(func(t *testing.T, v int) {})
}

func ExampleVersion() {
	fmt.Println(Version > 0)
	// Output: true
}
