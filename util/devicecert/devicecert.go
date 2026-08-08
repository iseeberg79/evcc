// Package devicecert provides a single, lazily-generated self-signed client
// certificate/key pair that evcc can present as its own identity when a
// device requires mutual TLS (mTLS) authentication (e.g. Modbus/TLS).
//
// The pair is generated once, persisted in the settings database, and
// reused across restarts. Unlike EEBUS's certificate (which is scoped to
// the SHIP protocol) or MQTT's client certificate (typically issued by the
// broker operator's own CA), this identity has no protocol-specific
// meaning of its own — it exists purely so users configuring an mTLS
// device aren't required to run openssl themselves. The device still has
// to explicitly trust this certificate (e.g. by approving it in the
// device's own UI), exactly as it would any other client certificate.
package devicecert

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"sync"

	"github.com/enbility/ship-go/cert"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/server/db/settings"
)

// identity is the subject embedded in the generated certificate. It carries
// no protocol semantics, just enough to identify the issuing evcc instance
// in a device's certificate list.
const (
	organizationalUnit = "evcc"
	organization       = "evcc"
	country            = ""
	commonName         = "evcc"
)

type pair struct {
	Cert string `json:"cert"`
	Key  string `json:"key"`
}

var mu sync.Mutex

// GetOrCreate returns evcc's self-signed device identity as a PEM-encoded
// certificate/key pair, generating and persisting it on first use.
func GetOrCreate() (certPEM string, keyPEM string, err error) {
	mu.Lock()
	defer mu.Unlock()

	var p pair
	if err := settings.Json(keys.DeviceCertificate, &p); err == nil && p.Cert != "" && p.Key != "" {
		return p.Cert, p.Key, nil
	}

	certPEM, keyPEM, err = create()
	if err != nil {
		return "", "", err
	}

	if err := settings.SetJson(keys.DeviceCertificate, pair{Cert: certPEM, Key: keyPEM}); err != nil {
		return "", "", fmt.Errorf("devicecert: persisting: %w", err)
	}

	return certPEM, keyPEM, nil
}

// create generates a new self-signed ECDSA certificate/key pair, PEM-encoded.
func create() (certPEM string, keyPEM string, err error) {
	tlsCert, err := cert.CreateCertificate(organizationalUnit, organization, country, commonName)
	if err != nil {
		return "", "", fmt.Errorf("devicecert: generating certificate: %w", err)
	}

	certBuf := new(bytes.Buffer)
	if err := pem.Encode(certBuf, &pem.Block{Type: "CERTIFICATE", Bytes: tlsCert.Certificate[0]}); err != nil {
		return "", "", fmt.Errorf("devicecert: encoding certificate: %w", err)
	}

	key, ok := tlsCert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return "", "", errors.New("devicecert: unexpected private key type")
	}

	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", fmt.Errorf("devicecert: marshalling private key: %w", err)
	}

	keyBuf := new(bytes.Buffer)
	if err := pem.Encode(keyBuf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		return "", "", fmt.Errorf("devicecert: encoding private key: %w", err)
	}

	return certBuf.String(), keyBuf.String(), nil
}
