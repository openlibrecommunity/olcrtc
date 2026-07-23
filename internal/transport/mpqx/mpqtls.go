package mpqx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"time"

	"github.com/SolverNA/mpq-brutal/core"
	"golang.org/x/crypto/hkdf"
)

// ServerName is the SNI/CommonName used for the mpq QUIC-TLS identity. Because
// the client pins the server's SPKI (see ClientTLS) rather than validating the
// certificate chain and SAN, the exact value only has to match between the two
// ends; it is never resolved or checked against a hostname.
const ServerName = "olcrtc-mpq"

// hkdfSalt / hkdfInfo domain-separate the key derivation so the tunnel's shared
// secret cannot be confused with any other use of the same bytes.
var (
	hkdfSalt = []byte("olcrtc-mpq-tls-v1")
	hkdfInfo = []byte("ecdsa-p256-server-identity")
)

// deriveKey deterministically derives the server's ECDSA P-256 identity key from
// the tunnel pre-shared secret (psk). Both ends run this over the SAME psk and
// therefore obtain byte-identical keys: the server serves a self-signed cert on
// this key, and the client independently computes the expected SPKI pin from it.
// No extra key exchange is needed — the shared secret already authenticates the
// QUIC-TLS layer, so the mpq tunnel is no weaker than the existing PSK-based
// muxconn+handshake path.
//
// The scalar is derived by hand rather than via ecdsa.GenerateKey: the standard
// library deliberately reads an extra random byte (randutil.MaybeReadByte) to
// defeat deterministic key generation, so feeding it a deterministic reader
// still yields different keys on each call. Here we pull 48 bytes from a
// deterministic HKDF stream and reduce them into [1, N-1]; the 128 surplus bits
// make the modular bias cryptographically negligible.
func deriveKey(psk []byte) (*ecdsa.PrivateKey, error) {
	curve := elliptic.P256()
	n := curve.Params().N

	r := hkdf.New(sha256.New, psk, hkdfSalt, hkdfInfo)
	buf := make([]byte, 48) // 256-bit order + 128 surplus bits to flatten bias
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("mpqx: derive identity scalar: %w", err)
	}

	// d = (buf mod (N-1)) + 1  ->  d in [1, N-1], never zero.
	nMinus1 := new(big.Int).Sub(n, big.NewInt(1))
	d := new(big.Int).Mod(new(big.Int).SetBytes(buf), nMinus1)
	d.Add(d, big.NewInt(1))

	priv := new(ecdsa.PrivateKey)
	priv.Curve = curve
	priv.D = d
	priv.PublicKey.X, priv.PublicKey.Y = curve.ScalarBaseMult(d.Bytes()) //nolint:staticcheck // deterministic key derivation needs the scalar API
	return priv, nil
}

// ServerTLS builds the server-side *tls.Config for mpq: a self-signed
// certificate whose key is derived deterministically from psk. The certificate
// serial and validity window are irrelevant to authentication (the client pins
// only the SPKI, which depends solely on the public key), so they are fixed.
func ServerTLS(psk []byte) (*tls.Config, error) {
	key, err := deriveKey(psk)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: ServerName},
		DNSNames:              []string{ServerName, "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("mpqx: create server certificate: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{der},
			PrivateKey:  key,
			Leaf: func() *x509.Certificate {
				leaf, _ := x509.ParseCertificate(der)
				return leaf
			}(),
		}},
		MinVersion: tls.VersionTLS13,
	}, nil
}

// ClientTLS builds the client-side *tls.Config for mpq: it derives the same
// identity key from psk, computes the expected SPKI pin (lowercase hex of
// SHA-256 over the DER SubjectPublicKeyInfo), and returns a pinned config via
// core.PinnedClientTLS. The pin depends only on the public key, so it matches
// the server's certificate regardless of its serial/validity fields. The TLS
// handshake fails closed if the presented SPKI does not match — this is NOT
// InsecureSkipVerify-style acceptance of any certificate.
func ClientTLS(psk []byte) (*tls.Config, error) {
	key, err := deriveKey(psk)
	if err != nil {
		return nil, err
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("mpqx: marshal SPKI: %w", err)
	}
	sum := sha256.Sum256(spki)
	return core.PinnedClientTLS(ServerName, hex.EncodeToString(sum[:]), nil), nil
}
