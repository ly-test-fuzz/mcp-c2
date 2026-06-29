package main

import (
	"encoding/json"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/debugmcp/mcp-c2/internal/certgen"
	"github.com/debugmcp/mcp-c2/internal/hubapi"
	"github.com/debugmcp/mcp-c2/internal/mtls"
	"github.com/debugmcp/mcp-c2/internal/proto"
	"github.com/debugmcp/mcp-c2/internal/remote"
	"github.com/debugmcp/mcp-c2/internal/transport"
)

func main() {
	addr := flag.String("addr", ":8443", "C2 WebSocket listen address (mTLS)")
	apiAddr := flag.String("api-addr", "127.0.0.1:9000", "local HTTP API listen address")
	certsDir := flag.String("certs-dir", "certs", "certificate directory; auto-generated if empty or files missing")
	serverIP := flag.String("server-ip", "", "server IP/hostname for certificate SAN (auto-detected if empty)")
	ca := flag.String("ca", "", "CA certificate path (empty = use certs-dir/ca.crt, auto-gen if missing)")
	cert := flag.String("cert", "", "server certificate path (empty = use certs-dir/server.crt)")
	key := flag.String("key", "", "server private key path (empty = use certs-dir/server.key)")
	allowedPath := flag.String("allowed-clients", "", "client cert SHA-256 fingerprint allowlist file (SIGHUP reload)")
	auditLogPath := flag.String("audit-log", "", "audit log file path")
	genOnly := flag.Bool("gen-certs", false, "generate certificates and exit (for build-time use)")
	flag.Parse()

	// ── Resolve cert paths ─────────────────────────────────────────
	caPath := *ca
	certPath := *cert
	keyPath := *key
	if caPath == "" {
		caPath = filepath.Join(*certsDir, "ca.crt")
	}
	if certPath == "" {
		certPath = filepath.Join(*certsDir, "server.crt")
	}
	if keyPath == "" {
		keyPath = filepath.Join(*certsDir, "server.key")
	}

	// ── Auto-generate certs if missing ─────────────────────────────
	if !fileExists(caPath) || !fileExists(certPath) || !fileExists(keyPath) {
		log.Printf("certificates not found, auto-generating to %s/", *certsDir)

		ips, dns := resolveSANs(*serverIP)
		bundle, err := certgen.GenerateBundle(ips, dns)
		if err != nil {
			log.Fatalf("generate certificates: %v", err)
		}
		if err := bundle.Save(*certsDir); err != nil {
			log.Fatalf("save certificates: %v", err)
		}
		log.Printf("certificates generated:")
		log.Printf("  CA:        %s", caPath)
		log.Printf("  Server:    %s / %s", certPath, keyPath)
		log.Printf("  Client:    %s/client.crt / %s/client.key", *certsDir, *certsDir)
		log.Printf("  Client fingerprint (SHA-256): %s", bundle.ClientFingerprint())
		log.Printf("")
		if *serverIP == "" {
			log.Printf("⚠  No -server-ip specified. Certificate SANs: %v", dns)
			log.Printf("   If clients connect via a different IP, re-run with: -server-ip YOUR_IP")
		}

		if *genOnly {
			return
		}
	}

	if *genOnly {
		log.Printf("certificates already exist in %s/", *certsDir)
		return
	}

	// ── Audit log ─────────────────────────────────────────────────
	if *auditLogPath != "" {
		f, err := os.OpenFile(*auditLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			log.Fatalf("audit log: %v", err)
		}
		defer f.Close()
		log.SetOutput(f)
	}

	log.Println("MCP-C2 hub starting — authorized environments only")

	// ── Allowed client fingerprints ───────────────────────────────
	allowed, err := mtls.LoadAllowedList(*allowedPath)
	if err != nil {
		log.Fatalf("load allowed clients: %v", err)
	}
	var allowedAtomic atomic.Value
	allowedAtomic.Store(allowed)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP)
	go func() {
		for range sigCh {
			al, err := mtls.LoadAllowedList(*allowedPath)
			if err != nil {
				log.Printf("reload allowed-clients: %v", err)
				continue
			}
			allowedAtomic.Store(al)
			log.Printf("reloaded allowed-clients")
		}
	}()

	// ── TLS config for C2 WebSocket ───────────────────────────────
	tlsCfg, err := mtls.ServerConfig(caPath, certPath, keyPath, true)
	if err != nil {
		log.Fatalf("tls config: %v", err)
	}

	// ── C2 hub + session manager ──────────────────────────────────
	hub := transport.NewHub(func(fp string) bool {
		cur := allowedAtomic.Load().(*mtls.AllowedList)
		return cur.Empty() || cur.ContainsFingerprint(fp)
	})
	rm := remote.NewManager(hub)
	hub.SetHandlers(
		func(cs *transport.ClientSession, hp *proto.HelloPayload) {
			ptyMode := "PTY"
			if hp.Caps != nil && hp.Caps["conpty"] {
				ptyMode = "ConPTY"
			} else if hp.Caps != nil && !hp.Caps["pty"] {
				ptyMode = "pipe"
			}
			log.Printf("")
			log.Printf("══════════════════════════════════════════════")
			log.Printf("  CLIENT ONLINE: %s", cs.ID)
			log.Printf("  hostname: %s  os: %s  arch: %s", hp.Hostname, hp.OS, hp.Arch)
			log.Printf("  mode: %s", ptyMode)
			log.Printf("  fingerprint: %s", cs.Summary.CertFingerprint[:16]+"...")
			log.Printf("══════════════════════════════════════════════")
			log.Printf("")
		},
		func(cs *transport.ClientSession, f *proto.Frame) {
			rm.HandleFrame(cs, f)
		},
		func(cs *transport.ClientSession) {
			log.Printf("")
			log.Printf("  CLIENT OFFLINE: %s (host=%s)", cs.ID, cs.Summary.Hostname)
			log.Printf("")
		},
	)

	// ── HTTP mux ──────────────────────────────────────────────────
	mux := hubapi.ServeMux(hub, rm)

	// Client cert bundle download endpoint (for building clients)
	mux.HandleFunc("/api/v1/certs/client-bundle", func(w http.ResponseWriter, r *http.Request) {
		clientCRT, err := os.ReadFile(filepath.Join(*certsDir, "client.crt"))
		if err != nil {
			http.Error(w, "client cert not found", http.StatusNotFound)
			return
		}
		clientKey, err := os.ReadFile(filepath.Join(*certsDir, "client.key"))
		if err != nil {
			http.Error(w, "client key not found", http.StatusNotFound)
			return
		}
		caCRT, err := os.ReadFile(caPath)
		if err != nil {
			http.Error(w, "ca cert not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"ca_crt":     string(caCRT),
			"client_crt": string(clientCRT),
			"client_key": string(clientKey),
		})
	})

	// Client fingerprint endpoint
	mux.HandleFunc("/api/v1/certs/client-fingerprint", func(w http.ResponseWriter, r *http.Request) {
		clientCRT, err := os.ReadFile(filepath.Join(*certsDir, "client.crt"))
		if err != nil {
			http.Error(w, "client cert not found", http.StatusNotFound)
			return
		}
		certs, err := mtls.ParseCertPEM(clientCRT)
		if err != nil || len(certs) == 0 {
			http.Error(w, "invalid client cert", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"fingerprint": mtls.FingerprintSHA256(certs[0]),
		})
	})

	// Also expose /clients for compatibility
	mux.HandleFunc("/clients", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(hub.List())
	})

	// ── Start C2 WebSocket server (mTLS) ──────────────────────────
	c2srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("C2 WebSocket listening on https://%s/c2", *addr)
		if err := c2srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			log.Fatalf("C2 server: %v", err)
		}
	}()

	// ── Start local HTTP API server ───────────────────────────────
	apisrv := &http.Server{
		Addr:              *apiAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("local API listening on http://%s", *apiAddr)
	port := (*addr)[1:] // strip leading ':'
	if port == "" {
		port = "8443"
	}
	log.Printf("hub ready — connect clients to wss://<host>:%s/c2", port)
	if err := apisrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// ── helpers ────────────────────────────────────────────────────────────

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// resolveSANs returns IPs and DNS names for the server certificate.
func resolveSANs(serverIP string) ([]net.IP, []string) {
	var ips []net.IP
	var dns []string

	if serverIP != "" {
		if ip := net.ParseIP(serverIP); ip != nil {
			ips = append(ips, ip)
		} else {
			dns = append(dns, serverIP)
		}
	} else {
		// Auto-detect: include loopback + hostname + non-loopback IPs
		ips = append(ips, net.IPv4(127, 0, 0, 1), net.IPv6loopback)
		if host, err := os.Hostname(); err == nil {
			dns = append(dns, host, "localhost")
		}
		// Add all non-loopback interface addresses
		if ifaces, err := net.Interfaces(); err == nil {
			for _, iface := range ifaces {
				if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
					continue
				}
				addrs, err := iface.Addrs()
				if err != nil {
					continue
				}
				for _, a := range addrs {
					if ipnet, ok := a.(*net.IPNet); ok {
						if ip := ipnet.IP.To4(); ip != nil && !ip.IsLoopback() {
							ips = append(ips, ip)
						} else if ipnet.IP.To16() != nil && !ipnet.IP.IsLoopback() {
							ips = append(ips, ipnet.IP)
						}
					}
				}
			}
		}
	}
	return ips, dns
}
