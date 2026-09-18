package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"fmt"
)

// encryptAESCBC encrypts plaintext using AES-CBC with PKCS7 padding and returns base64.
func encryptAESCBC(keyStr, ivStr string, plaintext []byte) (string, error) {
	key := []byte(keyStr)
	iv := []byte(ivStr)

	// Validate key length (16 for AES-128, 32 for AES-256)
	if len(key) != 16 && len(key) != 32 {
		return "", errors.New("encryption key must be 16 or 32 bytes")
	}

	// Validate IV length (must be 16 bytes for AES)
	if len(iv) != aes.BlockSize {
		return "", fmt.Errorf("encryption IV must be %d bytes", aes.BlockSize)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("failed to create cipher: %w", err)
	}

	// Apply PKCS7 padding
	padding := aes.BlockSize - len(plaintext)%aes.BlockSize
	padText := bytes.Repeat([]byte{byte(padding)}, padding)
	padded := make([]byte, 0, len(plaintext)+padding)
	padded = append(padded, plaintext...)
	padded = append(padded, padText...)

	// Encrypt
	ciphertext := make([]byte, len(padded))
	mode := cipher.NewCBCEncrypter(block, iv)
	mode.CryptBlocks(ciphertext, padded)

	return base64.StdEncoding.EncodeToString(ciphertext), nil
}
