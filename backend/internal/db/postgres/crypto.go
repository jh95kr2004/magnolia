/*
Package postgres
This file is a utility module that performs two-way encryption and decryption using the AES-GCM standard to prevent plaintext storage of sensitive user information (e.g., exchange API Secret Keys) in the database.
*/
package postgres

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
)

// getMasterKey fetches the 32-byte master encryption key from the environment.
func getMasterKey() ([]byte, error) {
	key := os.Getenv("ENCRYPTION_MASTER_KEY")
	if len(key) != 32 {
		return nil, errors.New("ENCRYPTION_MASTER_KEY must be exactly 32 bytes long")
	}
	return []byte(key), nil
}

// Encrypt payload using AES-GCM and the master key.
func Encrypt(plaintext []byte) ([]byte, error) {
	key, err := getMasterKey()
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, nil
}

// Decrypt payload using AES-GCM and the master key.
func Decrypt(ciphertext []byte) ([]byte, error) {
	key, err := getMasterKey()
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	if len(ciphertext) < gcm.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decryption failed: %w", err)
	}

	return plaintext, nil
}
