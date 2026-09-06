package filepreview

import (
	"errors"
	"os"
	"sync"

	"agent-overflow/internal/transport"
)

const maxDirectories = 16

type key struct {
	directory string
	local     bool
}

type entry struct {
	files
	gateway *transport.PreviewGateway
	port    int
	used    uint64
}

func (e *entry) close() {
	e.gateway.Close()
	_ = e.root.Close()
}

// Manager holds at most sixteen independently authorized directories. The
// least recently opened expires when another is opened. Each new entry gets a
// fresh grant book, even if its listener happens to reuse an earlier port.
type Manager struct {
	mu      sync.Mutex
	config  transport.PreviewGatewayConfig
	entries map[key]*entry
	serial  uint64
	closed  bool
}

func New(cfg transport.PreviewGatewayConfig) *Manager {
	return &Manager{config: cfg, entries: make(map[key]*entry)}
}

// Open returns a single-use URL. Local must come from the transport's host
// presence proof, never a wire argument. Even a local paired caller retains
// its principal so revoking that session ends its previews.
func (m *Manager) Open(file, workspace, principal string, local bool) (string, error) {
	directory, target, err := resolve(file, workspace)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", errors.New("file previews stopped; reconnect to this computer")
	}
	k := key{directory, local}
	e := m.entries[k]
	if e == nil {
		root, err := os.OpenRoot(directory)
		if err != nil {
			return "", err
		}
		e = &entry{files: files{root}}
		if err := e.check(target); err != nil {
			_ = root.Close()
			return "", err
		}
		e.gateway, e.port, err = transport.NewContentPreview(m.config, e.files, k.local)
		if err != nil {
			_ = root.Close()
			return "", err
		}
		if len(m.entries) >= maxDirectories {
			var oldest key
			var victim *entry
			for candidate, existing := range m.entries {
				if victim == nil || existing.used < victim.used {
					oldest, victim = candidate, existing
				}
			}
			delete(m.entries, oldest)
			victim.close()
		}
		m.entries[k] = e
	} else if err := e.check(target); err != nil {
		return "", err
	}
	m.serial++
	e.used = m.serial
	return e.gateway.MintURL(principal, e.port, previewPath(target))
}

func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	entries := m.entries
	m.entries = nil
	m.mu.Unlock()
	for _, e := range entries {
		e.close()
	}
}

// CloseNetwork ends remote previews when the host changes its sharing policy.
// Local pages remain usable; a later remote open must bind from current sources.
func (m *Manager) CloseNetwork() {
	m.mu.Lock()
	var retired []*entry
	for k, e := range m.entries {
		if !k.local {
			delete(m.entries, k)
			retired = append(retired, e)
		}
	}
	m.mu.Unlock()
	for _, e := range retired {
		e.close()
	}
}
