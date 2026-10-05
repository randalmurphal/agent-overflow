package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/netip"
	"time"

	"agent-overflow/internal/buildvariant"
)

// refuseRemoteBoot refuses, in a build without remote access
// (internal/buildvariant), every boot whose purpose is remote access: the
// `serve` and `supervise` backends, a client attached to another backend,
// and a bind beyond loopback.
func refuseRemoteBoot(serve, supervise bool, flags cliFlags) error {
	if buildvariant.RemoteAccess {
		return nil
	}
	var what string
	switch {
	case serve:
		what = serveVerb
	case supervise:
		what = superviseVerb
	case flags.connect != "":
		what = "--connect"
	case flags.frontend:
		what = "--frontend"
	case flags.listenAddr != "":
		host, _, err := splitListenAddr(flags.listenAddr)
		if err != nil {
			return err
		}
		if addr, err := netip.ParseAddr(host); err == nil && addr.IsLoopback() {
			return nil
		}
		what = "--listen " + flags.listenAddr
	default:
		return nil
	}
	return fmt.Errorf("%s: %w", what, buildvariant.ErrRemoteAccessUnavailable)
}

// remoteAccessOffMeta tells the SPA, before it renders anything, that this
// build has no remote access (frontend/src/lib/transport/buildVariant.ts).
// A build with remote access serves index.html unchanged.
const remoteAccessOffMeta = `<meta name="ao-remote-access" content="off" />`

// withVariantIndex serves the SPA entry shell carrying remoteAccessOffMeta
// in a build without remote access, and returns next unchanged otherwise.
// Only "/" serves the shell: the file server redirects /index.html there.
func withVariantIndex(spa fs.FS, next http.Handler) (http.Handler, error) {
	if buildvariant.RemoteAccess {
		return next, nil
	}
	raw, err := fs.ReadFile(spa, "index.html")
	if err != nil {
		return nil, fmt.Errorf("read the SPA entry shell: %w", err)
	}
	head := []byte("<head>")
	at := bytes.Index(raw, head)
	if at < 0 {
		return nil, errors.New("the SPA entry shell has no <head> to carry the build variant")
	}
	at += len(head)
	index := make([]byte, 0, len(raw)+len(remoteAccessOffMeta))
	index = append(index, raw[:at]...)
	index = append(index, remoteAccessOffMeta...)
	index = append(index, raw[at:]...)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			next.ServeHTTP(w, r)
			return
		}
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(index))
	}), nil
}
