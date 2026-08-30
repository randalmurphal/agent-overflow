package browser

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"agent-overflow/internal/atomicfile"

	"github.com/chromedp/cdproto/network"
	keyring "github.com/zalando/go-keyring"
)

const (
	stateDirectory = "browser-state"
	keyService     = "agent-overflow-browser"
	keyUser        = "site-data-v1"
	maxStateBytes  = 32 << 20
	maxStateFile   = 48 << 20
)

type storageState struct {
	Version      int                          `json:"version"`
	Workspace    string                       `json:"workspace"`
	Cookies      []*network.CookieParam       `json:"cookies,omitempty"`
	LocalStorage map[string]map[string]string `json:"localStorage,omitempty"`
}

type encryptedState struct {
	Version    int    `json:"version"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

type stateStore struct {
	root    string
	keyPath string
	service string
	user    string

	mu         sync.Mutex
	key        []byte
	keyFn      func() ([]byte, error)
	keyringGet func(service, user string) (string, error)
	keyringSet func(service, user, value string) error
}

func newStateStore(configDir string) *stateStore {
	store := &stateStore{
		root:       filepath.Join(configDir, stateDirectory),
		keyPath:    filepath.Join(configDir, "browser-state.key"),
		service:    keyService,
		user:       keyUser,
		keyringGet: keyring.Get,
		keyringSet: keyring.Set,
	}
	store.keyFn = store.loadOrCreateKey
	return store
}

func newTestStateStore(configDir string, key []byte) *stateStore {
	return &stateStore{
		root:    filepath.Join(configDir, stateDirectory),
		keyPath: filepath.Join(configDir, "browser-state.key"),
		keyFn: func() ([]byte, error) {
			return append([]byte(nil), key...), nil
		},
	}
}

func (s *stateStore) load(workspace string) (storageState, error) {
	path := s.path(workspace)
	if info, err := os.Stat(path); err == nil && info.Size() > maxStateFile {
		return storageState{}, fmt.Errorf("browser: persisted state exceeds %d bytes", maxStateFile)
	}
	var envelope encryptedState
	found, err := atomicfile.ReadJSON(path, &envelope)
	if err != nil {
		return storageState{}, fmt.Errorf("browser: read persisted state: %w", err)
	}
	if !found {
		return storageState{Version: 1, Workspace: workspace}, nil
	}
	if envelope.Version != 1 {
		return storageState{}, fmt.Errorf("browser: unsupported persisted state version %d", envelope.Version)
	}
	key, err := s.encryptionKey()
	if err != nil {
		return storageState{}, err
	}
	nonce, err := base64.StdEncoding.DecodeString(envelope.Nonce)
	if err != nil {
		return storageState{}, fmt.Errorf("browser: decode state nonce: %w", err)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(envelope.Ciphertext)
	if err != nil {
		return storageState{}, fmt.Errorf("browser: decode state ciphertext: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return storageState{}, fmt.Errorf("browser: create state cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return storageState{}, fmt.Errorf("browser: create state AEAD: %w", err)
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, []byte(workspace))
	if err != nil {
		return storageState{}, fmt.Errorf("browser: decrypt persisted state: %w", err)
	}
	var state storageState
	if err := json.Unmarshal(plain, &state); err != nil {
		return storageState{}, fmt.Errorf("browser: decode persisted state: %w", err)
	}
	if state.Workspace != workspace {
		return storageState{}, fmt.Errorf("browser: persisted state workspace mismatch")
	}
	if state.LocalStorage == nil {
		state.LocalStorage = make(map[string]map[string]string)
	}
	return state, nil
}

func (s *stateStore) save(state storageState) error {
	state.Version = 1
	key, err := s.encryptionKey()
	if err != nil {
		return err
	}
	plain, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("browser: encode persisted state: %w", err)
	}
	if len(plain) > maxStateBytes {
		return fmt.Errorf("browser: persisted state exceeds %d bytes", maxStateBytes)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return fmt.Errorf("browser: create state cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("browser: create state AEAD: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("browser: generate state nonce: %w", err)
	}
	envelope := encryptedState{
		Version:    1,
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(gcm.Seal(nil, nonce, plain, []byte(state.Workspace))),
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return fmt.Errorf("browser: create state directory: %w", err)
	}
	if err := atomicfile.WriteJSON(s.path(state.Workspace), envelope); err != nil {
		return fmt.Errorf("browser: persist state: %w", err)
	}
	return nil
}

func (s *stateStore) clear() error {
	if err := os.RemoveAll(s.root); err != nil {
		return fmt.Errorf("browser: clear persisted state: %w", err)
	}
	return nil
}

func (s *stateStore) path(workspace string) string {
	digest := sha256.Sum256([]byte(workspace))
	return filepath.Join(s.root, fmt.Sprintf("%x.json", digest[:16]))
}

func (s *stateStore) encryptionKey() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.key) == 32 {
		return append([]byte(nil), s.key...), nil
	}
	key, err := s.keyFn()
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("browser: invalid state encryption key length")
	}
	s.key = append([]byte(nil), key...)
	return key, nil
}

func (s *stateStore) loadOrCreateKey() ([]byte, error) {
	// A prior run may have fallen back because the desktop keyring was
	// unavailable. Keep using that key if present; switching stores later
	// would make every existing encrypted workspace unreadable.
	if _, err := os.Stat(s.keyPath); err == nil {
		return s.loadOrCreateFallbackKey()
	}
	encoded, err := s.keyringGet(s.service, s.user)
	if err == nil {
		key, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		if decodeErr != nil || len(key) != 32 {
			return nil, fmt.Errorf("browser: invalid OS keyring state key")
		}
		return key, nil
	}
	if !errors.Is(err, keyring.ErrNotFound) {
		return s.loadOrCreateFallbackKey()
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("browser: generate state key: %w", err)
	}
	if err := s.keyringSet(s.service, s.user, base64.StdEncoding.EncodeToString(key)); err != nil {
		return s.loadOrCreateFallbackKey()
	}
	return key, nil
}

func (s *stateStore) loadOrCreateFallbackKey() ([]byte, error) {
	path := s.keyPath
	data, err := os.ReadFile(path)
	if err == nil {
		key, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
		if decodeErr != nil || len(key) != 32 {
			return nil, fmt.Errorf("browser: invalid fallback state key")
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("browser: read fallback state key: %w", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("browser: generate fallback state key: %w", err)
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return nil, fmt.Errorf("browser: create state directory: %w", err)
	}
	if err := atomicfile.Write(path, []byte(base64.StdEncoding.EncodeToString(key))); err != nil {
		return nil, fmt.Errorf("browser: persist fallback state key: %w", err)
	}
	return key, nil
}
