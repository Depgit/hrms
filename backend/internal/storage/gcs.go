package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// GCS keeps files in a private Google Cloud Storage bucket through the JSON API. On Cloud Run the access token comes
// from the metadata server, so the service account needs roles/storage.objectUser on the bucket and no key file is
// kept anywhere (the same way as Purchase Desk).
type GCS struct {
	Bucket string
	HTTP   *http.Client
	// Metadata and API are the token and storage addresses; tests point them at a local server.
	Metadata string
	API      string

	mu      sync.Mutex
	token   string
	expires time.Time
}

const (
	metadataTokenURL = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"
	storageAPI       = "https://storage.googleapis.com"
)

func NewGCS(bucket string) *GCS {
	return &GCS{Bucket: bucket, HTTP: &http.Client{Timeout: 5 * time.Minute}, Metadata: metadataTokenURL, API: storageAPI}
}

func (g *GCS) accessToken(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.token != "" && time.Now().Before(g.expires) {
		return g.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.Metadata, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	res, err := g.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("gcs token: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return "", fmt.Errorf("gcs token: %s", res.Status)
	}
	var t struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(res.Body).Decode(&t); err != nil {
		return "", fmt.Errorf("gcs token: %w", err)
	}
	g.token = t.AccessToken
	g.expires = time.Now().Add(time.Duration(t.ExpiresIn-60) * time.Second)
	return g.token, nil
}

func (g *GCS) do(ctx context.Context, method, u string, body io.Reader, contentType string) (*http.Response, error) {
	tok, err := g.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return g.HTTP.Do(req)
}

func badKey(key string) bool {
	return key == "" || strings.Contains(key, "..") || strings.HasPrefix(key, "/")
}

// Put writes the object, replacing one with the same key (a retry after a failed move to Drive writes it again).
func (g *GCS) Put(ctx context.Context, key string, r io.Reader) error {
	if badKey(key) {
		return errors.New("bad storage key")
	}
	q := url.Values{"uploadType": {"media"}, "name": {key}}
	res, err := g.do(ctx, http.MethodPost, fmt.Sprintf("%s/upload/storage/v1/b/%s/o?%s", g.API, url.PathEscape(g.Bucket), q.Encode()), r, "application/octet-stream")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return fmt.Errorf("gcs upload failed: %s %s", res.Status, b)
	}
	return nil
}

// Open returns the object, or an error wrapping os.ErrNotExist when there is none.
func (g *GCS) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if badKey(key) {
		return nil, errors.New("bad storage key")
	}
	res, err := g.do(ctx, http.MethodGet, fmt.Sprintf("%s/storage/v1/b/%s/o/%s?alt=media", g.API, url.PathEscape(g.Bucket), url.PathEscape(key)), nil, "")
	if err != nil {
		return nil, err
	}
	if res.StatusCode == http.StatusNotFound {
		res.Body.Close()
		return nil, os.ErrNotExist
	}
	if res.StatusCode >= 300 {
		res.Body.Close()
		return nil, fmt.Errorf("gcs download failed: %s", res.Status)
	}
	return res.Body, nil
}

// Delete removes the object. A missing object is not an error.
func (g *GCS) Delete(ctx context.Context, key string) error {
	if badKey(key) {
		return errors.New("bad storage key")
	}
	res, err := g.do(ctx, http.MethodDelete, fmt.Sprintf("%s/storage/v1/b/%s/o/%s", g.API, url.PathEscape(g.Bucket), url.PathEscape(key)), nil, "")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 && res.StatusCode != http.StatusNotFound {
		return fmt.Errorf("gcs delete failed: %s", res.Status)
	}
	return nil
}

// FromEnv picks the file store from STORAGE_DRIVER: "gcs" (needs GCS_BUCKET) or local disk under STORAGE_DIR
// (the default, development).
func FromEnv(get func(string) string) (Store, error) {
	switch d := strings.ToLower(strings.TrimSpace(get("STORAGE_DRIVER"))); d {
	case "", "local":
		dir := get("STORAGE_DIR")
		if dir == "" {
			dir = "data/uploads"
		}
		return Local{Root: dir}, nil
	case "gcs":
		b := strings.TrimSpace(get("GCS_BUCKET"))
		if b == "" {
			return nil, errors.New("STORAGE_DRIVER=gcs needs GCS_BUCKET")
		}
		return NewGCS(b), nil
	default:
		return nil, fmt.Errorf("unknown STORAGE_DRIVER %q (use local or gcs)", d)
	}
}
