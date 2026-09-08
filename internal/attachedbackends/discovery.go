package attachedbackends

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/nearby"
	"agent-overflow/internal/pairbootstrap"
)

type DiscoveredComputer struct {
	BackendID string `json:"backendId"`
	Name      string `json:"name"`
	Address   string `json:"address"`
	Network   string `json:"network"`
}

// Discover checks public hints only. No credential, certificate pin or profile
// is persisted until the separately approved pairing ceremony completes.
//
// Overlapping calls share one scan only when they brought the same hints:
// a tailnet scan can add or drop a candidate between two requests, and the
// later caller must not be answered from the earlier caller's hints.
func (m *Manager) Discover(ctx context.Context, extra []DiscoveredComputer) ([]DiscoveredComputer, error) {
	result := m.discovery.DoChan(discoveryKey(extra), func() (any, error) { return m.discover(context.WithoutCancel(ctx), extra) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-result:
		if r.Err != nil {
			return nil, r.Err
		}
		return r.Val.([]DiscoveredComputer), nil
	}
}

// discoveryKey names a hint set by what the probe reads off it. The name is
// left out: the probe replaces it with what the computer calls itself.
func discoveryKey(extra []DiscoveredComputer) string {
	var key strings.Builder
	for _, hint := range extra {
		key.WriteString(hint.BackendID)
		key.WriteByte(0)
		key.WriteString(hint.Address)
		key.WriteByte(0)
		key.WriteString(hint.Network)
		key.WriteByte(0)
	}
	return key.String()
}

func (m *Manager) discover(ctx context.Context, extra []DiscoveredComputer) ([]DiscoveredComputer, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	lan, err := nearby.Discover(ctx)
	candidates := make([]DiscoveredComputer, 0, len(lan)+len(extra))
	for _, h := range lan {
		candidates = append(candidates, DiscoveredComputer{BackendID: h.BackendID, Name: h.Name, Address: h.Address, Network: "lan"})
	}
	candidates = append(candidates, extra...)
	if len(candidates) == 0 && err != nil {
		return nil, err
	}
	return m.probeCandidates(ctx, candidates)
}

// probeCandidates shares validation and identity aggregation for LAN and
// tailnet hints without coupling the probe stage to a multicast scan.
func (m *Manager) probeCandidates(ctx context.Context, candidates []DiscoveredComputer) ([]DiscoveredComputer, error) {
	if len(candidates) == 0 {
		return []DiscoveredComputer{}, nil
	}
	if len(candidates) > 128 {
		candidates = candidates[:128]
	}
	known := make(map[string]bool)
	for _, h := range m.Attached() {
		known[h.BackendID] = true
	}
	if m.selfID != nil {
		known[m.selfID()] = true
	}
	client := pairbootstrap.NewHTTPClient(m.dial)
	defer client.CloseIdleConnections()
	var mu sync.Mutex
	results := make(map[string]DiscoveredComputer)
	jobs := make(chan DiscoveredComputer)
	var wg sync.WaitGroup
	for range min(16, len(candidates)) {
		wg.Go(func() {
			for hint := range jobs {
				if hint.BackendID != "" && known[hint.BackendID] {
					continue
				}
				found, ok := probeComputer(ctx, client, hint)
				if !ok || known[found.BackendID] {
					continue
				}
				mu.Lock()
				old, exists := results[found.BackendID]
				if !exists || (found.Network == "lan" && old.Network != "lan") || (found.Network == old.Network && found.Address < old.Address) {
					results[found.BackendID] = found
				}
				mu.Unlock()
			}
		})
	}
	for _, hint := range candidates {
		select {
		case jobs <- hint:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()
	out := make([]DiscoveredComputer, 0, len(results))
	for _, h := range results {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].BackendID < out[j].BackendID
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

type computerInfo struct {
	BackendID string `json:"backendId"`
	Name      string `json:"name"`
	Open      bool   `json:"open"`
}

func inspectComputer(ctx context.Context, client *http.Client, address string) (computerInfo, error) {
	endpoint, err := pairbootstrap.NormalizeAddress(address)
	if err != nil {
		return computerInfo{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+pairbootstrap.Path, bytes.NewBufferString(`{"op":"info"}`))
	if err != nil {
		return computerInfo{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return computerInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return computerInfo{}, pairbootstrap.ErrClosed
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	var info computerInfo
	if err != nil || len(data) > 4096 || json.Unmarshal(data, &info) != nil || !entityid.Valid(info.BackendID) || info.Name == "" || len(info.Name) > 256 {
		return info, pairbootstrap.ErrInvalid
	}
	return info, nil
}

func probeComputer(ctx context.Context, client *http.Client, hint DiscoveredComputer) (DiscoveredComputer, bool) {
	info, err := inspectComputer(ctx, client, hint.Address)
	if err != nil || !info.Open || (hint.BackendID != "" && hint.BackendID != info.BackendID) {
		return hint, false
	}
	hint.BackendID, hint.Name = info.BackendID, info.Name
	return hint, true
}

// SetNetwork is boot wiring, before any clients or pairing attempts exist.
func (m *Manager) SetNetwork(selfID func() string, dial deviceclient.DialContextFunc) {
	m.selfID, m.dial = selfID, dial
}
