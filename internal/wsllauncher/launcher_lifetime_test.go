package wsllauncher

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestLauncherLifetimeChild(t *testing.T) {
	mode := os.Getenv("AO_TEST_LAUNCHER_LIFETIME")
	if mode == "" {
		return
	}
	fmt.Println("ready")
	if mode == "running" {
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

func TestLauncherStopAfterWaitAndDuringWait(t *testing.T) {
	for _, mode := range []string{"exited", "running"} {
		t.Run(mode, func(t *testing.T) {
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestLauncherLifetimeChild$")
			cmd.Env = append(os.Environ(), "AO_TEST_LAUNCHER_LIFETIME="+mode)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			launcher := &Launcher{cmd: cmd}
			t.Cleanup(func() {
				if err := launcher.Stop(); err != nil {
					t.Error(err)
				}
			})
			if ready, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || ready != "ready\n" {
				t.Fatalf("child readiness = %q, %v", ready, err)
			}
			if mode == "exited" {
				if err := launcher.Wait(); err != nil {
					t.Fatal(err)
				}
			}
			waited := make(chan error, 2)
			for range 2 {
				go func() { waited <- launcher.Wait() }()
			}
			if err := launcher.Stop(); err != nil {
				t.Fatalf("stop %s child: %v", mode, err)
			}
			for range 2 {
				select {
				case err := <-waited:
					if (err == nil) != (mode == "exited") {
						t.Fatalf("wait %s child: %v", mode, err)
					}
				case <-ctx.Done():
					t.Fatal("process wait did not complete after Stop")
				}
			}
		})
	}
}
