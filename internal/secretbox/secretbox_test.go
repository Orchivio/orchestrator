package secretbox

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func newKey(t *testing.T, n int) string {
	t.Helper()
	k := make([]byte, n)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k)
}

func TestSealOpenRoundTrip(t *testing.T) {
	b, err := New(newKey(t, 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := b.Seal([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("secret")) {
		t.Fatal("ciphertext contains plaintext")
	}
	got, err := b.Open(b.KeyID(), sealed)
	if err != nil || string(got) != "secret" {
		t.Fatalf("got %q, %v", got, err)
	}
	if len(b.KeyID()) != 16 {
		t.Fatalf("key id = %q", b.KeyID())
	}
}

func TestOpenRejectsTamperAndWrongKeyID(t *testing.T) {
	b, _ := New(newKey(t, 32))
	sealed, _ := b.Seal([]byte("secret"))
	if _, err := b.Open("0000000000000000", sealed); err == nil {
		t.Fatal("wrong key id accepted")
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := b.Open(b.KeyID(), sealed); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	if _, err := b.Open(b.KeyID(), []byte{1, 2}); err == nil {
		t.Fatal("short ciphertext accepted")
	}
}

func TestNewRejectsWrongKeyLength(t *testing.T) {
	for _, key := range []string{newKey(t, 16), "not base64 !!", ""} {
		_, err := New(key)
		if err == nil {
			t.Fatalf("key %q accepted", key)
		}
		if key != "" && strings.Contains(err.Error(), key) {
			t.Fatalf("error echoes key: %v", err)
		}
	}
}
