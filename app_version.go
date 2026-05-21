package main

// Version returns the build-stamped semantic version (e.g. "0.0.1") or
// "dev" for unstamped builds. The frontend's Settings footer reads
// this to display the current release. Read-only, no FS / process /
// settings touch — intentionally NOT in LocalOnlyMethods so a
// remote --connect client sees the backend's version too.
func (a *App) Version() string {
	return version
}
