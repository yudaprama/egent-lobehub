// Package keyvault encrypts and decrypts secrets at rest (user API keys,
// OAuth tokens, bot credentials, connector credentials). It is the Go
// replacement for the TypeScript KeyVaultsGateKeeper.
//
// The TS implementation used WebCrypto AES-256-GCM with a 12-byte random
// IV and emitted the format `{iv-hex}:{authTag-hex}:{ciphertext-hex}`.
// This Go implementation uses Tink-Go (Google's audited crypto library)
// for all NEW encryption, while still being able to DECRYPT existing
// rows written by the TS service.
//
// Key source: the KEY_VAULTS_SECRET env var — base64-encoded 16/24/32
// raw AES key bytes. The same env var powers the TS service, so the two
// stacks can coexist during the migration.
//
// Wire format:
//
//   - NEW (Go/Tink): Tink binary ciphertext. Begins with a 5-byte Tink
//     prefix (0x01 || 4-byte key ID) so it is distinguishable from the
//     legacy format on read. Followed by 12-byte IV + ciphertext + 16-byte
//     GCM tag.
//   - LEGACY (TS/WebCrypto): ASCII string "iv-hex:tag-hex:ciphertext-hex".
//     The first byte is always an ASCII hex digit (0-9, a-f), never 0x01.
//
// Decrypt auto-detects which format it is given. Encrypt always writes
// the Tink format so rows migrate lazily on the next write.
package keyvault

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/keyset"
	gcmpb "github.com/tink-crypto/tink-go/v2/proto/aes_gcm_go_proto"
	tinkpb "github.com/tink-crypto/tink-go/v2/proto/tink_go_proto"
	"github.com/tink-crypto/tink-go/v2/tink"
	"google.golang.org/protobuf/proto"
)

// EnvKey is the env var read by both the TS and Go implementations.
const EnvKey = "KEY_VAULTS_SECRET"

// tinkPrefix is the first byte of every Tink AES-GCM ciphertext produced
// with the default (TINK) variant. Legacy TS ciphertext is ASCII hex, so
// its first byte is in [0-9a-f] and never collides with 0x01.
const tinkPrefix = 0x01

// ErrNotConfigured is returned when KEY_VAULTS_SECRET is unset. Callers
// that want fail-open behaviour should treat a nil *Encryptor as
// "encryption disabled" (Encrypt/Decrypt become passthroughs).
var ErrNotConfigured = errors.New("keyvault: KEY_VAULTS_SECRET not set")

// Encryptor wraps a Tink AEAD primitive for encrypting/decrypting key
// vault data. Thread-safe once constructed; the underlying Tink primitive
// is immutable.
//
// A nil *Encryptor means encryption is disabled (dev/test). Encrypt and
// Decrypt on a nil receiver return their input unchanged.
type Encryptor struct {
	aead   tink.AEAD
	rawKey []byte // for legacy-format decryption only; never written
}

// New creates an Encryptor from the KEY_VAULTS_SECRET env var.
//
// Returns (nil, nil) when the env var is unset — callers should treat
// this as "encryption disabled" and pass the nil encryptor through. This
// matches the TS behaviour where an unset secret leaves the gatekeeper
// inert.
//
// Returns an error when the secret is set but malformed (bad base64 or
// wrong length). The accepted lengths are 16 and 32 bytes — AES-128-GCM
// and AES-256-GCM. The TS service also accepted 24 bytes; Tink rejects
// that length, so callers with a 24-byte key must rotate to 32 bytes.
func New() (*Encryptor, error) {
	secret := os.Getenv(EnvKey)
	if secret == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(secret)
	if err != nil {
		return nil, fmt.Errorf("keyvault: decode %s: %w", EnvKey, err)
	}
	return NewFromRawKey(raw)
}
// NewFromRawKey builds an Encryptor from already-decoded AES key bytes.
// Exposed so tests can construct an encryptor without touching the env.
//
// Accepted lengths are 16 and 32 bytes (AES-128-GCM and AES-256-GCM).
// The TS service accepted 24 bytes as well, but Tink's AES-GCM key
// manager rejects that length for security reasons (24-byte AES-GCM
// is rarely used and lacks hardware acceleration). Callers migrating
// a 24-byte TS key should rotate to a 32-byte key.
func NewFromRawKey(rawKey []byte) (*Encryptor, error) {
	if len(rawKey) != 16 && len(rawKey) != 32 {
		return nil, fmt.Errorf("keyvault: key must be 16 or 32 bytes (AES-128 or AES-256), got %d", len(rawKey))
	}

	a, err := tinkAEADFromRawKey(rawKey)
	if err != nil {
		return nil, fmt.Errorf("keyvault: build tink AEAD: %w", err)
	}

	// Copy the raw key so the caller can mutate the slice they passed in
	// without affecting this encryptor.
	keyCopy := make([]byte, len(rawKey))
	copy(keyCopy, rawKey)

	return &Encryptor{aead: a, rawKey: keyCopy}, nil
}

// Enabled reports whether encryption is active. False when the receiver
// is nil or was constructed without a key.
func (e *Encryptor) Enabled() bool {
	return e != nil && e.aead != nil
}

// Encrypt encrypts plaintext and returns Tink-format ciphertext.
//
// A nil receiver (or disabled encryptor) returns plaintext unchanged —
// the fail-open path used in dev/test where no secret is configured.
//
// The output is NOT the legacy "{iv}:{tag}:{ct}" hex string. It is the
// raw Tink binary (prefix || iv || ciphertext || tag). Callers that
// store this in a text column should base64-encode it; callers storing
// in a bytea column can use it directly.
func (e *Encryptor) Encrypt(plaintext []byte) ([]byte, error) {
	if !e.Enabled() {
		return plaintext, nil
	}
	ct, err := e.aead.Encrypt(plaintext, nil)
	if err != nil {
		return nil, fmt.Errorf("keyvault: encrypt: %w", err)
	}
	return ct, nil
}

// EncryptString is a convenience wrapper for Encrypt that returns a
// base64-encoded string suitable for storage in a text column. It is
// the mirror of DecryptString.
func (e *Encryptor) EncryptString(plaintext string) (string, error) {
	ct, err := e.Encrypt([]byte(plaintext))
	if err != nil {
		return "", err
	}
	if !e.Enabled() {
		return plaintext, nil
	}
	return base64.StdEncoding.EncodeToString(ct), nil
}

// Decrypt reverses Encrypt. It accepts both the Tink binary format
// (produced by this package) and the legacy TS hex format
// ("{iv}:{tag}:{ct}"), auto-detecting which one it received.
//
// A nil receiver returns ciphertext unchanged (fail-open).
//
// The input may be either raw Tink bytes or a base64-encoded string
// produced by EncryptString. Base64 decoding is attempted first; if it
// fails the raw bytes are used directly.
func (e *Encryptor) Decrypt(ciphertext []byte) ([]byte, error) {
	if !e.Enabled() {
		return ciphertext, nil
	}

	// Heuristic: Tink binary starts with 0x01; legacy is ASCII hex.
	// If the input looks like base64 (and isn't the Tink binary), try
	// decoding it first so callers that stored base64 strings work.
	if len(ciphertext) > 0 && ciphertext[0] != tinkPrefix && isLikelyBase64(ciphertext) {
		if decoded, err := base64.StdEncoding.DecodeString(string(ciphertext)); err == nil {
			ciphertext = decoded
		}
	}

	// Tink format.
	if len(ciphertext) > 0 && ciphertext[0] == tinkPrefix {
		pt, err := e.aead.Decrypt(ciphertext, nil)
		if err == nil {
			return pt, nil
		}
		// Fall through to legacy — the 0x01 prefix could theoretically
		// appear in an unlucky legacy value, though it is statistically
		// improbable (hex digits never start with 0x01).
	}

	// Legacy TS hex format.
	if pt, err := e.decryptLegacy(ciphertext); err == nil {
		return pt, nil
	}

	return nil, errors.New("keyvault: decrypt failed (not a valid Tink or legacy ciphertext)")
}

// DecryptString is a convenience wrapper for Decrypt that accepts the
// stored string form (base64 Tink or legacy hex) and returns the
// plaintext as a string.
func (e *Encryptor) DecryptString(ciphertext string) (string, error) {
	pt, err := e.Decrypt([]byte(ciphertext))
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// decryptLegacy parses the TS WebCrypto format "iv-hex:tag-hex:ct-hex"
// and decrypts it with raw AES-GCM using the same key. Returns an error
// if the input is not in that format or the auth tag does not verify.
func (e *Encryptor) decryptLegacy(data []byte) ([]byte, error) {
	s := string(data)
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return nil, errors.New("keyvault: legacy format expects 3 colon-separated parts")
	}
	iv, err := hex.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("keyvault: legacy iv hex: %w", err)
	}
	tag, err := hex.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("keyvault: legacy tag hex: %w", err)
	}
	ct, err := hex.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("keyvault: legacy ciphertext hex: %w", err)
	}

	block, err := aes.NewCipher(e.rawKey)
	if err != nil {
		return nil, fmt.Errorf("keyvault: legacy aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCMWithTagSize(block, len(tag))
	if err != nil {
		return nil, fmt.Errorf("keyvault: legacy gcm: %w", err)
	}

	// Go's GCM.Open expects ciphertext || tag as a single buffer.
	combined := make([]byte, 0, len(ct)+len(tag))
	combined = append(combined, ct...)
	combined = append(combined, tag...)

	plaintext, err := gcm.Open(nil, iv, combined, nil)
	if err != nil {
		return nil, fmt.Errorf("keyvault: legacy gcm open: %w", err)
	}
	return plaintext, nil
}

// tinkAEADFromRawKey builds a Tink AEAD primitive backed by the supplied
// raw AES key. It constructs an AesGcmKey proto, wraps it in a Keyset,
// and reads it back via insecurecleartextkeyset. This is the supported
// Tink pathway for importing existing key material — the "insecure"
// prefix refers to the fact that the keyset is held in cleartext in
// memory (which is unavoidable when the key comes from an env var).
func tinkAEADFromRawKey(rawKey []byte) (tink.AEAD, error) {
	keyProto := &gcmpb.AesGcmKey{
		Version:  0,
		KeyValue: rawKey,
	}
	serializedKey, err := proto.Marshal(keyProto)
	if err != nil {
		return nil, fmt.Errorf("marshal AesGcmKey: %w", err)
	}

	ks := &tinkpb.Keyset{
		PrimaryKeyId: 1,
		Key: []*tinkpb.Keyset_Key{
			{
				KeyData: &tinkpb.KeyData{
					TypeUrl:         "type.googleapis.com/google.crypto.tink.AesGcmKey",
					Value:           serializedKey,
					KeyMaterialType: tinkpb.KeyData_SYMMETRIC,
				},
				Status:       tinkpb.KeyStatusType_ENABLED,
				KeyId:        1,
				OutputPrefixType: tinkpb.OutputPrefixType_TINK,
			},
		},
	}

	serializedKeyset, err := proto.Marshal(ks)
	if err != nil {
		return nil, fmt.Errorf("marshal keyset: %w", err)
	}
	handle, err := insecurecleartextkeyset.Read(keyset.NewBinaryReader(bytes.NewReader(serializedKeyset)))
	if err != nil {
		return nil, fmt.Errorf("read keyset: %w", err)
	}

	a, err := aead.New(handle)
	if err != nil {
		return nil, fmt.Errorf("create AEAD: %w", err)
	}
	return a, nil
}

// isLikelyBase64 returns true when the input is non-empty, has only
// base64-legal characters, and a length that is a multiple of 4. Used
// to decide whether to attempt base64 decoding before Tink decryption.
func isLikelyBase64(b []byte) bool {
	if len(b) == 0 || len(b)%4 != 0 {
		return false
	}
	for _, c := range b {
		if !((c >= 'A' && c <= 'Z') ||
			(c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') ||
			c == '+' || c == '/' || c == '=') {
			return false
		}
	}
	return true
}

// ConstantTimeCompare performs a constant-time comparison of two
// plaintexts. Exposed as a convenience for callers that need to compare
// decrypted secrets without leaking timing information.
func ConstantTimeCompare(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// Ensure the unused bytes import stays meaningful for future key-erasure
// work without forcing callers to import it.
var _ = bytes.Equal
