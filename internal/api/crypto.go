package api

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
)

// encryptor encrypts/decrypts account passwords at rest with AES-GCM.
// Key comes from ENCRYPTION_KEY (32 random bytes, base64-encoded).
type encryptor struct {
	gcm cipher.AEAD
}

func newEncryptor() (*encryptor, error) {
	keyB64 := os.Getenv("ENCRYPTION_KEY")
	if keyB64 == "" {
		return nil, fmt.Errorf("ENCRYPTION_KEY env var is required (32 random bytes, base64 — e.g. `openssl rand -base64 32`)")
	}
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("ENCRYPTION_KEY must decode to exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &encryptor{gcm: gcm}, nil
}

func (e *encryptor) encrypt(plaintext string) ([]byte, error) {
	nonce := make([]byte, e.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return e.gcm.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

func (e *encryptor) decrypt(ciphertext []byte) (string, error) {
	nonceSize := e.gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, data := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := e.gcm.Open(nil, nonce, data, nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}
