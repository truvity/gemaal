package authn

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testCA renders a self-signed certificate, PEM-encoded — enough for
// the CA pool assembly (nothing dials it).
func testCA(t *testing.T) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"test"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	require.NoError(t, err)

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestNewTokenReviewerFromFiles(t *testing.T) {
	dir := t.TempDir()

	tokenPath := filepath.Join(dir, "token")
	caPath := filepath.Join(dir, "ca.crt")

	require.NoError(t, os.WriteFile(tokenPath, []byte("the-token\n"), 0o600))
	require.NoError(t, os.WriteFile(caPath, testCA(t), 0o600))

	reviewer, err := newTokenReviewer("10.96.0.1", "443", tokenPath, caPath, []string{"gemaal"})
	require.NoError(t, err)

	assert.Equal(t, "https://10.96.0.1:443", reviewer.BaseURL)
	assert.Equal(t, "the-token", reviewer.Token, "the token is trimmed")
	assert.Equal(t, []string{"gemaal"}, reviewer.Audiences)
	assert.NotNil(t, reviewer.Client)
}

func TestNewTokenReviewerMissingFiles(t *testing.T) {
	dir := t.TempDir()

	caPath := filepath.Join(dir, "ca.crt")
	require.NoError(t, os.WriteFile(caPath, testCA(t), 0o600))

	_, err := newTokenReviewer("h", "1", filepath.Join(dir, "absent"), caPath, nil)
	require.ErrorContains(t, err, "service-account token")

	tokenPath := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("t"), 0o600))

	_, err = newTokenReviewer("h", "1", tokenPath, filepath.Join(dir, "absent"), nil)
	require.ErrorContains(t, err, "cluster CA")
}

func TestNewTokenReviewerGarbageCA(t *testing.T) {
	dir := t.TempDir()

	tokenPath := filepath.Join(dir, "token")
	caPath := filepath.Join(dir, "ca.crt")

	require.NoError(t, os.WriteFile(tokenPath, []byte("t"), 0o600))
	require.NoError(t, os.WriteFile(caPath, []byte("not a pem"), 0o600))

	_, err := newTokenReviewer("h", "1", tokenPath, caPath, nil)
	require.ErrorContains(t, err, "no certificates")
}
