package procutil

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// parentDeathHelperEnv makes TestParentDeathHelper the process that starts a
// group and is then killed. Its value is "end" or "keep".
const parentDeathHelperEnv = "PROCUTIL_PARENT_DEATH_HELPER"

// TestParentDeathHelper is not a test of its own: the test below runs this
// binary as the parent that dies.
func TestParentDeathHelper(t *testing.T) {
	mode := os.Getenv(parentDeathHelperEnv)
	if mode == "" {
		t.Skip("runs only as the parent TestEndWithParentEndsTheLeaderWhenItsStarterDies kills")
	}
	_, err := StartGroup(context.Background(), GroupCommand{
		Argv: []string{"sleep", "600"}, Output: io.Discard, EndWithParent: mode == "end",
		Started: func(pid int) { fmt.Printf("leader %d\n", pid) },
	})
	if err != nil {
		t.Fatal(err)
	}
	select {}
}

// A command started with EndWithParent ends when the process that started
// it is killed, which nothing in that process can arrange. One started
// without it outlives its parent, which is what the first case would show
// if the signal were not set.
func TestEndWithParentEndsTheLeaderWhenItsStarterDies(t *testing.T) {
	for _, c := range []struct {
		mode string
		ends bool
	}{{"end", true}, {"keep", false}} {
		t.Run(c.mode, func(t *testing.T) {
			t.Parallel()
			helper := exec.Command(os.Args[0], "-test.run=^TestParentDeathHelper$")
			helper.Env = append(os.Environ(), parentDeathHelperEnv+"="+c.mode)
			stdout, err := helper.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := helper.Start(); err != nil {
				t.Fatal(err)
			}
			leader := 0
			scanner := bufio.NewScanner(stdout)
			for leader == 0 && scanner.Scan() {
				if rest, ok := strings.CutPrefix(scanner.Text(), "leader "); ok {
					leader, _ = strconv.Atoi(rest)
				}
			}
			if leader == 0 {
				_ = helper.Process.Kill()
				_ = helper.Wait()
				t.Fatalf("the helper reported no leader: %v", scanner.Err())
			}
			t.Cleanup(func() { _ = SignalGroup(leader, syscall.SIGKILL) })
			if err := helper.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = helper.Wait()

			deadline := time.Now().Add(5 * time.Second)
			for running(leader) && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if got := !running(leader); got != c.ends {
				t.Fatalf("leader ended = %v after its parent was killed, want %v", got, c.ends)
			}
		})
	}
}
