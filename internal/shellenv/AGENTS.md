# Login-shell environment

This package performs the one startup probe that merges the user's login and
interactive shell PATH into the GUI application's environment. Provider
settings remain the explicit override for unusual shell setups.

Sync runs once from main before provider detection. On Windows it is inert
because the launcher does not start provider children. On Unix, select shell
candidates deterministically, invoke a bounded login-interactive command, parse
only the value between unique sentinels, and merge entries without duplicates.
Shell banners and startup output outside the sentinels are ignored.

Failure to start, timeout, nonzero exit, or malformed sentinel output returns an
error and leaves PATH unchanged. Startup may continue so provider detection can
surface the missing binary. Do not hard-code nvm, Homebrew, user-bin, or other
installation paths.

Keep the probe limited to PATH unless a concrete child-process requirement
justifies another variable. Do not repeat Sync or let callers independently
merge inherited environment. Tests use fake shell executables and cover noisy
startup, missing sentinels, timeout, failure, duplicate paths, and unchanged
fallback behavior.
