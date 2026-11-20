package access

import (
	"crypto/rand"
	"encoding/hex"

	"golang.org/x/crypto/argon2"
)

// HashPassword is the argon2id hash stored in auth_credential.
func HashPassword(pw, salt string) string {
	return hex.EncodeToString(argon2.IDKey([]byte(pw), []byte(salt), 1, 64*1024, 4, 32))
}

// RandomHex returns n random bytes as hex, used for salts and tokens.
func RandomHex(n int) string { b := make([]byte, n); _, _ = rand.Read(b); return hex.EncodeToString(b) }
