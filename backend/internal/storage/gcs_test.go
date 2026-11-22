package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// A fake bucket: the token call, upload, download and delete, enough to prove the driver's requests and its
// not-found behaviour (Open of a missing file must satisfy os.ErrNotExist, Delete of one must not fail).
func TestGCS(t *testing.T) {
	objects := map[string]string{}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "no", 403)
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
	})
	mux.HandleFunc("/upload/storage/v1/b/bkt/o", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "auth", 401)
			return
		}
		b, _ := io.ReadAll(r.Body)
		objects[r.URL.Query().Get("name")] = string(b)
	})
	mux.HandleFunc("/storage/v1/b/bkt/o/", func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.EscapedPath(), "/storage/v1/b/bkt/o/")
		key = strings.ReplaceAll(key, "%2F", "/")
		v, ok := objects[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodDelete {
			delete(objects, key)
			return
		}
		_, _ = w.Write([]byte(v))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	g := &GCS{Bucket: "bkt", HTTP: srv.Client(), Metadata: srv.URL + "/token", API: srv.URL}
	ctx := context.Background()

	if err := g.Put(ctx, "docs/a.pdf", strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	r, err := g.Open(ctx, "docs/a.pdf")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	r.Close()
	if string(b) != "hello" {
		t.Fatalf("got %q", b)
	}
	if err := g.Delete(ctx, "docs/a.pdf"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Open(ctx, "docs/a.pdf"); !os.IsNotExist(err) {
		t.Fatalf("a missing file must be os.ErrNotExist, got %v", err)
	}
	if err := g.Delete(ctx, "docs/a.pdf"); err != nil {
		t.Fatalf("deleting a missing file must not fail: %v", err)
	}
	if err := g.Put(ctx, "../x", strings.NewReader("x")); err == nil {
		t.Fatal("a key with .. must be refused")
	}
}

func TestFromEnv(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if s, err := FromEnv(get); err != nil || s.(Local).Root != "data/uploads" {
		t.Fatal("default must be local data/uploads", s, err)
	}
	env["STORAGE_DRIVER"] = "gcs"
	if _, err := FromEnv(get); err == nil {
		t.Fatal("gcs without a bucket must fail")
	}
	env["GCS_BUCKET"] = "b"
	if s, err := FromEnv(get); err != nil || s.(*GCS).Bucket != "b" {
		t.Fatal(s, err)
	}
	env["STORAGE_DRIVER"] = "s3"
	if _, err := FromEnv(get); err == nil {
		t.Fatal("an unknown driver must fail")
	}
}
