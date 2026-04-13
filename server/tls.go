package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

const (
	serverCertName = "vm-builder-agent.crt"
	serverKeyName  = "vm-builder-agent.key"
)

// buildTLSConfig loads or creates the server cert/key in privateDir and fetches
// the client-trust CA from caURL.
func buildTLSConfig(privateDir, caURL string) (*tls.Config, error) {
	cert, err := loadOrCreateServerKeyPair(privateDir)
	if err != nil {
		return nil, err
	}

	caPEM, err := fetchCACertificate(caURL)
	if err != nil {
		return nil, err
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse CA cert from %s", caURL)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

func loadOrCreateServerKeyPair(privateDir string) (tls.Certificate, error) {
	if privateDir == "" {
		return tls.Certificate{}, fmt.Errorf("private-dir is required when mTLS is enabled")
	}
	if err := os.MkdirAll(privateDir, 0o700); err != nil {
		return tls.Certificate{}, fmt.Errorf("create private dir: %w", err)
	}

	certPath := filepath.Join(privateDir, serverCertName)
	keyPath := filepath.Join(privateDir, serverKeyName)
	if fileExists(certPath) && fileExists(keyPath) {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("load cert/key from %s: %w", privateDir, err)
		}
		return cert, nil
	}

	if err := generateSelfSignedServerKeyPair(certPath, keyPath); err != nil {
		return tls.Certificate{}, err
	}

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("load generated cert/key from %s: %w", privateDir, err)
	}
	return cert, nil
}

func fetchCACertificate(caURL string) ([]byte, error) {
	parsedURL, err := url.Parse(caURL)
	if err != nil {
		return nil, fmt.Errorf("parse ca-url: %w", err)
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return nil, fmt.Errorf("ca-url must use http or https")
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(caURL)
	if err != nil {
		return nil, fmt.Errorf("fetch CA from %s: %w", caURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch CA from %s: unexpected status %s", caURL, resp.Status)
	}

	caPEM, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read CA from %s: %w", caURL, err)
	}
	return caPEM, nil
}

func generateSelfSignedServerKeyPair(certPath, keyPath string) error {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate private key: %w", err)
	}

	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "vm-builder-agent"
	}

	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return fmt.Errorf("generate serial number: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: hostname,
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              uniqueStrings([]string{hostname, "localhost"}),
		IPAddresses: []net.IP{
			net.ParseIP("127.0.0.1"),
			net.ParseIP("::1"),
		},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return fmt.Errorf("create self-signed certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return fmt.Errorf("marshal private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := writeFileAtomically(certPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("write certificate: %w", err)
	}
	if err := writeFileAtomically(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write private key: %w", err)
	}
	return nil
}

func writeFileAtomically(path string, contents []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(contents); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
