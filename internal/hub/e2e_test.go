package hub

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"debugmcp/internal/agent"
)

func newE2E(t *testing.T) (*Hub, string, context.CancelFunc) {
	t.Helper()
	psk := bytes.Repeat([]byte{0x11}, 32)
	h := New(psk, nil)
	ln, err := h.ListenAgents("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = h.ServeAgentsOn(ctx, ln) }()
	return h, addr, cancel
}

func waitForTarget(t *testing.T, h *Hub) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ts := h.ListTargets(); len(ts) > 0 {
			return ts[0].ID
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("agent did not register within timeout")
	return ""
}

func startAgent(t *testing.T, ctx context.Context, addr string, psk []byte, cap int) {
	t.Helper()
	a := agent.New(agent.Options{HubAddr: addr, PSK: psk, NoDaemon: true, Cap: cap})
	errc := make(chan error, 1)
	go func() { errc <- a.Run(ctx) }()
	// Fail fast if Run returns immediately with an error.
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("agent.Run: %v", err)
		}
	default:
	}
}

func TestE2E_ExecBashSyntax(t *testing.T) {
	h, addr, cancel := newE2E(t)
	defer cancel()
	psk := h.PSK()
	startAgent(t, context.Background(), addr, psk, 8)
	target := waitForTarget(t, h)

	// command substitution + pipe + redirect: proves login-shell wrapping works.
	cmd := `echo "hi-$(echo 42)" | tr -d '[:space:]'`
	res, err := h.Exec(ExecParams{OpSession: "win-A", Target: target, Command: cmd})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "hi-42") {
		t.Fatalf("exec result: code=%d out=%q err=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
	if res.Completion != "authoritative" {
		t.Fatalf("expected authoritative completion, got %q", res.Completion)
	}
}

// TestE2E_UploadDownload_SingleFile drives the high-level Upload/Download with a
// real local file on the operator (hub) side. The hub does disk I/O and streams
// to the agent via the chunked path; only paths + {size,sha256} cross the API.
func TestE2E_UploadDownload_SingleFile(t *testing.T) {
	h, addr, cancel := newE2E(t)
	defer cancel()
	startAgent(t, context.Background(), addr, h.PSK(), 8)
	target := waitForTarget(t, h)

	// operator-side local source
	localSrc := t.TempDir() + "/src.bin"
	payload := make([]byte, 10*1024*1024) // 10 MiB — spans multiple 4 MiB chunks
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(localSrc, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	remotePath := t.TempDir() + "/remote.bin"

	ures, err := h.Upload(UploadParams{Target: target, LocalPath: localSrc, RemotePath: remotePath, IsDir: false})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if ures.Err != "" {
		t.Fatalf("upload err: %s", ures.Err)
	}
	if ures.Size != int64(len(payload)) {
		t.Fatalf("upload size: got %d want %d", ures.Size, len(payload))
	}
	if ures.Sha256 != sha256Hex(payload) {
		t.Fatalf("upload sha mismatch: got %s want %s", ures.Sha256, sha256Hex(payload))
	}

	// verify file landed verbatim on the agent side (target = same process here)
	got, err := osReadFile(remotePath)
	if err != nil {
		t.Fatalf("agent-side read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("uploaded content mismatch: got %d want %d bytes", len(got), len(payload))
	}

	// download back to a different local path
	localDst := t.TempDir() + "/dst.bin"
	dres, err := h.Download(DownloadParams{Target: target, RemotePath: remotePath, LocalPath: localDst, IsDir: false})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if dres.Err != "" {
		t.Fatalf("download err: %s", dres.Err)
	}
	got2, err := osReadFile(localDst)
	if err != nil {
		t.Fatalf("operator-side read: %v", err)
	}
	if !bytes.Equal(got2, payload) {
		t.Fatalf("downloaded content mismatch")
	}
}

// TestE2E_UploadDownload_Dir round-trips a multi-level directory tree (incl. a
// subdir + nested file) through tar streaming both ways.
func TestE2E_UploadDownload_Dir(t *testing.T) {
	h, addr, cancel := newE2E(t)
	defer cancel()
	startAgent(t, context.Background(), addr, h.PSK(), 8)
	target := waitForTarget(t, h)

	localSrc := t.TempDir() + "/srcdir"
	mustMkdirAll(localSrc + "/sub")
	if err := writeFile(localSrc+"/a.txt", []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(localSrc+"/sub/b.txt", []byte("beta"), 0o644); err != nil {
		t.Fatal(err)
	}
	remotePath := t.TempDir() + "/remotedir"

	ures, err := h.Upload(UploadParams{Target: target, LocalPath: localSrc, RemotePath: remotePath, IsDir: true})
	if err != nil || ures.Err != "" {
		t.Fatalf("upload dir: %v %+v", err, ures)
	}
	// verify entries landed
	gotA, err := osReadFile(remotePath + "/a.txt")
	if err != nil {
		t.Fatalf("remote a.txt: %v", err)
	}
	if string(gotA) != "alpha" {
		t.Errorf("remote a.txt = %q", gotA)
	}
	gotB, err := osReadFile(remotePath + "/sub/b.txt")
	if err != nil {
		t.Fatalf("remote sub/b.txt: %v", err)
	}
	if string(gotB) != "beta" {
		t.Errorf("remote sub/b.txt = %q", gotB)
	}

	// download the directory back
	localDst := t.TempDir() + "/dstdir"
	dres, err := h.Download(DownloadParams{Target: target, RemotePath: remotePath, LocalPath: localDst, IsDir: true})
	if err != nil || dres.Err != "" {
		t.Fatalf("download dir: %v %+v", err, dres)
	}
	gotA2, _ := osReadFile(localDst + "/a.txt")
	if string(gotA2) != "alpha" {
		t.Errorf("downloaded a.txt = %q", gotA2)
	}
	gotB2, _ := osReadFile(localDst + "/sub/b.txt")
	if string(gotB2) != "beta" {
		t.Errorf("downloaded sub/b.txt = %q", gotB2)
	}
}

// TestE2E_Upload_TarFileAsFile sends a real .tar archive with is_dir=false and
// confirms the agent keeps it as a file (not unpacked).
func TestE2E_Upload_TarFileAsFile(t *testing.T) {
	h, addr, cancel := newE2E(t)
	defer cancel()
	startAgent(t, context.Background(), addr, h.PSK(), 8)
	target := waitForTarget(t, h)

	// build a real tar archive in memory: one file "inside.txt"
	var tarBuf bytes.Buffer
	tw := newTarWriter(&tarBuf)
	writeTarEntry(tw, "inside.txt", []byte("payload"))
	closeTarWriter(tw)
	localSrc := t.TempDir() + "/archive.tar"
	if err := writeFile(localSrc, tarBuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	remotePath := t.TempDir() + "/archive.tar"

	ures, err := h.Upload(UploadParams{Target: target, LocalPath: localSrc, RemotePath: remotePath, IsDir: false})
	if err != nil || ures.Err != "" {
		t.Fatalf("upload tar-as-file: %v %+v", err, ures)
	}
	got, err := osReadFile(remotePath)
	if err != nil {
		t.Fatalf("remote archive read: %v", err)
	}
	if !bytes.Equal(got, tarBuf.Bytes()) {
		t.Fatalf("tar file not preserved verbatim: got %d want %d bytes", len(got), tarBuf.Len())
	}
}

// TestE2E_Download_IsDirMismatch: remote is a regular file but caller says is_dir=true.
func TestE2E_Download_IsDirMismatch(t *testing.T) {
	h, addr, cancel := newE2E(t)
	defer cancel()
	startAgent(t, context.Background(), addr, h.PSK(), 8)
	target := waitForTarget(t, h)

	// stage a single remote file via upload (is_dir=false)
	remotePath := t.TempDir() + "/plain.txt"
	localStage := t.TempDir() + "/stage.txt"
	writeFile(localStage, []byte("x"), 0o644)
	if _, err := h.Upload(UploadParams{Target: target, LocalPath: localStage, RemotePath: remotePath, IsDir: false}); err != nil {
		t.Fatal(err)
	}

	localDst := t.TempDir() + "/out"
	_, err := h.Download(DownloadParams{Target: target, RemotePath: remotePath, LocalPath: localDst, IsDir: true})
	if err == nil {
		t.Fatalf("expected is_dir mismatch error, got nil")
	}
}

func TestE2E_InteractiveShell(t *testing.T) {
	h, addr, cancel := newE2E(t)
	defer cancel()
	startAgent(t, context.Background(), addr, h.PSK(), 8)
	target := waitForTarget(t, h)

	open, err := h.ShellOpen(ShellOpenParams{OpSession: "win-A", Target: target})
	if err != nil {
		t.Fatalf("shell_open: %v", err)
	}
	if open.Sid == "" {
		t.Fatalf("expected session id, got %+v", open)
	}
	if err := h.ShellSend(ShellSendParams{Target: target, Sid: open.Sid, Input: []byte("echo shell-hello-XYZ\r\n")}); err != nil {
		t.Fatalf("shell_send: %v", err)
	}
	read, err := h.ShellRead(ShellReadParams{Target: target, Sid: open.Sid, TimeoutMs: 1500})
	if err != nil {
		t.Fatalf("shell_read: %v", err)
	}
	var all []byte
	for _, c := range read.Chunks {
		all = append(all, c.Data...)
	}
	if !strings.Contains(string(all), "shell-hello-XYZ") {
		t.Fatalf("interactive shell did not echo token; got %q", all)
	}
	if _, err := h.ShellClose(ShellCloseParams{Target: target, Sid: open.Sid}); err != nil {
		t.Fatalf("shell_close: %v", err)
	}
}

func TestE2E_OccupancyBusy(t *testing.T) {
	h, addr, cancel := newE2E(t)
	defer cancel()
	startAgent(t, context.Background(), addr, h.PSK(), 2) // cap=2
	target := waitForTarget(t, h)

	var sids []string
	for i := 0; i < 2; i++ {
		o, err := h.ShellOpen(ShellOpenParams{OpSession: "win-A", Target: target})
		if err != nil || o.Sid == "" {
			t.Fatalf("open %d: %v %+v", i, err, o)
		}
		sids = append(sids, o.Sid)
	}
	// Third open must be busy (cap=2).
	o, err := h.ShellOpen(ShellOpenParams{OpSession: "win-B", Target: target})
	if err != nil {
		t.Fatalf("open3: %v", err)
	}
	if o.Busy == nil || !o.Busy.Busy || o.Busy.Used != 2 || o.Busy.Cap != 2 {
		t.Fatalf("expected busy{used:2,cap:2}, got %+v", o)
	}
	// Occupancy visible in list_targets.
	ts := h.ListTargets()
	if !ts[0].Busy || ts[0].SessionsActive != 2 || ts[0].ConcurrencyCap != 2 {
		t.Fatalf("occupancy not surfaced: %+v", ts[0])
	}
	// Closing frees the slot; a new open should then succeed.
	if _, err := h.ShellClose(ShellCloseParams{Target: target, Sid: sids[0]}); err != nil {
		t.Fatal(err)
	}
	o2, err := h.ShellOpen(ShellOpenParams{OpSession: "win-C", Target: target})
	if err != nil {
		t.Fatal(err)
	}
	if o2.Sid == "" {
		t.Fatalf("expected a fresh session after close, got %+v", o2)
	}
	_, _ = h.ShellClose(ShellCloseParams{Target: target, Sid: o2.Sid})
	_, _ = h.ShellClose(ShellCloseParams{Target: target, Sid: sids[1]})
}

// --- test helpers (thin wrappers to keep the upload/download tests readable) ---

func writeFile(path string, data []byte, mode os.FileMode) error {
	return os.WriteFile(path, data, mode)
}

func osReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func mustMkdirAll(path string) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		panic(err)
	}
}

func newTarWriter(w io.Writer) *tar.Writer { return tar.NewWriter(w) }

func writeTarEntry(tw *tar.Writer, name string, data []byte) {
	_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(data))})
	_, _ = tw.Write(data)
}

func closeTarWriter(tw *tar.Writer) { _ = tw.Close() }
