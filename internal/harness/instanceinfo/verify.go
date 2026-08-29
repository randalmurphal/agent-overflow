package instanceinfo

import "fmt"

// VerifyProcessIdentity confirms all evidence present in a record. Missing
// evidence is not a match when the record claims it, which prevents a
// platform or permissions failure from silently becoming PID-only signalling.
func VerifyProcessIdentity(pid int, expected ProcessIdentity) error {
	if expected.StartTime == "" || expected.Executable == "" || expected.Namespace == "" {
		return fmt.Errorf("process %d identity is incomplete; destructive operations require start time, executable, and namespace", pid)
	}
	actual, err := CaptureProcessIdentity(pid)
	if err != nil {
		return err
	}
	if expected.StartTime != "" && actual.StartTime != expected.StartTime {
		return fmt.Errorf("process %d start time %q does not match recorded %q", pid, actual.StartTime, expected.StartTime)
	}
	if expected.Executable != "" && actual.Executable != expected.Executable {
		return fmt.Errorf("process %d executable %q does not match recorded %q", pid, actual.Executable, expected.Executable)
	}
	if expected.Namespace != "" && actual.Namespace != expected.Namespace {
		return fmt.Errorf("process %d pid namespace %q does not match recorded %q", pid, actual.Namespace, expected.Namespace)
	}
	return nil
}
