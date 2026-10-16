package media

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdenesfe/wax/internal/backoff"
)

func TestRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, 15, 16, 17, 100000} {
		pt := make([]byte, n)
		rand.Read(pt)
		enc, err := Encrypt(pt, Image)
		if err != nil {
			t.Fatal(err)
		}
		if (len(enc.Data)-macLength)%16 != 0 || enc.FileLength != uint64(n) {
			t.Fatalf("bad layout for %d", n)
		}
		got, err := Decrypt(enc.Data, enc.MediaKey, Image, enc.FileEncSHA256, enc.FileSHA256)
		if err != nil || !bytes.Equal(got, pt) {
			t.Fatalf("n=%d: %v", n, err)
		}
		// The type is bound into the keys.
		if _, err := Decrypt(enc.Data, enc.MediaKey, Video, nil, nil); !errors.Is(err, ErrInvalidMAC) {
			t.Fatalf("wrong type accepted: %v", err)
		}
	}
}

func TestTamper(t *testing.T) {
	enc, _ := Encrypt([]byte("some file content"), Document)
	for _, pos := range []int{0, len(enc.Data) / 2, len(enc.Data) - 1} {
		bad := bytes.Clone(enc.Data)
		bad[pos] ^= 1
		if _, err := Decrypt(bad, enc.MediaKey, Document, enc.FileEncSHA256, enc.FileSHA256); !errors.Is(err, ErrHashMismatch) {
			t.Fatalf("pos %d: %v", pos, err)
		}
		if _, err := Decrypt(bad, enc.MediaKey, Document, nil, nil); !errors.Is(err, ErrInvalidMAC) {
			t.Fatalf("pos %d without hashes: %v", pos, err)
		}
	}
	wrongPlainHash := make([]byte, 32)
	if _, err := Decrypt(enc.Data, enc.MediaKey, Document, enc.FileEncSHA256, wrongPlainHash); !errors.Is(err, ErrHashMismatch) {
		t.Fatal("plaintext hash not checked")
	}
}

// TestKeyDerivationPinned checks the HKDF expansion against a value computed
// independently (RFC 5869 by hand with Python's hmac module), so a change of
// info string, salt or split is caught.
func TestKeyDerivationPinned(t *testing.T) {
	mk := bytes.Repeat([]byte{0x11}, 32)
	k, err := DeriveKeys(mk, Image)
	if err != nil {
		t.Fatal(err)
	}
	k2, _ := DeriveKeys(mk, Image)
	if k != k2 {
		t.Fatal("derivation not deterministic")
	}
	got := hex.EncodeToString(k.IV[:]) + hex.EncodeToString(k.CipherKey[:4])
	if got != "c3994d20d318005274c80ebc6e20b29b85948aea" {
		t.Fatalf("derivation changed: %s", got)
	}
}

func TestTransfer(t *testing.T) {
	var mu sync.Mutex
	store := map[string][]byte{}
	failures := 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			if r.URL.Query().Get("auth") != "secret" || r.Header.Get("Origin") != Origin {
				w.WriteHeader(401)
				return
			}
			b, _ := io.ReadAll(r.Body)
			path := "/v/t62.7118-24/" + strings.TrimPrefix(r.URL.Path, "/mms/image/")
			store[path+"?ccb=11"] = b
			w.Write([]byte(`{"url":"https://x` + path + `","direct_path":"` + path + `?ccb=11"}`))
		case http.MethodGet:
			if failures > 0 {
				failures--
				w.WriteHeader(503)
				return
			}
			key := r.URL.Path + "?ccb=11"
			b, ok := store[key]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Write(b)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	tr := &Transfer{Scheme: "http", Retry: backoff.Policy{Min: time.Millisecond, Max: time.Millisecond}}
	conn := &Conn{Auth: "secret", Hosts: []string{host}}
	enc, _ := Encrypt([]byte("picture bytes"), Image)
	res, err := tr.Upload(context.Background(), conn, enc, Image)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := tr.Download(context.Background(), conn, res.DirectPath, enc.FileEncSHA256, Image)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := Decrypt(blob, enc.MediaKey, Image, enc.FileEncSHA256, enc.FileSHA256)
	if err != nil || string(pt) != "picture bytes" {
		t.Fatalf("%q %v", pt, err)
	}
	if _, err := tr.Download(context.Background(), conn, "/v/missing?ccb=11", enc.FileEncSHA256, Image); err == nil {
		t.Fatal("expected 404 error")
	}
}

func TestErrorsDoNotLeakURL(t *testing.T) {
	err := withoutURL(&url.Error{Op: "Post", URL: "https://mmg.whatsapp.net/mms/image/x?auth=SECRET", Err: errors.New("connection refused")})
	if strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("error %q", err)
	}
}
