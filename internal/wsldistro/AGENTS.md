# WSL selection state

This package owns the cross-process schema and path resolution for the Windows
launcher's wsl.json. The launcher records successful boots; the WSL-side
Settings flow updates the saved distro while preserving payload fields.

InstalledBinPath, InstalledSHA256, and InstalledDistro form one payload record.
Only the exact embedded-byte digest identifies an installation. Invalidate the
record before replacement and publish the new record only after successful
boot. A transient launcher override may update payload identity but not the
saved default. Isolated profiles never write this shared file.

Save uses private permissions and atomic temp-file, sync, and rename semantics.
Both Windows and WSL processes may write through the NTFS automount, so readers
must never observe a partial JSON document. Preserve independently owned
`Config` fields during load-mutate-save flows.

On WSL, WSLConfigDir resolves AGENT_OVERFLOW_WIN_APPDATA after WSLENV path
translation. Reject relative paths, traversal segments, nonexistent paths, and
non-directories. On Windows, resolve the per-user AppData directory with the
existing home fallback.

Process spawning belongs in internal/wsllauncher. Picker UI belongs in
cmd/agent-overflow-windows.
