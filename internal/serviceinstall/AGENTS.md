# internal/serviceinstall/

Installs `agent-overflow serve` as a per-user background service: a systemd
user unit on Linux, a launchd LaunchAgent on macOS. It generates the unit file,
hands the manager its own commands, and reports what the manager says back.

Reached from one place: the `service` verb in `internal/aocli`
(`service.go`). Operator-facing documentation is
[serve-mode.md](../../docs/architecture/serve-mode.md).

## Two rules the package is built around

**The host is a string, not a build tag.** `Config.GOOS` selects the manager,
so both unit-file formats generate on any machine and both are golden-tested in
full on every `make go-test`. Behind a build tag, the launchd plist would be
reviewed only by whoever happens to be at a Mac, which is how a plist that
never loads ships. Only the COMMANDS are platform-bound, and those never run in
a test.

**Every external command goes through `Runner`, and `New` refuses a nil one.**
There is exactly one real implementation, `ExecRunner`, and no test may
construct it. A test that forgot the fake fails at construction instead of
enabling a service on the developer's own login — the same
mocking-is-mandatory-by-default posture `internal/kerneltest` takes for
provider spawns, for the same reason: `make go-test` runs on somebody's real
machine.

## What the generated units must keep

- **`Restart=on-failure`, and its launchd equivalent
  `KeepAlive/SuccessfulExit=false`.** Not `always`: a clean exit is the
  operator stopping the backend, and a supervisor that restarts one of those
  cannot be stopped. A saved `network.listenPort` that will not bind exits
  non-zero, which IS a failure and is worth retrying.
- **Quoting is not cosmetic.** systemd's `%` introduces a specifier and its
  whitespace splits argv, so `systemdQuote` escapes and quotes; a newline
  cannot be represented in a unit value at all and is refused rather than
  mangled. The plist runs every value through `xml.EscapeText`. A home
  directory with an ampersand in it is a valid home directory.
- **Absolute paths only.** A service manager starts the unit with none of the
  installing shell's context, so a relative path is refused at construction
  rather than written into a service that silently never starts.
- **`ConfigHome` is honored on Linux.** systemd reads user units from
  `$XDG_CONFIG_HOME/systemd/user`. A host that sets it and a unit written to
  `~/.config` never meet.
- **`LaunchdLabel` is not the app bundle identifier.** A Mac can run the
  desktop app and a serve agent at once, and launchd tells services apart by
  label.

## What it deliberately does not do

- **It never enables lingering.** `loginctl enable-linger` changes how the
  user's session behaves for everything on the machine. `Notes()` names the
  command; the operator runs it. A test asserts install issues no such
  command.
- **It never removes data.** `uninstall` stops the service and deletes the
  unit. The config root, its history and its credentials are untouched, and
  the CLI says so out loud.
- **It does not supervise Windows.** The Windows install is a launcher that
  already supervises its backend inside WSL; `New` refuses with that as the
  remedy. `ErrUnsupported` is an ANSWER, not a machine failure, which is why
  the CLI exits 1 for it rather than 2.

## Verification

Tests must never run a real `systemctl` or `launchctl`, and never write outside
`t.TempDir()`. Unit files are golden-tested WHOLE rather than by substring: a
diff is a change to what gets installed on somebody's machine and should be
seen in full. Cover both managers on whatever host the suite runs on.
