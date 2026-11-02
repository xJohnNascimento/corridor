package migrations

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

const (
	scramIterations = 4096
	scramSaltBytes  = 16
)

// scramVerifier returns the SCRAM-SHA-256 secret Postgres stores for a password (RFC 5802, RFC 7677),
// so the plaintext never appears in a statement the server may log. Postgres applies SASLprep to the
// password; it is the identity for printable ASCII, which is all this accepts.
func scramVerifier(password string) (string, error) {
	for _, r := range password {
		if r < 0x20 || r > 0x7e {
			return "", errors.New("the password must be printable ASCII")
		}
	}
	salt := make([]byte, scramSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	stored, server, err := scramKeys(password, salt, scramIterations)
	if err != nil {
		return "", err
	}
	encode := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", scramIterations, encode(salt), encode(stored), encode(server)), nil
}

func scramKeys(password string, salt []byte, iterations int) (storedKey, serverKey []byte, err error) {
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return nil, nil, err
	}
	client := hmacSHA256(salted, "Client Key")
	stored := sha256.Sum256(client)
	return stored[:], hmacSHA256(salted, "Server Key"), nil
}

func hmacSHA256(key []byte, message string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(message))
	return mac.Sum(nil)
}
