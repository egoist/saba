// Package cluster connects the primary to the other servers of a saba
// cluster.
//
// Each node runs a saba agent that dials out to the primary over TLS (ALPN
// "saba-cluster/1" on the primary's HTTPS port, or a dedicated port when TLS
// is off). Both sides authenticate with keys pinned when the node joined.
// The connection is multiplexed (yamux): the primary opens a stream per
// Docker API call and per proxied request, and the agent splices each one to
// the node's Docker socket or to a replica's loopback port. Nodes therefore
// need no open ports, can sit behind NAT, and the primary holds no
// credentials to them beyond the connection the agent itself opened.
package cluster

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/egoist/saba/internal/api"
)

// ALPN is the TLS protocol name agents use to reach the primary.
const ALPN = "saba-cluster/1"

// Hello is the agent's first message.
type Hello struct {
	Node     string `json:"node"`
	Version  string `json:"version"`
	Token    string `json:"token,omitempty"` // one-time join token, first connection only
	Hostname string `json:"hostname"`
	Platform string `json:"platform"`
	Docker   string `json:"docker"`
}

// Welcome is the primary's answer to Hello.
type Welcome struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Version string `json:"version,omitempty"`
}

// Heartbeat is sent by the agent periodically.
type Heartbeat struct {
	Stats  api.HostStats `json:"stats"`
	Docker string        `json:"docker,omitempty"`
}

// stream operations, opened by the primary.
type streamRequest struct {
	Op   string `json:"op"` // docker | tcp
	Port int    `json:"port,omitempty"`
}

type streamReply struct {
	Error string `json:"error,omitempty"`
}

const maxFrame = 1 << 20

// writeFrame writes v as a length-prefixed JSON frame. Frames rather than
// lines, so that no reader buffers bytes past the message before the
// connection is handed to the multiplexer.
func writeFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b)))
	if _, err := w.Write(n[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func readFrame(r io.Reader, v any) error {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(n[:])
	if size > maxFrame {
		return fmt.Errorf("frame of %d bytes is too large", size)
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func yamuxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	// Large windows keep image transfers and big responses fast on links
	// with latency.
	c.MaxStreamWindowSize = 16 << 20
	c.KeepAliveInterval = 5 * time.Second
	c.ConnectionWriteTimeout = 20 * time.Second
	c.StreamOpenTimeout = 15 * time.Second
	c.LogOutput = io.Discard
	return c
}

// JoinInfo is what a node needs to join: where the primary is, how to
// recognize it, and a one-time token.
type JoinInfo struct {
	Node        string `json:"n"`
	Primary     string `json:"p"` // host:port
	Fingerprint string `json:"f"` // the primary's key
	Token       string `json:"t"`
}

const joinPrefix = "saba-join-"

func (j JoinInfo) String() string {
	b, _ := json.Marshal(j)
	return joinPrefix + base64.RawURLEncoding.EncodeToString(b)
}

func ParseJoin(s string) (JoinInfo, error) {
	var j JoinInfo
	raw, ok := strings.CutPrefix(strings.TrimSpace(s), joinPrefix)
	if !ok {
		return j, errors.New("not a saba join code (it starts with " + joinPrefix + ")")
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return j, errors.New("the join code is damaged; copy it again")
	}
	if err := json.Unmarshal(b, &j); err != nil || j.Node == "" || j.Primary == "" || j.Fingerprint == "" || j.Token == "" {
		return j, errors.New("the join code is incomplete; copy it again")
	}
	return j, nil
}

// Identity is a key pair with a self-signed certificate. Peers recognize
// each other by the certificate's public key, not by a CA.
type Identity struct {
	Cert        tls.Certificate
	Fingerprint string
}

// Fingerprint identifies a certificate by its public key.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256:" + base64.RawURLEncoding.EncodeToString(sum[:])
}

// LoadIdentity loads dir/name.{key,crt}, creating them on first use.
func LoadIdentity(dir, name string) (*Identity, error) {
	keyPath, certPath := filepath.Join(dir, name+".key"), filepath.Join(dir, name+".crt")
	if pair, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return nil, err
		}
		pair.Leaf = leaf
		return &Identity{Cert: pair, Fingerprint: Fingerprint(leaf)}, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "saba " + name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(100, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, err
	}
	return LoadIdentity(dir, name)
}

// pinned verifies that the peer presented the certificate with this key.
func pinned(fingerprint string) func([][]byte, [][]*x509.Certificate) error {
	return func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return errors.New("no certificate presented")
		}
		cert, err := x509.ParseCertificate(raw[0])
		if err != nil {
			return err
		}
		if Fingerprint(cert) != fingerprint {
			return fmt.Errorf("the primary's key is %s, but this node joined %s; re-add the node with `saba node add --rejoin`", Fingerprint(cert), fingerprint)
		}
		return nil
	}
}
