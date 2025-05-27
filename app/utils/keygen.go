package utils

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// GenerateAPIKeyHashes takes a plaintext API key and returns its SHA256 hash (for lookup)
// and its bcrypt hash (for secure storage and comparison).
func GenerateAPIKeyHashes(plaintextKey string) (lookupKeyHex string, bcryptHashString string, err error) {
	if plaintextKey == "" {
		return "", "", fmt.Errorf("plaintext API key cannot be empty")
	}

	// Generate SHA256 lookup key
	hasher := sha256.New()
	hasher.Write([]byte(plaintextKey))
	lookupKeyHex = hex.EncodeToString(hasher.Sum(nil))

	// Generate bcrypt hash
	// bcrypt.DefaultCost is 10, which is a good starting point.
	bcryptHashBytes, err := bcrypt.GenerateFromPassword([]byte(plaintextKey), bcrypt.DefaultCost)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate bcrypt hash: %w", err)
	}
	bcryptHashString = string(bcryptHashBytes)

	return lookupKeyHex, bcryptHashString, nil
}
