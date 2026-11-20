package mdm

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
)

// Secrets at rest are sealed with AES-256-GCM under the app's secret key, in the same format as the Google Drive
// token (drive.go): nonce || ciphertext || tag, with the field's name as additional data, so a sealed value cannot
// be moved to another field. A webhook secret or API key is shown once when it is made; afterwards only its last
// four characters are.

// The additional data of each sealed field.
const (
	aadSubscriberSecret = "mdm_subscriber.secret"
	aadInboundSecret    = "mdm_setting.inbound_secret"
	aadCMSAPIKey        = "mdm_setting.cms_api_key"
)

// Prefixes of the secrets HRMS makes, so a leaked one is recognisable.
const (
	prefixWebhookSecret = "whsec_"
	prefixFeedKey       = "hrmsk_"
	prefixInboundSecret = "cmsin_"
)

func sealKey(k []byte) []byte {
	if len(k) == 32 {
		return k
	}
	if v := os.Getenv("APP_SECRET"); v != "" {
		h := sha256.Sum256([]byte(v))
		return h[:]
	}
	// No key given (tests): one for this process only; sealed values then last until restart.
	k = make([]byte, 32)
	_, _ = rand.Read(k)
	return k
}

func (s *Service) seal(aad, plain string) ([]byte, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(plain), []byte(aad)), nil
}

func (s *Service) unseal(aad string, b []byte) (string, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(b) < gcm.NonceSize() {
		return "", errors.New("sealed value too short")
	}
	p, err := gcm.Open(nil, b[:gcm.NonceSize()], b[gcm.NonceSize():], []byte(aad))
	return string(p), err
}

// newSecret is prefix + 32 random bytes, base64url.
func newSecret(prefix string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func last4(s string) string {
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}

// keyHash is what is stored of a pull-feed API key.
func keyHash(k string) string {
	h := sha256.Sum256([]byte(k))
	return hex.EncodeToString(h[:])
}

// Sign is the signature HRMS sends and the CMS must send: "sha256=" + hex HMAC-SHA256(secret, timestamp + "." + body),
// timestamp in Unix seconds.
func Sign(secret string, ts int64, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(strconv.FormatInt(ts, 10)))
	m.Write([]byte("."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func signatureOK(secret string, ts int64, body []byte, got string) bool {
	return hmac.Equal([]byte(Sign(secret, ts, body)), []byte(got))
}
