package app

import (
	"errors"
	"reflect"
	"testing"

	"agent-overflow/internal/appupdate"
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

func TestUpdaterWireAdaptersPreserveEveryField(t *testing.T) {
	availability := appupdate.UpdateAvailability{
		Supported:        true,
		Available:        true,
		CurrentVersion:   "0.0.7",
		LatestVersion:    "0.0.9",
		ReleaseName:      "release name",
		ReleaseNotes:     "release notes",
		LastApplyFailure: "apply failure",
		CheckError:       "check error",
	}
	wantAvailability := UpdateAvailability{
		Supported:        true,
		Available:        true,
		CurrentVersion:   "0.0.7",
		LatestVersion:    "0.0.9",
		ReleaseName:      "release name",
		ReleaseNotes:     "release notes",
		LastApplyFailure: "apply failure",
		CheckError:       "check error",
	}
	if got := wireUpdateAvailability(availability); got != wantAvailability {
		t.Fatalf("wireUpdateAvailability() = %+v, want %+v", got, wantAvailability)
	}

	releases := []appupdate.ReleaseSummary{{
		Tag:         "v0.0.9",
		Version:     "0.0.9",
		Name:        "release name",
		PublishedAt: "2026-08-26T00:00:00Z",
		Prerelease:  true,
		IsLatest:    true,
		IsCurrent:   true,
		IsOlder:     true,
	}}
	wantReleases := []ReleaseSummary{{
		Tag:         "v0.0.9",
		Version:     "0.0.9",
		Name:        "release name",
		PublishedAt: "2026-08-26T00:00:00Z",
		Prerelease:  true,
		IsLatest:    true,
		IsCurrent:   true,
		IsOlder:     true,
	}}
	if got := wireReleaseSummaries(releases); !reflect.DeepEqual(got, wantReleases) {
		t.Fatalf("wireReleaseSummaries() = %+v, want %+v", got, wantReleases)
	}
}
