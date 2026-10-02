# Login-shell environment

This package performs the one startup probe that merges the user's login and
interactive shell PATH, plus the TLS certificate and proxy variables in
`importedVars`, into the GUI application's environment. Provider settings
remain the explicit override for unusual shell setups.

Sync runs once from main before provider detection. On Windows it is inert
because the launcher does not start provider children. On Unix, select shell
candidates deterministically, invoke a bounded login-interactive command, parse
only the values between unique sentinels, and merge PATH entries without
duplicates. Shell banners and startup output outside the sentinels are ignored.

Failure to start, timeout, nonzero exit, or malformed sentinel output returns an
error and leaves PATH and the imported variables unchanged. Startup may continue
so provider detection can surface the missing binary. Do not hard-code nvm,
Homebrew, user-bin, or other installation paths.

A value the process inherited wins over the shell's. When any proxy variable is
set, `NO_PROXY`/`no_proxy` gain the loopback names, because children reach the
app at `http://[::1]:port`; this runs even when the probe fails. Add a variable
to `importedVars` only for a concrete child-process requirement; credentials
and provider settings belong to provider accounts, never this probe.

Do not repeat Sync or let callers independently merge inherited environment.
Tests use fake shell executables and cover noisy startup, missing sentinels,
timeout, failure, duplicate paths, inherited precedence, the loopback proxy
bypass, and unchanged fallback behavior.
