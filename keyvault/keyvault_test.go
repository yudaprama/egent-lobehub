package keyvault

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// testKey returns a fresh 32-byte AES key for tests.
func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return k
}

func TestNew_NoEnv(t *testing.T) {
	t.Setenv(EnvKey, "")
	e, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e != nil {
		t.Fatalf("expected nil encryptor when env unset, got %v", e)
	}
}

func TestNew_InvalidBase64(t *testing.T) {
	t.Setenv(EnvKey, "!!!not-base64!!!")
	_, err := New()
	if err == nil {
		t.Fatal("expected error for bad base64")
	}
}

func TestNew_InvalidKeyLength(t *testing.T) {
	cases := []struct {
		keyLen int
		desc   string
	}{
		{15, "15 bytes"},
		{24, "24 bytes (not supported by Tink)"},
		{33, "33 bytes"},
	}
	for _, c := range cases {
		raw := make([]byte, c.keyLen)
		t.Setenv(EnvKey, base64.StdEncoding.EncodeToString(raw))
		_, err := New()
		if err == nil {
			t.Errorf("key size %d (%s): expected error", c.keyLen, c.desc)
		}
	}
}

func TestNew_ValidKeyLengths(t *testing.T) {
	for _, n := range []int{16, 32} {
		raw := make([]byte, n)
		t.Setenv(EnvKey, base64.StdEncoding.EncodeToString(raw))
		e, err := New()
		if err != nil {
			t.Errorf("key size %d: %v", n, err)
			continue
		}
		if e == nil || !e.Enabled() {
			t.Errorf("key size %d: expected enabled encryptor", n)
		}
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	e, err := NewFromRawKey(testKey(t))
	if err != nil {
		t.Fatalf("NewFromRawKey: %v", err)
	}

	cases := [][]byte{
		[]byte("hello world"),
		[]byte(""),
		bytes.Repeat([]byte{0xAB}, 4096),
		[]byte("sk-1234567890abcdef"),
	}

	for i, pt := range cases {
		ct, err := e.Encrypt(pt)
		if err != nil {
			t.Fatalf("case %d encrypt: %v", i, err)
		}
		if !e.Enabled() {
			t.Fatal("encryptor unexpectedly disabled")
		}
		// Tink ciphertext must start with 0x01 prefix.
		if len(ct) == 0 || ct[0] != tinkPrefix {
			t.Errorf("case %d: ciphertext does not start with Tink prefix", i)
		}

		got, err := e.Decrypt(ct)
		if err != nil {
			t.Fatalf("case %d decrypt: %v", i, err)
		}
		if !bytes.Equal(got, pt) {
			t.Errorf("case %d: plaintext mismatch got=%q want=%q", i, got, pt)
		}
	}
}

func TestEncryptStringDecryptStringRoundTrip(t *testing.T) {
	e, err := NewFromRawKey(testKey(t))
	if err != nil {
		t.Fatalf("NewFromRawKey: %v", err)
	}

	plain := "super-secret-api-key"
	enc, err := e.EncryptString(plain)
	if err != nil {
		t.Fatalf("EncryptString: %v", err)
	}
	if enc == plain {
		t.Fatal("expected encrypted form to differ from plaintext")
	}

	got, err := e.DecryptString(enc)
	if err != nil {
		t.Fatalf("DecryptString: %v", err)
	}
	if got != plain {
		t.Errorf("mismatch: got=%q want=%q", got, plain)
	}
}

func TestDecryptLegacyTSFormat(t *testing.T) {
	rawKey := testKey(t)
	e, err := NewFromRawKey(rawKey)
	if err != nil {
		t.Fatalf("NewFromRawKey: %v", err)
	}

	plaintext := []byte(`{"apiKey":"sk-xxx","org":"acme"}`)

	// Encrypt using raw Go AES-GCM (the TS WebCrypto equivalent).
	block, err := aes.NewCipher(rawKey)
	if err != nil {
		t.Fatalf("aes cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block) // default 16-byte tag
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	iv := make([]byte, gcm.NonceSize()) // 12 bytes
	if _, err := rand.Read(iv); err != nil {
		t.Fatalf("rand iv: %v", err)
	}
	ct := gcm.Seal(nil, iv, plaintext, nil)
	tag := ct[len(ct)-gcm.Overhead():]
	body := ct[:len(ct)-gcm.Overhead()]

	legacy := strings.Join([]string{
		hex.EncodeToString(iv),
		hex.EncodeToString(tag),
		hex.EncodeToString(body),
	}, ":")

	got, err := e.Decrypt([]byte(legacy))
	if err != nil {
		t.Fatalf("Decrypt legacy: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("legacy mismatch: got=%q want=%q", got, plaintext)
	}
}

func TestDecryptDetectsFormat(t *testing.T) {
	rawKey := testKey(t)
	e, err := NewFromRawKey(rawKey)
	if err != nil {
		t.Fatalf("NewFromRawKey: %v", err)
	}

	// Encrypt with Tink → decrypt should recover.
	pt := []byte("detect-me")
	ct, err := e.Encrypt(pt)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := e.Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt tink: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Errorf("tink mismatch: %q vs %q", got, pt)
	}

	// Encrypt with raw AES-GCM (legacy TS format) → decrypt should recover.
	block, _ := aes.NewCipher(rawKey)
	gcm, _ := cipher.NewGCM(block)
	iv := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(iv)
	out := gcm.Seal(nil, iv, pt, nil)
	tag := out[len(out)-gcm.Overhead():]
	body := out[:len(out)-gcm.Overhead()]
	legacy := strings.Join([]string{
		hex.EncodeToString(iv),
		hex.EncodeToString(tag),
		hex.EncodeToString(body),
	}, ":")
	got, err = e.Decrypt([]byte(legacy))
	if err != nil {
		t.Fatalf("Decrypt legacy: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Errorf("legacy mismatch: %q vs %q", got, pt)
	}
}

func TestDecryptGarbageFails(t *testing.T) {
	e, err := NewFromRawKey(testKey(t))
	if err != nil {
		t.Fatalf("NewFromRawKey: %v", err)
	}
	cases := [][]byte{
		nil,
		[]byte(""),
		[]byte("not-valid-at-all"),
		[]byte("aa:bb:cc"), // valid shape but invalid hex values would fail too
	}
	for i, c := range cases {
		_, err := e.Decrypt(c)
		if err == nil {
			t.Errorf("case %d: expected error for input %q", i, c)
		}
	}
}

func TestNilEncryptorIsPassthrough(t *testing.T) {
	var e *Encryptor
	if e.Enabled() {
		t.Fatal("nil encryptor should be disabled")
	}
	ct, err := e.Encrypt([]byte("plain"))
	if err != nil {
		t.Fatalf("nil Encrypt: %v", err)
	}
	if string(ct) != "plain" {
		t.Errorf("nil Encrypt should pass through, got %q", ct)
	}
	pt, err := e.Decrypt([]byte("cipher"))
	if err != nil {
		t.Fatalf("nil Decrypt: %v", err)
	}
	if string(pt) != "cipher" {
		t.Errorf("nil Decrypt should pass through, got %q", pt)
	}
}

func TestNilEncryptorStringPassthrough(t *testing.T) {
	var e *Encryptor
	enc, err := e.EncryptString("plain")
	if err != nil {
		t.Fatalf("EncryptString: %v", err)
	}
	if enc != "plain" {
		t.Errorf("expected passthrough, got %q", enc)
	}
	dec, err := e.DecryptString("ciphertext")
	if err != nil {
		t.Fatalf("DecryptString: %v", err)
	}
	if dec != "ciphertext" {
		t.Errorf("expected passthrough, got %q", dec)
	}
}

func TestConstantTimeCompare(t *testing.T) {
	if !ConstantTimeCompare([]byte("a"), []byte("a")) {
		t.Error("equal slices should compare true")
	}
	if ConstantTimeCompare([]byte("a"), []byte("b")) {
		t.Error("unequal slices should compare false")
	}
}

func TestIsLikelyBase64(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"a", false},        // wrong length
		{"abcd", true},      // 4 chars
		{"ABCD1234==", false}, // length 10, not multiple of 4
		{"++++====", true},  // 8 chars, all base64
		{"hello world", false},
		{"0123456789ABCDEF", true}, // 16 chars hex
	}
	for _, c := range cases {
		if got := isLikelyBase64([]byte(c.in)); got != c.want {
			t.Errorf("isLikelyBase64(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCiphertextIsRandom(t *testing.T) {
	e, err := NewFromRawKey(testKey(t))
	if err != nil {
		t.Fatalf("NewFromRawKey: %v", err)
	}
	pt := []byte("same input")
	a, _ := e.Encrypt(pt)
	b, _ := e.Encrypt(pt)
	if bytes.Equal(a, b) {
		t.Error("two encryptions of the same plaintext produced identical ciphertext — IV not random?")
	}
}

// TestNewFromEnvKey ensures the env-var path constructs an encryptor
// equivalent to NewFromRawKey with the decoded key.
func TestNewFromEnvKey(t *testing.T) {
	raw := testKey(t)
	t.Setenv(EnvKey, base64.StdEncoding.EncodeToString(raw))

	fromEnv, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if fromEnv == nil {
		t.Fatal("expected non-nil encryptor from env")
	}

	fromRaw, err := NewFromRawKey(raw)
	if err != nil {
		t.Fatalf("NewFromRawKey: %v", err)
	}

	ct, _ := fromEnv.Encrypt([]byte("x"))
	if _, err := fromRaw.Decrypt(ct); err != nil {
		t.Errorf("cross-decrypt failed: %v", err)
	}
}

// Ensure env var reads work even when already set externally.
func TestEnvKeyConstant(t *testing.T) {
	if EnvKey != "KEY_VAULTS_SECRET" {
		t.Errorf("EnvKey = %q, want KEY_VAULTS_SECRET", EnvKey)
	}
	// suppress unused-var warning if EnvKey ever changes.
	_ = os.Getenv(EnvKey)
}
