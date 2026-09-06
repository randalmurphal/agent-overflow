package app

import (
	"errors"
	"testing"
)

func TestUpdaterBindingsReportUnsupportedWithoutService(t *testing.T) {
	a := &App{}

	availability, err := a.CheckForUpdate()
	if err != nil {
		t.Fatalf("CheckForUpdate: %v", err)
	}
	if availability.Supported {
		t.Fatal("CheckForUpdate reported support without an updater service")
	}
	if _, err := a.ListReleases(); !errors.Is(err, ErrUpdatesUnsupported) {
		t.Fatalf("ListReleases error = %v, want ErrUpdatesUnsupported", err)
	}
	if err := a.DownloadUpdate(""); !errors.Is(err, ErrUpdatesUnsupported) {
		t.Fatalf("DownloadUpdate error = %v, want ErrUpdatesUnsupported", err)
	}
	if err := a.RestartToUpdate(); !errors.Is(err, ErrUpdatesUnsupported) {
		t.Fatalf("RestartToUpdate error = %v, want ErrUpdatesUnsupported", err)
	}
	if err := a.ReportUpdateInstallStatus("failed", "0.0.2", "failed"); !errors.Is(err, ErrUpdatesUnsupported) {
		t.Fatalf("ReportUpdateInstallStatus error = %v, want ErrUpdatesUnsupported", err)
	}
}
