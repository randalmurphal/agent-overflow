package main

import "agent-overflow/internal/dirbrowse"

// BrowseDirectory lists the contents of path for the project-picker
// UI. The full contract (path normalisation, ordering, .git-marker
// detection, EntryLimit truncation) lives in internal/dirbrowse.
func (a *App) BrowseDirectory(path string) (dirbrowse.Listing, error) {
	return dirbrowse.Browse(path)
}
