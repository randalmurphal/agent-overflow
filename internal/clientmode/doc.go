// Package clientmode owns `agent-overflow --connect`: the remote-client
// mode in which the desktop binary boots no transport of its own and
// points its webview at a backend somewhere else.
//
// What it runs is a loopback stub that serves the embedded SPA bundle,
// answers the page's /bootstrap.json with a manifest naming its OWN
// origin, and carries the page's /ws to the upstream backend with the
// upstream credential attached in Go. The shell is served verbatim and
// the manifest carries no credential, which is what keeps the SPA
// byte-identical across embedded, --connect and remote-browser boots.
//
// Two credentials can authenticate that hop and the caller supplies
// exactly one. Config.Token is the upstream backend's launch
// credential, for an attach on the same machine — the WSL relay, an SSH
// tunnel, one process pointed at another. Config.Paired is a device
// session for a backend across a network: a durable rotating credential
// this installation obtained by pairing, presented with a proof minted
// per request over TLS pinned to the certificate the pairing payload
// named (internal/deviceclient). Neither ever reaches the page.
package clientmode
