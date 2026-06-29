package hubapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/debugmcp/mcp-c2/internal/outputbuf"
	"github.com/debugmcp/mcp-c2/internal/remote"
	"github.com/debugmcp/mcp-c2/internal/transport"
)

// ServeMux builds an *http.ServeMux that serves the hub REST API at /api/v1/
// and the C2 WebSocket upgrade at /c2.
func ServeMux(hub *transport.Hub, rm *remote.Manager) *http.ServeMux {
	mux := http.NewServeMux()

	// C2 WebSocket (client connections)
	mux.HandleFunc("/c2", hub.ServeWS)

	// Health check (no auth)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})

	// ── REST API v1 ────────────────────────────────────────────────

	// GET /api/v1/health
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		jsonOK(w, map[string]any{
			"status":       "ok",
			"version":      "0.2.0",
			"client_count": len(hub.List()),
		})
	})

	// GET /api/v1/clients
	mux.HandleFunc("/api/v1/clients", func(w http.ResponseWriter, r *http.Request) {
		jsonOK(w, hub.List())
	})

	// /api/v1/sessions (collection)
	mux.HandleFunc("/api/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// GET /api/v1/sessions?client_id=X
			cid := r.URL.Query().Get("client_id")
			if cid == "" {
				http.Error(w, "missing client_id", http.StatusBadRequest)
				return
			}
			jsonOK(w, rm.List(cid))

		case http.MethodPost:
			// POST /api/v1/sessions  {client_id, shell}
			var req struct {
				ClientID string `json:"client_id"`
				Shell    string `json:"shell"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			info, err := rm.Open(req.ClientID, req.Shell)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			jsonOK(w, info)

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// /api/v1/sessions/:id (single session)
	mux.HandleFunc("/api/v1/sessions/", func(w http.ResponseWriter, r *http.Request) {
		// Extract session ID from path: /api/v1/sessions/{id}/...
		cid := r.URL.Query().Get("client_id")
		sid, sub := splitSessionPath(strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/"))
		if sid == "" {
			http.Error(w, "missing session_id", http.StatusBadRequest)
			return
		}
		if cid == "" {
			http.Error(w, "missing client_id", http.StatusBadRequest)
			return
		}

		switch {
		case sub == "" && r.Method == http.MethodDelete:
			// DELETE /api/v1/sessions/:id?client_id=X
			if err := rm.Close(cid, sid); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			jsonOK(w, map[string]bool{"ok": true})

		case sub == "cmd" && r.Method == http.MethodPost:
			// POST /api/v1/sessions/:id/cmd?client_id=X
			var req struct {
				Command       string `json:"command"`
				AppendNewline bool   `json:"append_newline"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			cursor, err := rm.SendInput(cid, sid, req.Command, req.AppendNewline)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			jsonOK(w, map[string]any{"output_cursor": cursor})

		case sub == "input" && r.Method == http.MethodPost:
			// POST /api/v1/sessions/:id/input?client_id=X
			var req struct {
				Text          string `json:"text"`
				AppendNewline bool   `json:"append_newline"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			cursor, err := rm.SendInput(cid, sid, req.Text, req.AppendNewline)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			jsonOK(w, map[string]any{"output_cursor": cursor})

		case sub == "output" && r.Method == http.MethodGet:
			// GET /api/v1/sessions/:id/output?client_id=X&since=N&max_bytes=M&block_ms=B
			since := int64Param(r, "since", 0)
			maxBytes := intParam(r, "max_bytes", 0)
			blockMS := intParam(r, "block_ms", 0)
			if blockMS < 0 {
				blockMS = 0
			}
			if blockMS > 5000 {
				blockMS = 5000
			}
			res, err := readOutputWithBlock(rm, cid, sid, since, maxBytes, blockMS)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			jsonOK(w, res)

		case sub == "interrupt" && r.Method == http.MethodPost:
			// POST /api/v1/sessions/:id/interrupt?client_id=X
			if err := rm.Interrupt(cid, sid); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			jsonOK(w, map[string]bool{"ok": true})

		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})

	// ── File operations ────────────────────────────────────────────

	mux.HandleFunc("/api/v1/files/list", func(w http.ResponseWriter, r *http.Request) {
		cid := r.URL.Query().Get("client_id")
		path := r.URL.Query().Get("path")
		if cid == "" {
			http.Error(w, "missing client_id", http.StatusBadRequest)
			return
		}
		files, err := rm.ListFiles(cid, path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, map[string]any{"files": files, "path": path})
	})

	mux.HandleFunc("/api/v1/files/download", func(w http.ResponseWriter, r *http.Request) {
		cid := r.URL.Query().Get("client_id")
		if cid == "" {
			http.Error(w, "missing client_id", http.StatusBadRequest)
			return
		}
		var req struct {
			RemotePath string `json:"remote_path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ftp, err := rm.Download(cid, req.RemotePath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, ftp)
	})

	mux.HandleFunc("/api/v1/files/upload", func(w http.ResponseWriter, r *http.Request) {
		cid := r.URL.Query().Get("client_id")
		if cid == "" {
			http.Error(w, "missing client_id", http.StatusBadRequest)
			return
		}
		var req struct {
			RemotePath string `json:"remote_path"`
			Data       []byte `json:"data"`
			Overwrite  bool   `json:"overwrite"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ftp, err := rm.Upload(cid, req.RemotePath, req.Data, req.Overwrite)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, ftp)
	})

	// POST /api/v1/files/download-batch — native recursive tar download (like scp -r)
	mux.HandleFunc("/api/v1/files/download-batch", func(w http.ResponseWriter, r *http.Request) {
		cid := r.URL.Query().Get("client_id")
		if cid == "" {
			http.Error(w, "missing client_id", http.StatusBadRequest)
			return
		}
		var req struct {
			Paths []string `json:"paths"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(req.Paths) == 0 {
			http.Error(w, "empty paths", http.StatusBadRequest)
			return
		}

		ftp, err := rm.RecursiveDownload(cid, req.Paths)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, ftp)
	})

	return mux
}

// ── helpers ────────────────────────────────────────────────────────────

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// splitSessionPath splits "sid/cmd" → ("sid", "cmd"), "sid" → ("sid", "").
func splitSessionPath(p string) (sessionID, sub string) {
	for i, ch := range p {
		if ch == '/' {
			return p[:i], p[i+1:]
		}
	}
	return p, ""
}

func int64Param(r *http.Request, name string, def int64) int64 {
	s := r.URL.Query().Get(name)
	if s == "" {
		return def
	}
	var v int64
	fmt.Sscanf(s, "%d", &v)
	return v
}

func intParam(r *http.Request, name string, def int) int {
	return int(int64Param(r, name, int64(def)))
}

// readOutputWithBlock implements long-poll for the HTTP API.
func readOutputWithBlock(rm *remote.Manager, clientID, sessionID string, since int64, maxBytes, blockMS int) (outputbuf.ReadResult, error) {
	deadline := time.Now().Add(time.Duration(blockMS) * time.Millisecond)
	for {
		res, err := rm.ReadOutput(clientID, sessionID, since, maxBytes)
		if err != nil || blockMS == 0 || len(res.Output) > 0 || !res.Alive || time.Now().After(deadline) {
			return res, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}
