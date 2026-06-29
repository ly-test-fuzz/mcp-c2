//go:build windows

package session

import (
	"context"
	"io"
	"os/exec"
	"sync"
	"syscall"
)

type Output struct {
	SessionID string
	Data      []byte
	Alive     bool
	ExitCode  *int
}

type Session struct {
	ID    string
	Shell string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   chan Output
	done  chan struct{}
	once  sync.Once
}

type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

func NewManager() *Manager { return &Manager{sessions: map[string]*Session{}} }

func (m *Manager) Open(ctx context.Context, id, shell string) (*Session, bool, error) {
	if shell == "" {
		shell = "cmd"
	}
	cmd := exec.CommandContext(ctx, shell)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}
	s := &Session{ID: id, Shell: shell, cmd: cmd, stdin: stdin, out: make(chan Output, 128), done: make(chan struct{})}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	readFrom := func(r io.Reader) {
		buf := make([]byte, 8192)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				s.out <- Output{SessionID: id, Data: append([]byte(nil), buf[:n]...), Alive: true}
			}
			if err != nil {
				return
			}
		}
	}
	go readFrom(stdout)
	go readFrom(stderr)
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
		_ = stdin.Close()
		close(s.out)
	}()
	return s, false, nil
}

func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	return s, ok
}

func (m *Manager) Write(id string, text string, appendNewline bool) error {
	s, ok := m.Get(id)
	if !ok || s.stdin == nil {
		return nil
	}
	if appendNewline {
		text += "\r\n"
	}
	_, _ = io.WriteString(s.stdin, text)
	return nil
}

func (m *Manager) Interrupt(id string) error {
	s, ok := m.Get(id)
	if !ok || s.cmd.Process == nil {
		return nil
	}
	return s.cmd.Process.Signal(syscall.SIGINT)
}

func (m *Manager) Close(id string) error {
	s, ok := m.Get(id)
	if !ok {
		return nil
	}
	s.once.Do(func() {
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
	})
	return nil
}

func (s *Session) Output() <-chan Output { return s.out }
