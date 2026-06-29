package embedded

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
)

//go:embed data/ca.crt
var DefaultCACert []byte

//go:embed data/client.crt
var DefaultClientCert []byte

//go:embed data/client.key
var DefaultClientKey []byte

func TLSConfig(serverName string) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(DefaultClientCert, DefaultClientKey)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(DefaultCACert)
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		RootCAs:      pool,
		Certificates: []tls.Certificate{cert},
		ServerName:   serverName,
	}, nil
}
