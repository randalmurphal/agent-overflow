package harnessclient

import (
	"context"
	"os"
	"slices"
	"testing"

	"agent-overflow/internal/harness/instanceinfo"
)

// fakeProcess is a processControl over no real process: it says the pid is
// alive and the expected one, and records each signal and answers it as
// told. No signal reaches the operating system.
type fakeProcess struct {
	signals    []string
	stopAnswer error
	killAnswer error
}

func (f *fakeProcess) control() processControl {
	return processControl{
		alive:  func(int) bool { return true },
		verify: func(int, instanceinfo.ProcessIdentity) error { return nil },
		open:   func(int) (processSignaler, error) { return f, nil },
	}
}

func (f *fakeProcess) stop() error {
	f.signals = append(f.signals, "stop")
	return f.stopAnswer
}

func (f *fakeProcess) kill() error {
	f.signals = append(f.signals, "kill")
	return f.killAnswer
}

func currentIdentity() instanceinfo.ProcessIdentity {
	return instanceinfo.ProcessIdentity{StartTime: "1", Executable: "/bin/backend", Namespace: instanceinfo.CurrentPIDNamespace()}
}

// A kill refused because the process already exited proves it gone, even
// when a probe of its pid, which another process may since hold, says
// alive and the identity check passes, as they would for a pid reused by a
// process that looks the same.
func TestTerminateTreatsAKillOfAFinishedProcessAsStopped(t *testing.T) {
	process := &fakeProcess{killAnswer: os.ErrProcessDone}
	if err := terminateVerified(context.Background(), 4242, currentIdentity(), 0, process.control()); err != nil {
		t.Fatalf("terminate of a process that exited = %v, want success", err)
	}
	if want := []string{"stop", "kill"}; !slices.Equal(process.signals, want) {
		t.Fatalf("signals = %v, want %v", process.signals, want)
	}
}

// A polite stop refused because the process already exited is a stop: no
// kill follows it.
func TestTerminateTreatsAStopOfAFinishedProcessAsStopped(t *testing.T) {
	process := &fakeProcess{stopAnswer: os.ErrProcessDone}
	if err := terminateVerified(context.Background(), 4242, currentIdentity(), 0, process.control()); err != nil {
		t.Fatalf("terminate of a reaped process = %v, want success", err)
	}
	if want := []string{"stop"}; !slices.Equal(process.signals, want) {
		t.Fatalf("signals = %v, want %v", process.signals, want)
	}
}
