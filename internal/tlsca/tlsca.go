// Package tlsca issues the certificates Sonda presents when it terminates TLS
// for a client, signed by an authority it generates once and keeps beside the
// database.
//
// Nothing here installs anything. Sonda writes the authority to disk and prints
// the commands to trust it and to remove it again; running them is the user's
// decision. A debugging tool that silently adds a root to a machine's trust
// store is indistinguishable from malware, and a root the user cannot find
// later is worse than no root at all.
//
// The private key is the most dangerous thing this process holds: anyone with
// it can impersonate any site to this machine. It is written owner-only, it is
// never logged, and no accessor here returns it.
package tlsca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	// caLife is bounded on purpose. A root good for a decade is a root nobody
	// remembers installing, still trusted long after the afternoon it was needed
	// for — and Sonda cannot withdraw it from a store it never wrote to. A year
	// is long enough not to be a nuisance and short enough to expire on its own.
	caLife = 365 * 24 * time.Hour

	// leafLife only has to outlast a debugging session. The cache is per
	// process, so every leaf is reissued on the next run regardless.
	leafLife = 30 * 24 * time.Hour

	// backdate covers the clock skew between a host and a container, which is
	// the one place a certificate that is valid "from now" is refused as
	// not-yet-valid.
	backdate = time.Hour

	certFile = "sonda-ca.pem"
	keyFile  = "sonda-ca-key.pem"

	organization = "Sonda debugging proxy"
)

// CA is the authority and the certificates issued from it.
type CA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte

	certPath, keyPath string

	// leaves is keyed by the name the client asked for. Without it a busy page
	// generates a key per connection, which is both slow and pointless: the
	// certificate for a name never changes within a run.
	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// Open loads the authority from dir, creating it when there is none or when the
// one on disk has expired. It is called only when a service is actually set to
// terminate TLS, so a user who never asks for it never gets a CA on disk.
func Open(dir string) (*CA, error) {
	certPath := filepath.Join(dir, certFile)
	keyPath := filepath.Join(dir, keyFile)

	ca, why := load(certPath, keyPath)
	if ca != nil {
		return ca, nil
	}

	ca, err := create(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	// Printed, never installed. This is the whole contract of the feature, so it
	// goes out at a level nobody filters away.
	slog.Warn("sonda created a local certificate authority (" + why + "). Nothing was added to this machine's trust store; that part is yours.\n" + ca.Instructions().String())
	return ca, nil
}

// load returns the stored authority, or nil and the reason a new one is needed.
func load(certPath, keyPath string) (*CA, string) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, "there was none yet"
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, "its private key was missing"
	}

	// A key readable by anyone else on the machine is a key that can impersonate
	// any site to it. Sonda does not silently repair the file — the user may
	// have widened it deliberately, and changing it behind their back is its own
	// surprise — but it refuses to stay quiet about it.
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(keyPath); err == nil && info.Mode().Perm()&0o077 != 0 {
			slog.Warn("the Sonda CA private key is readable beyond its owner; anyone who can read it can impersonate any site to this machine",
				"path", keyPath, "mode", info.Mode().Perm().String())
		}
	}

	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, "the stored one could not be decoded"
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, "the stored certificate could not be parsed"
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		// Deliberately says nothing about the key material itself.
		return nil, "the stored private key could not be parsed"
	}
	if time.Now().After(cert.NotAfter) {
		return nil, "the previous one expired on " + cert.NotAfter.Format(time.DateOnly) + ", so it has to be trusted again"
	}

	return &CA{
		cert: cert, key: key, certPEM: certPEM,
		certPath: certPath, keyPath: keyPath,
		leaves: map[string]*tls.Certificate{},
	}, ""
}

func create(certPath, keyPath string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate the CA key: %w", err)
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}

	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown host"
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			// Whoever finds this root in a trust store a year from now has to be
			// able to tell what put it there and which machine it belongs to,
			// without asking anyone.
			CommonName:   "Sonda local CA (" + host + ")",
			Organization: []string{organization},
		},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(caLife),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		// This root signs leaves and nothing else: it may not mint another CA.
		MaxPathLenZero: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create the CA certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode the CA key: %w", err)
	}

	// Removed first, then created 0600. os.WriteFile applies its mode only when
	// it creates the file, so writing over a key left world-readable by an
	// earlier version would keep the old permissions.
	if err := os.Remove(keyPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("replace the CA key at %s: %w", keyPath, err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("write the CA key to %s: %w", keyPath, err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("write the CA certificate to %s: %w", certPath, err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{
		cert: cert, key: key, certPEM: certPEM,
		certPath: certPath, keyPath: keyPath,
		leaves: map[string]*tls.Certificate{},
	}, nil
}

// Config is what a listener terminates with. One config serves every service:
// the certificate is chosen per connection from the name the client asked for,
// which is the only thing that decides it.
func (c *CA) Config() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// Without ALPN a gRPC client cannot negotiate HTTP/2 over this listener
		// and falls back to HTTP/1.1, which is not a transport gRPC has.
		NextProtos:     []string{"h2", "http/1.1"},
		GetCertificate: c.certificateFor,
	}
}

// CertificatePEM is the public certificate. There is no counterpart for the
// key, and adding one would be the mistake this package exists to avoid.
func (c *CA) CertificatePEM() []byte { return c.certPEM }

func (c *CA) certificateFor(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name := hello.ServerName
	if name == "" {
		// A client connecting to a literal address sends no SNI at all — `curl
		// https://127.0.0.1:9443` is the ordinary case here — so the address it
		// reached is the only name there is to certify.
		name = localHost(hello.Conn)
	}
	if name == "" {
		return nil, fmt.Errorf("sonda: the client sent no server name and the local address could not be read, so there is no name to certify")
	}

	// The lock is held across issuance on purpose: twenty parallel requests for
	// one page must produce one certificate, not twenty.
	c.mu.Lock()
	defer c.mu.Unlock()
	if leaf, ok := c.leaves[name]; ok {
		return leaf, nil
	}
	leaf, err := c.issue(name)
	if err != nil {
		return nil, err
	}
	c.leaves[name] = leaf
	return leaf, nil
}

func (c *CA) issue(name string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name, Organization: []string{organization}},
		NotBefore:    now.Add(-backdate),
		NotAfter:     now.Add(leafLife),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	// A subject alternative name is what every client actually checks; the
	// common name has not been consulted for a decade.
	if ip := net.ParseIP(name); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{name}
	}

	der, err := x509.CreateCertificate(rand.Reader, template, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	// Only the leaf goes on the wire. Sending the root would help nobody: a
	// client that trusts it already has it, and one that does not would refuse
	// it anyway.
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

func localHost(conn net.Conn) string {
	if conn == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return ""
	}
	return host
}

// serialNumber is 128 bits from the same source the keys come from. A
// predictable serial is one of the few things that makes a certificate
// forgeable without the key.
func serialNumber() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("draw a certificate serial: %w", err)
	}
	return n, nil
}

// Step is one thing a person can run, and where it applies.
type Step struct {
	Where   string `json:"where"`
	Command string `json:"command"`
}

// Instructions is everything needed to trust this authority and, later, to find
// it and take it back out. Sonda prints it and serves it; it runs none of it.
//
// The private key appears nowhere in here except inside the removal command,
// where the point is deleting the file rather than reading it.
type Instructions struct {
	Path        string `json:"path"`
	Subject     string `json:"subject"`
	Serial      string `json:"serial"`
	Fingerprint string `json:"fingerprint_sha256"`
	Expires     string `json:"expires"`

	// PerTool comes first because it is usually the right answer: pointing one
	// program at the file trusts nothing else on the machine, needs no
	// administrator, and leaves nothing behind to remove.
	PerTool []Step `json:"per_tool"`
	Trust   []Step `json:"trust_system_wide"`
	Remove  []Step `json:"remove"`
}

func (c *CA) Instructions() Instructions {
	sum := sha256.Sum256(c.cert.Raw)
	hex := make([]string, 0, len(sum))
	for _, b := range sum {
		hex = append(hex, fmt.Sprintf("%02X", b))
	}
	subject := c.cert.Subject.CommonName
	path := c.certPath

	return Instructions{
		Path:        path,
		Subject:     subject,
		Serial:      strings.ToUpper(c.cert.SerialNumber.Text(16)),
		Fingerprint: strings.Join(hex, ":"),
		Expires:     c.cert.NotAfter.UTC().Format(time.RFC3339),

		PerTool: []Step{
			{"curl", `curl --cacert "` + path + `" https://…`},
			{"Node.js", `NODE_EXTRA_CA_CERTS="` + path + `"`},
			{"Go", `SSL_CERT_FILE="` + path + `"`},
			{"Python requests", `REQUESTS_CA_BUNDLE="` + path + `"`},
		},
		Trust: []Step{
			{"macOS", `sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain "` + path + `"`},
			{"Windows", `certutil -user -addstore Root "` + path + `"`},
			{"Linux (Debian, Ubuntu)", `sudo cp "` + path + `" /usr/local/share/ca-certificates/sonda-ca.crt && sudo update-ca-certificates`},
			{"Linux (Fedora, RHEL)", `sudo cp "` + path + `" /etc/pki/ca-trust/source/anchors/sonda-ca.pem && sudo update-ca-trust`},
			{"Firefox", `Firefox keeps its own store: Settings → Privacy & Security → Certificates → View Certificates → Authorities → Import, then tick "Trust this CA to identify websites".`},
		},
		Remove: []Step{
			{"macOS", `sudo security delete-certificate -c "` + subject + `" /Library/Keychains/System.keychain`},
			{"Windows", `certutil -user -delstore Root ` + strings.ToUpper(c.cert.SerialNumber.Text(16))},
			{"Linux (Debian, Ubuntu)", `sudo rm /usr/local/share/ca-certificates/sonda-ca.crt && sudo update-ca-certificates --fresh`},
			{"Linux (Fedora, RHEL)", `sudo rm /etc/pki/ca-trust/source/anchors/sonda-ca.pem && sudo update-ca-trust`},
			{"Firefox", `The same dialog: Authorities → select "` + subject + `" → Delete or Distrust.`},
			// Last, and deliberately so: withdraw the trust first, then delete
			// the files. The other order leaves a root the machine still trusts
			// and nobody can account for.
			{"the files themselves", `rm "` + path + `" "` + c.keyPath + `"`},
		},
	}
}

// String is the block Sonda prints when it creates the authority.
func (i Instructions) String() string {
	var b strings.Builder
	b.WriteString("\n  certificate: " + i.Path + "\n")
	b.WriteString("  subject:     " + i.Subject + "\n")
	b.WriteString("  sha256:      " + i.Fingerprint + "\n")
	b.WriteString("  expires:     " + i.Expires + "\n")
	b.WriteString("\n  Trust it in one program only — no administrator, nothing to undo later:\n")
	for _, s := range i.PerTool {
		b.WriteString("    " + s.Where + ": " + s.Command + "\n")
	}
	b.WriteString("\n  Or trust it for the whole machine, if a browser has to accept it. Run it yourself; Sonda will not:\n")
	for _, s := range i.Trust {
		b.WriteString("    " + s.Where + ": " + s.Command + "\n")
	}
	b.WriteString("\n  To take it back out again:\n")
	for _, s := range i.Remove {
		b.WriteString("    " + s.Where + ": " + s.Command + "\n")
	}
	return b.String()
}
