package codex

// codexAppServerArgs pins Agent Overflow's Codex processes to Codex's native
// auth.json store. Codex's "auto" credential mode may select an OS keyring,
// which cannot be atomically swapped as an opaque per-account file. The
// override affects credentials only; CODEX_HOME remains canonical for normal
// sessions and temporary only for login/inactive-account probes.
func codexAppServerArgs() []string {
	return []string{
		"-c",
		`cli_auth_credentials_store="file"`,
		"app-server",
	}
}

// codexSharedAppServerArgs adds immediate unload on thread/unsubscribe for
// long-lived multiplexed app-server connections so unsubscribed threads
// release their writer locks and MCP children right away instead of waiting
// out Codex's 60-second default delay.
func codexSharedAppServerArgs() []string {
	return []string{
		"-c",
		`cli_auth_credentials_store="file"`,
		"-c",
		"thread_unload_delay_secs=0",
		"app-server",
	}
}
