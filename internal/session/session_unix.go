//go:build !windows

package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/creack/pty"
)

type Output struct {
	SessionID string
	Data      []byte
	Alive     bool
	ExitCode  *int
}

type Session struct {
	ID      string
	Shell   string
	cmd     *exec.Cmd
	ptyFile *os.File
	stdin   io.WriteCloser
	out     chan Output
	done    chan struct{}
	once    sync.Once
}

type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

func NewManager() *Manager { return &Manager{sessions: map[string]*Session{}} }

func (m *Manager) Open(ctx context.Context, id, shell string) (*Session, bool, error) {
	if shell == "" {
		shell = "/bin/bash"
	}
	if id == "" {
		return nil, false, errors.New("empty session id")
	}

	// Try PTY first (full-duplex interactive)
	s, interactive, err := m.openPTY(ctx, id, shell)
	if err != nil {
		// Auto-fallback to pipe mode (non-interactive)
		log.Default().Printf("PTY unavailable for %s: %v — falling back to pipe mode", id, err)
		return m.openPipe(ctx, id, shell)
	}
	return s, interactive, nil
}

func (m *Manager) openPTY(ctx context.Context, id, shell string) (*Session, bool, error) {
	cmd := exec.CommandContext(ctx, shell)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, false, fmt.Errorf("start pty: %w", err)
	}
	s := &Session{ID: id, Shell: shell, cmd: cmd, ptyFile: ptmx, out: make(chan Output, 128), done: make(chan struct{})}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	go s.readLoop()
	go s.waitLoop(m)
	return s, true, nil
}

func (m *Manager) openPipe(ctx context.Context, id, shell string) (*Session, bool, error) {
	cmd := exec.CommandContext(ctx, shell)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	stdin, _ := cmd.StdinPipe()
	if err := cmd.Start(); err != nil {
		return nil, false, fmt.Errorf("start pipe: %w", err)
	}
	s := &Session{ID: id, Shell: shell, cmd: cmd, ptyFile: nil, stdin: stdin, out: make(chan Output, 128), done: make(chan struct{})}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				s.out <- Output{SessionID: id, Data: append([]byte(nil), buf[:n]...), Alive: true}
			}
			if err != nil {
				break
			}
		}
	}()
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				s.out <- Output{SessionID: id, Data: append([]byte(nil), buf[:n]...), Alive: true}
			}
			if err != nil {
				break
			}
		}
	}()
	go func() {
		err := cmd.Wait()
		code := 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				code = -1
			}
		}
		s.out <- Output{SessionID: id, Alive: false, ExitCode: &code}
		close(s.done)
		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
		close(s.out)
	}()
	return s, false, nil
}

func (s *Session) readLoop() {
	buf := make([]byte, 8192)
	for {
		n, err := s.ptyFile.Read(buf)
		if n > 0 {
			s.out <- Output{SessionID: s.ID, Data: append([]byte(nil), buf[:n]...), Alive: true}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) { /* waitLoop will send final status */
			}
			return
		}
	}
}

func (s *Session) waitLoop(m *Manager) {
	err := s.cmd.Wait()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	s.out <- Output{SessionID: s.ID, Alive: false, ExitCode: &code}
	close(s.done)
	m.mu.Lock()
	delete(m.sessions, s.ID)
	m.mu.Unlock()
	_ = s.ptyFile.Close()
	close(s.out)
}

func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	return s, ok
}

func (m *Manager) Write(id string, text string, appendNewline bool) error {
	s, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("session %s not found", id)
	}
	if appendNewline {
		text += "\n"
	}
	if s.ptyFile != nil {
		_, err := s.ptyFile.Write([]byte(text))
		return err
	}
	if s.stdin != nil {
		_, err := io.WriteString(s.stdin, text)
		return err
	}
	return fmt.Errorf("session %s has no writable handle", id)
}

func (m *Manager) Interrupt(id string) error {
	s, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("session %s not found", id)
	}
	if s.cmd.Process == nil {
		return nil
	}
	return s.cmd.Process.Signal(syscall.SIGINT)
}

func (m *Manager) Close(id string) error {
	s, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("session %s not found", id)
	}
	s.once.Do(func() {
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		if s.ptyFile != nil {
			_ = s.ptyFile.Close()
		}
		if s.stdin != nil {
			_ = s.stdin.Close()
		}
	})
	return nil
}

func (s *Session) Output() <-chan Output { return s.out }
