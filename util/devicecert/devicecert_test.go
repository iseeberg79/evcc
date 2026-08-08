package devicecert

import (
	"crypto/tls"
	"path/filepath"
	"testing"

	"github.com/evcc-io/evcc/server/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupDB(t *testing.T) {
	t.Helper()
	require.NoError(t, db.NewInstance("sqlite", filepath.Join(t.TempDir(), "test.db")))
}

func TestGetOrCreate(t *testing.T) {
	setupDB(t)

	certPEM, keyPEM, err := GetOrCreate()
	require.NoError(t, err)
	assert.NotEmpty(t, certPEM)
	assert.NotEmpty(t, keyPEM)

	// valid, matching PEM-encoded X509 key pair
	_, err = tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	require.NoError(t, err)

	t.Run("idempotent", func(t *testing.T) {
		cert2, key2, err := GetOrCreate()
		require.NoError(t, err)
		assert.Equal(t, certPEM, cert2)
		assert.Equal(t, keyPEM, key2)
	})
}

func TestGetOrCreate_ConcurrentSafe(t *testing.T) {
	setupDB(t)

	const n = 10
	certs := make([]string, n)
	errs := make([]error, n)
	done := make(chan int, n)

	for i := range n {
		go func(i int) {
			certs[i], _, errs[i] = GetOrCreate()
			done <- i
		}(i)
	}
	for range n {
		<-done
	}

	for i := range n {
		require.NoError(t, errs[i])
		assert.Equal(t, certs[0], certs[i], "all callers must observe the same generated certificate")
	}
}
