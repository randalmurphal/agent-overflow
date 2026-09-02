// Package devscan finds the dev servers running on this machine and says
// which thread each belongs to.
//
// It is the discovery half of the dev-server port gateway
// (docs/specs/remote-access.md §7): the transport's preview listeners
// forward only to ports this package attributed to a thread, plus the
// ports the owner allowed by hand. Nothing here binds a port, serves a
// request or decides authorization — it reads the kernel's own view of
// what is listening, asks each candidate whether it answers like a web
// server, and reports.
//
// Three properties are worth knowing before editing:
//
//   - It reads, it never spawns anything of ours. On Linux (which is also
//     what the Windows deployment runs, since that ships as a WSL payload)
//     the whole enumeration is /proc. On macOS it shells out to `lsof` and
//     `ps`, the platform's own read-only tools, and every parser is a pure
//     function over their output so tests never execute either.
//   - A listener is only published if a bounded probe says it answers like
//     a page. A port being open proves nothing about it being openable,
//     and offering a "preview" of a language server's RPC socket would be
//     worse than offering nothing.
//   - Attribution is a fact about processes, not a guess from names. A
//     listener belongs to a thread when its ancestor chain reaches that
//     thread's provider session or terminal, or when it shares that
//     session's process group — which is what still catches a dev server
//     that daemonised and reparented to init.
package devscan
