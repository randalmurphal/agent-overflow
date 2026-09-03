package app

import (
	"context"

	"agent-overflow/internal/devserverprobe"
)

// ProbeDevServerURL reports whether something is currently listening on
// the loopback URL a command row's meta announced, gating the
// DevServerChip (rationale: internal/devserverprobe doc.go). Loopback-
// only on the wire: the answer is a port-scan oracle for the backend
// host, and a remote viewer's localhost is not this machine anyway.
//
//ao:scope host
//ao:route home
func (a *App) ProbeDevServerURL(rawURL string) (bool, error) {
	return a.devServerProbe().Live(context.Background(), rawURL)
}

func (a *App) devServerProbe() *devserverprobe.Prober {
	a.devServerProbeOnce.Do(func() {
		a.devServerProber = devserverprobe.New()
	})
	return a.devServerProber
}
