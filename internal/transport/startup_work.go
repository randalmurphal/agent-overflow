package transport

import "time"

// processWork is what this process has done since it started: CPU time
// and bytes moved to or from storage. ioSource names the counter the
// bytes came from; counts from different counters are not compared.
type processWork struct {
	cpu      time.Duration
	io       int64
	ioSource string
}

// The least work one heartbeat must see to count as progress, as rates
// over the interval: a twentieth of one CPU (50 ms per 1 s tick) and
// 64 KiB/s of storage I/O. An idle backend answering readiness polls and
// writing its log stays under both; a sort, a scan, a rebuild or a cold
// read is far over them. A step blocked on a lock does neither.
const (
	startupCPUShare    = 20
	startupIOPerSecond = 64 << 10
)

// workProgressed reports whether the work between two samples one
// interval apart counts as progress.
func workProgressed(prev, cur processWork, interval time.Duration) bool {
	if cur.cpu-prev.cpu >= interval/startupCPUShare {
		return true
	}
	minIO := int64(startupIOPerSecond * interval.Seconds())
	return cur.ioSource == prev.ioSource && cur.io-prev.io >= minIO
}
