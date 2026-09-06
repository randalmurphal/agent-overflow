package sshsetup

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/procutil"
)

type Status struct {
	ID                 string `json:"id"`
	State              string `json:"state"`
	Target             string `json:"target"`
	Invitation         string `json:"invitation,omitempty"`
	VerificationNumber string `json:"verificationNumber,omitempty"`
	Error              string `json:"error,omitempty"`
}
type session struct {
	status  Status
	cancel  context.CancelFunc
	input   *io.PipeWriter
	touched time.Time
}
type Manager struct {
	mu       sync.Mutex
	runner   Runner
	sessions map[string]*session
	closed   bool
}

func New(runner Runner) *Manager {
	return &Manager{runner: runner, sessions: make(map[string]*session)}
}

// Start wakes an already-installed service. It does not enroll a new device
// or keep an SSH session open once the service manager accepts the request.
func (m *Manager) Start(ctx context.Context, request Request) error {
	if err := validate(request); err != nil {
		return err
	}
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed || m.runner == nil {
		return errors.New("SSH setup is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	diagnostic := procutil.NewTailBuffer(4096)
	if err := m.runner.Run(ctx, request, "start", nil, io.Discard, diagnostic); err != nil {
		if text := strings.TrimSpace(diagnostic.String()); text != "" {
			return fmt.Errorf("start remote service: %s", text)
		}
		return fmt.Errorf("start remote service: %w", err)
	}
	return nil
}
func (m *Manager) Begin(ctx context.Context, request Request) (Status, error) {
	if err := validate(request); err != nil {
		return Status{}, err
	}
	if m.runner == nil {
		return Status{}, errors.New("SSH is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Status{}, errors.New("SSH setup is closed")
	}
	var oldest string
	for id, held := range m.sessions {
		if held.status.State == "connected" || held.status.State == "error" || held.status.State == "canceled" {
			if time.Since(held.touched) > 15*time.Minute {
				delete(m.sessions, id)
			} else if oldest == "" || held.touched.Before(m.sessions[oldest].touched) {
				oldest = id
			}
		}
	}
	if len(m.sessions) >= 4 && oldest != "" {
		delete(m.sessions, oldest)
	}
	if len(m.sessions) >= 4 {
		return Status{}, errors.New("finish an existing SSH setup first")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	input, writer := io.Pipe()
	held := &session{status: Status{ID: rand.Text(), State: "connecting", Target: request.Target}, cancel: cancel, input: writer, touched: time.Now()}
	m.sessions[held.status.ID] = held
	go m.run(ctx, request, held, input)
	return held.status, nil
}
func (m *Manager) Get(id string) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	held := m.sessions[id]
	if held == nil {
		return Status{}, errors.New("SSH setup no longer exists")
	}
	return held.status, nil
}
func (m *Manager) Confirm(ctx context.Context, id, number string) error {
	m.mu.Lock()
	held := m.sessions[id]
	if held == nil || held.status.State != "verification" {
		m.mu.Unlock()
		return errors.New("SSH setup is not waiting for confirmation")
	}
	if number != held.status.VerificationNumber {
		m.mu.Unlock()
		return errors.New("verification numbers do not match")
	}
	held.status.State = "confirming"
	input := held.input
	m.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := io.WriteString(input, number+"\n")
		done <- err
	}()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		m.Cancel(id)
		return ctx.Err()
	}
}
func (m *Manager) Cancel(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if held := m.sessions[id]; held != nil {
		held.status = Status{ID: id, State: "canceled", Target: held.status.Target}
		held.cancel()
		_ = held.input.Close()
	}
}
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for _, held := range m.sessions {
		held.cancel()
		_ = held.input.Close()
	}
	m.sessions = make(map[string]*session)
}
func (m *Manager) update(held *session, change func(*Status)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[held.status.ID] != held || held.status.State == "canceled" {
		return
	}
	change(&held.status)
	held.touched = time.Now()
}
func (m *Manager) run(ctx context.Context, request Request, held *session, input *io.PipeReader) {
	defer held.cancel()
	defer input.Close()
	defer held.input.Close()
	diagnostic := procutil.NewTailBuffer(4096)
	fail := func(err error) {
		m.update(held, func(status *Status) {
			if status.State == "connected" {
				return
			}
			status.State = "error"
			status.Invitation = ""
			status.VerificationNumber = ""
			status.Error = "SSH setup failed. " + strings.TrimSpace(diagnostic.String())
			if diagnostic.String() == "" {
				status.Error = fmt.Sprintf("SSH setup failed: %v", err)
			}
		})
	}
	if request.StartService {
		if err := m.runner.Run(ctx, request, "start", nil, io.Discard, diagnostic); err != nil {
			fail(err)
			return
		}
		diagnostic = procutil.NewTailBuffer(4096)
	}
	output, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := m.runner.Run(ctx, request, "pair", input, writer, diagnostic)
		_ = writer.CloseWithError(err)
		done <- err
	}()
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), 16*1024)
	for scanner.Scan() {
		var record struct {
			Type string `json:"type"`
			Data struct {
				URL                string `json:"url"`
				VerificationNumber string `json:"verificationNumber"`
			} `json:"data"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			fail(errors.New("the remote command did not return an Agent Overflow pairing record"))
			held.cancel()
			break
		}
		valid := true
		m.update(held, func(status *Status) {
			switch record.Type {
			case "invitation":
				if status.State != "connecting" || record.Data.URL == "" {
					valid = false
					return
				}
				status.State = "invitation"
				status.Invitation = record.Data.URL
			case "verification":
				if status.State != "invitation" || !validNumber(record.Data.VerificationNumber) {
					valid = false
					return
				}
				status.State = "verification"
				status.VerificationNumber = record.Data.VerificationNumber
			case "paired":
				if status.State != "confirming" {
					valid = false
					return
				}
				status.State = "connected"
				status.Invitation = ""
				status.VerificationNumber = ""
			default:
				valid = false
			}
		})
		if !valid {
			fail(errors.New("unexpected SSH pairing state"))
			held.cancel()
			break
		}
	}
	_ = output.Close()
	if err := scanner.Err(); err != nil {
		fail(err)
	}
	if err := <-done; err != nil {
		fail(err)
	}
	m.update(held, func(status *Status) {
		if status.State != "connected" && status.State != "error" {
			status.State = "error"
			status.Error = "SSH closed before pairing completed."
			status.Invitation = ""
			status.VerificationNumber = ""
		}
	})
}

func validNumber(number string) bool {
	if len(number) != 6 {
		return false
	}
	for _, ch := range number {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}
