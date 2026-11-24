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

// A signal refused because the process already exited proves it gone.
// A refused kill does so even when a probe of its pid, which another
// process may since hold, says alive and the identity check passes, as
// they would for a pid reused by a process that looks the same. A refused
// polite stop is a stop: no kill follows it.
func TestTerminateTreatsASignalToAFinishedProcessAsStopped(t *testing.T) {
	for _, tc := range []struct {
		name    string
		process *fakeProcess
		want    []string
	}{
		{"kill of a finished process", &fakeProcess{killAnswer: os.ErrProcessDone}, []string{"stop", "kill"}},
		{"stop of a finished process", &fakeProcess{stopAnswer: os.ErrProcessDone}, []string{"stop"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := terminateVerified(context.Background(), 4242, currentIdentity(), 0, tc.process.control()); err != nil {
				t.Fatalf("terminate of a process that exited = %v, want success", err)
			}
			if !slices.Equal(tc.process.signals, tc.want) {
				t.Fatalf("signals = %v, want %v", tc.process.signals, tc.want)
			}
		})
	}
}
