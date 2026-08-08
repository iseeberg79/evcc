package server

import (
	"net/http"

	"github.com/evcc-io/evcc/util/devicecert"
)

// deviceCertHandler returns evcc's self-signed device identity (PEM-encoded
// certificate/key pair), generating it on first use. Intended for the config
// UI to prefill mTLS-capable device fields (e.g. Modbus/TLS) without
// requiring the user to generate their own certificate.
func deviceCertHandler(w http.ResponseWriter, r *http.Request) {
	cert, key, err := devicecert.GetOrCreate()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err)
		return
	}

	res := struct {
		Cert string `json:"cert"`
		Key  string `json:"key"`
	}{cert, key}

	jsonWrite(w, res)
}
