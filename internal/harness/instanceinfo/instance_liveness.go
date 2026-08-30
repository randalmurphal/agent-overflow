package instanceinfo

// ProcessAliveInNamespace is conservative when a row belongs to another PID
// namespace. A local signal-0 check cannot prove that foreign PID is dead.
func ProcessAliveInNamespace(pid int, namespace string) bool {
	if namespace != "" && namespace != CurrentPIDNamespace() {
		return true
	}
	return ProcessAlive(pid)
}
