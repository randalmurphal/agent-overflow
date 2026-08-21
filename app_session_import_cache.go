package main

import (
	"context"
	"slices"
	"sync"
	"time"

	"agent-overflow/internal/sessionimport"
)

// sessionImportScanTTL is how long one provider-home scan is reused.
//
// A scan walks every session file in both provider homes — a real Claude home
// is gigabytes across a thousand-plus transcripts — and the import modal
// re-lists whenever it is reopened, so an uncached list would re-walk the disk
// each time. Sixty seconds is short enough that a session finished while the
// modal is open shows up on the next natural re-list, and the Refresh button
// bypasses the cache outright.
const sessionImportScanTTL = time.Minute

// sessionImportScan is one cached scan plus the moment it was taken. The
// timestamp travels with the rows so a cache hit reports when the disk was
// actually read, not when the RPC was answered.
type sessionImportScan struct {
	result    sessionimport.ScanResult
	scannedAt int64
}

// sessionImportScanCache holds THE scan — one entry, not a keyed map.
//
// The scan is unfiltered by construction: `ImportScanRequest` carries no
// provider or workspace filter, so there is exactly one question to answer and
// a keyed cache would only be a way to serve one filter's rows as another's.
// It also makes id resolution a map lookup instead of a walk of every cached
// entry's rows.
//
// Failures are NOT cached, unlike the Codex catalog's: Scan only errors when
// the store read behind the dedup set fails, which is neither expected nor
// self-healing on a timer, and serving a cached failure would make the modal's
// Retry button do nothing.
type sessionImportScanCache struct {
	mu       sync.Mutex
	ttl      time.Duration
	now      func() time.Time
	scan     func(context.Context) (sessionimport.ScanResult, error)
	entry    *sessionImportScanEntry
	inflight *sessionImportScanLoad
}

// sessionImportScanEntry is the cached scan, its expiry, and the id index
// Lookup answers from.
type sessionImportScanEntry struct {
	scan      sessionImportScan
	byID      map[string]sessionimport.Row
	expiresAt time.Time
}

type sessionImportScanLoad struct {
	done chan struct{}
	scan sessionImportScan
	err  error
}

func newSessionImportScanCache(
	ttl time.Duration,
	now func() time.Time,
	scan func(context.Context) (sessionimport.ScanResult, error),
) *sessionImportScanCache {
	if ttl <= 0 {
		ttl = sessionImportScanTTL
	}
	if now == nil {
		now = time.Now
	}
	return &sessionImportScanCache{ttl: ttl, now: now, scan: scan}
}

// Get returns the current scan, walking the provider homes at most once
// across concurrent callers.
//
// force skips the cached entry but still joins an in-flight scan: two Refresh
// clicks a millisecond apart are one disk walk, and the second caller gets
// fresh rows either way.
//
// The result is a deep copy. The cache keeps its slices for the whole TTL and
// hands them to every caller in between; returning them directly would make
// one caller's append or in-place edit another caller's listing (the same rule
// internal/codexmodels follows).
func (c *sessionImportScanCache) Get(ctx context.Context, force bool) (sessionImportScan, error) {
	c.mu.Lock()
	if !force && c.fresh() {
		entry := c.entry
		c.mu.Unlock()
		return cloneSessionImportScan(entry.scan), nil
	}
	if existing := c.inflight; existing != nil {
		done := existing.done
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return sessionImportScan{}, ctx.Err()
		case <-done:
			if existing.err != nil {
				return sessionImportScan{}, existing.err
			}
			return cloneSessionImportScan(existing.scan), nil
		}
	}
	load := &sessionImportScanLoad{done: make(chan struct{})}
	c.inflight = load
	c.mu.Unlock()

	result, err := c.scan(ctx)
	scan := sessionImportScan{result: result, scannedAt: c.now().UnixMilli()}

	c.mu.Lock()
	load.scan = scan
	load.err = err
	c.inflight = nil
	if err == nil {
		c.entry = &sessionImportScanEntry{
			scan:      scan,
			byID:      indexScanRows(result.Rows),
			expiresAt: c.now().Add(c.ttl),
		}
	}
	close(load.done)
	c.mu.Unlock()

	if err != nil {
		return sessionImportScan{}, err
	}
	return cloneSessionImportScan(scan), nil
}

// fresh reports whether the cached entry may still be served. Callers hold mu.
func (c *sessionImportScanCache) fresh() bool {
	return c.entry != nil && c.now().Before(c.entry.expiresAt)
}

// Lookup resolves a scanned row by its opaque id.
//
// It HONORS the TTL: an entry past its expiry is not an answer, because the
// row carries a file path and a size the disk may since have changed, and
// importing from a stale row is exactly the case where that matters. A miss —
// expired or never scanned — is the caller's cue to force a rescan, which
// re-mints the same ids (a row id is (provider, session id) and depends on
// nothing about when the scan ran). Lookup itself never scans: whether a miss
// is worth re-walking the disk for is the caller's decision.
func (c *sessionImportScanCache) Lookup(id string) (sessionimport.Row, bool) {
	if id == "" {
		return sessionimport.Row{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fresh() {
		return sessionimport.Row{}, false
	}
	row, ok := c.entry.byID[id]
	if !ok {
		return sessionimport.Row{}, false
	}
	return cloneScanRow(row), true
}

// Reset drops the cached scan. Used after an import run so the next list does
// not offer sessions that now have threads.
func (c *sessionImportScanCache) Reset() {
	c.mu.Lock()
	c.entry = nil
	c.mu.Unlock()
}

func indexScanRows(rows []sessionimport.Row) map[string]sessionimport.Row {
	byID := make(map[string]sessionimport.Row, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	return byID
}

func cloneSessionImportScan(scan sessionImportScan) sessionImportScan {
	scan.result.Providers = slices.Clone(scan.result.Providers)
	rows := slices.Clone(scan.result.Rows)
	for i := range rows {
		rows[i].Warnings = slices.Clone(rows[i].Warnings)
		rows[i].ImportedFrom = rows[i].ImportedFrom.Clone()
	}
	scan.result.Rows = rows
	return scan
}

func cloneScanRow(row sessionimport.Row) sessionimport.Row {
	row.Warnings = slices.Clone(row.Warnings)
	row.ImportedFrom = row.ImportedFrom.Clone()
	return row
}
