package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecryptSecretRoundtrip(t *testing.T) {
	master := "dev-master-key-change-me-000000000000000000000000"
	key, err := decodeMasterKey(master)
	require.NoError(t, err)

	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	nonce := make([]byte, gcm.NonceSize())
	_, err = io.ReadFull(rand.Reader, nonce)
	require.NoError(t, err)
	ct := gcm.Seal(nil, nonce, []byte("sftp-password"), nil)

	plain, err := DecryptSecret(master, ct, nonce)
	require.NoError(t, err)
	require.Equal(t, "sftp-password", string(plain))
}

func TestDecodeMasterKeyRejectsShort(t *testing.T) {
	_, err := decodeMasterKey("short")
	require.Error(t, err)
}
