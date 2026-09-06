# internal/sshsetup/

Desktop SSH setup/control is a bounded console session, not an agent scheduler.
Use OpenSSH configuration, agent authentication and known-host checks. Never
turn off host-key verification, inject passwords, or log pairing records.
Requests name a host/config alias and one binary path; remote commands are a
closed set with POSIX quoting (the remote backend is macOS, Linux or WSL).

The runner is injected and the OS runner refuses execution inside a test binary.
At most four setups exist; output lines and retained errors are bounded. Closing
or timing out a setup closes stdin and its local SSH process group, while the
separately installed remote service keeps running. Confirmation only forwards a
matching verification number. App methods are host-scoped and home-routed.
