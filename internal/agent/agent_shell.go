package agent

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"

	"debugmcp/internal/wire"
)

// shellSession is one interactive PTY shell on the agent. A reader goroutine drains
// the PTY master into buf; shell_read drains buf. Completion is "heuristic" while
// the shell runs and "authoritative" once the child exits (PTY EOF + reaped exit).
type shellSession struct {
	sid      string
	shell    string
	mu       sync.Mutex
	buf      []byte
	done     bool
	exitCode int32
	signal   string
	created  time.Time
	lastIO   time.Time
	p        ptyHandle
}

// ptyHandle is the minimal PTY surface the agent uses (indirection so tests can
// substitute a fake). The real implementation is *pty.PTY.
type ptyHandle interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close() error
	PID() int
	Wait() (exitCode int, signal string, err error)
}

func (a *Agent) allocSID() string {
	a.nextSid++
	return fmt.Sprintf("s-%d", a.nextSid)
}

func (a *Agent) getSession(sid string) *shellSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.shells[sid]
}

func (a *Agent) shellOpen(env *wire.Envelope, req *wire.ShellOpen) {
	a.mu.Lock()
	if len(a.shells) >= a.opts.Cap {
		used := len(a.shells)
		cap := a.opts.Cap
		a.mu.Unlock()
		a.reply(env.Seq, env.Sid, wire.MsgShellOpenResult, &wire.ShellOpenResult{
			Busy: &wire.BusyInfo{Busy: true, Used: used, Cap: cap},
		})
		return
	}
	sid := a.allocSID()
	s := &shellSession{sid: sid, created: time.Now(), lastIO: time.Now()}
	a.shells[sid] = s
	a.mu.Unlock()

	shell := req.Shell
	if shell == "" {
		shell = loginShell()
	}
	p, err := openPTY(shell, req.Cols, req.Rows)
	if err != nil {
		a.mu.Lock()
		delete(a.shells, sid)
		a.mu.Unlock()
		a.replyErr(env.Seq, env.Sid, "shell_start", err.Error())
		return
	}
	s.shell = shell
	s.p = p
	go drainPTY(s)

	a.reply(env.Seq, env.Sid, wire.MsgShellOpenResult, &wire.ShellOpenResult{Sid: sid, Shell: shell})
}

func drainPTY(s *shellSession) {
	b := make([]byte, 4096)
	for {
		n, err := s.p.Read(b)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, b[:n])
			s.mu.Lock()
			s.buf = append(s.buf, chunk...)
			s.lastIO = time.Now()
			s.mu.Unlock()
		}
		if err != nil {
			code, sig, _ := s.p.Wait()
			s.mu.Lock()
			s.done = true
			s.exitCode = int32(code)
			s.signal = sig
			s.mu.Unlock()
			return
		}
	}
}

func (a *Agent) shellSend(env *wire.Envelope, req *wire.ShellSend) {
	s := a.getSession(env.Sid)
	if s == nil {
		a.replyErr(env.Seq, env.Sid, "no_session", "no such session")
		return
	}
	if _, err := s.p.Write(req.Input); err != nil {
		a.replyErr(env.Seq, env.Sid, "write", err.Error())
		return
	}
	s.mu.Lock()
	s.lastIO = time.Now()
	s.mu.Unlock()
	a.reply(env.Seq, env.Sid, wire.MsgShellReadResult, &wire.ShellReadResult{})
}

func (a *Agent) shellRead(env *wire.Envelope, req *wire.ShellRead) {
	s := a.getSession(env.Sid)
	if s == nil {
		a.replyErr(env.Seq, env.Sid, "no_session", "no such session")
		return
	}
	deadline := time.Now()
	if req.TimeoutMs > 0 {
		deadline = deadline.Add(time.Duration(req.TimeoutMs) * time.Millisecond)
	} else {
		deadline = deadline.Add(500 * time.Millisecond)
	}
	for {
		s.mu.Lock()
		if len(s.buf) > 0 || s.done {
			chunk := s.buf
			s.buf = nil
			done := s.done
			s.lastIO = time.Now()
			s.mu.Unlock()
			completion := wire.CompletionHeuristic
			if done {
				completion = wire.CompletionAuthoritative
			}
			a.reply(env.Seq, env.Sid, wire.MsgShellReadResult, &wire.ShellReadResult{
				Chunks:     toChunks(chunk),
				Done:       done,
				Completion: completion,
			})
			return
		}
		s.mu.Unlock()
		if !time.Now().Before(deadline) {
			a.reply(env.Seq, env.Sid, wire.MsgShellReadResult, &wire.ShellReadResult{
				Done:       false,
				Completion: wire.CompletionIdleTimeout,
			})
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (a *Agent) shellClose(env *wire.Envelope) {
	s := a.getSession(env.Sid)
	if s == nil {
		a.replyErr(env.Seq, env.Sid, "no_session", "no such session")
		return
	}
	_ = s.p.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		done := s.done
		code := s.exitCode
		sig := s.signal
		s.mu.Unlock()
		if done || !time.Now().Before(deadline) {
			a.mu.Lock()
			delete(a.shells, env.Sid)
			a.mu.Unlock()
			a.reply(env.Seq, env.Sid, wire.MsgShellCloseResult, &wire.ShellCloseResult{ExitCode: code, Signal: sig})
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (a *Agent) shellSignal(env *wire.Envelope, req *wire.Signal) {
	s := a.getSession(env.Sid)
	if s == nil {
		a.replyErr(env.Seq, env.Sid, "no_session", "no such session")
		return
	}
	if err := s.deliverSignal(req.Sig); err != nil {
		a.replyErr(env.Seq, env.Sid, "signal", err.Error())
		return
	}
	a.reply(env.Seq, env.Sid, wire.MsgShellReadResult, &wire.ShellReadResult{})
}

func (s *shellSession) deliverSignal(sig string) error {
	if s.p == nil {
		return errors.New("no pty")
	}
	switch sig {
	case wire.SigInterrupt:
		_, err := s.p.Write([]byte{0x03}) // Ctrl-C to the foreground group
		return err
	case wire.SigTerminate:
		return killGroup(s.p.PID(), syscall.SIGTERM)
	case wire.SigForceKill:
		return killGroup(s.p.PID(), syscall.SIGKILL)
	case wire.SigQuit:
		return killGroup(s.p.PID(), syscall.SIGQUIT)
	default:
		return fmt.Errorf("unknown signal %q", sig)
	}
}

func toChunks(b []byte) []wire.StreamChunk {
	if len(b) == 0 {
		return nil
	}
	return []wire.StreamChunk{{Stream: wire.StreamStdout, Data: b}}
}
