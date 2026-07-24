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
