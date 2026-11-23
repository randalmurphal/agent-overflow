package fswatch

// StartGated is Start with gate run just before each handler or Emit call
// is admitted, so a test can begin Close exactly at that point.
func StartGated(config Config, source Source, handle Handler, gate func()) (*Loop, error) {
	return start(config, source, handle, gate)
}
