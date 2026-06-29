package mtls

import (
	"bufio"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
)

func LoadCAPool(caPath string) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if caPath == "" {
		return pool, nil
	}
	pemBytes, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read ca: %w", err)
	}
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("no certs found in %s", caPath)
	}
	return pool, nil
}

func ServerConfig(caPath, certPath, keyPath string, requireClientCert bool) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load server keypair: %w", err)
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	if requireClientCert {
		pool, err := LoadCAPool(caPath)
		if err != nil {
			return nil, err
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

func ClientConfig(caPath, certPath, keyPath, serverName string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load client keypair: %w", err)
	}
	pool, err := LoadCAPool(caPath)
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, Certificates: []tls.Certificate{cert}, ServerName: serverName}, nil
}

// ParseCertPEM parses one or more PEM-encoded certificates.
func ParseCertPEM(pemBytes []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for {
		block, rest := pem.Decode(pemBytes)
		if block == nil {
			break
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return out, err
		}
		out = append(out, cert)
		pemBytes = rest
	}
	return out, nil
}

func FingerprintSHA256(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

type AllowedList struct {
	entries map[string]struct{}
}

func LoadAllowedList(path string) (*AllowedList, error) {
	al := &AllowedList{entries: map[string]struct{}{}}
	if path == "" {
		return al, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		al.entries[strings.ToLower(strings.ReplaceAll(line, ":", ""))] = struct{}{}
	}
	return al, s.Err()
}

func (a *AllowedList) Empty() bool { return a == nil || len(a.entries) == 0 }

func (a *AllowedList) ContainsFingerprint(fp string) bool {
	if a == nil || len(a.entries) == 0 {
		return true
	}
	_, ok := a.entries[strings.ToLower(strings.ReplaceAll(fp, ":", ""))]
	return ok
}
