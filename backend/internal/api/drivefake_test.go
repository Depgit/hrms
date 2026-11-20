package api_test

// A fake Google for the Drive tests: the OAuth token and revoke endpoints and the parts of Drive v3 the app uses
// (files get / list with q / create / update, multipart upload, alt=media download, about). Nothing here talks to
// the real Google.

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

type fakeFile struct {
	ID, Name, Mime string
	Parents        []string
	Trashed        bool
	ReadOnly       bool // the connected account may only view it
	Data           []byte
	App            map[string]string
}

type fakeGoogle struct {
	srv          *httptest.Server
	mu           sync.Mutex
	clientID     string
	clientSecret string
	email        string
	refresh      string          // the refresh token it issues
	access       map[string]bool // access tokens it accepts
	expiresIn    int
	revoked      bool
	down         bool // every Drive call answers 503
	badMD5       bool // uploads report a checksum that does not match
	files        map[string]*fakeFile
	next         int
	grants       []string // grant_type of every token call
	refreshSeen  []string // refresh tokens presented
	uploads      int
}

func newFakeGoogle() *fakeGoogle {
	g := &fakeGoogle{clientID: "test-client.apps.googleusercontent.com", clientSecret: "test-client-secret", email: "drive-admin@saltstayz.example",
		refresh: "1//rt-SECRET-refresh-token-do-not-leak", access: map[string]bool{}, expiresIn: 3600, files: map[string]*fakeFile{}}
	g.srv = httptest.NewServer(g)
	return g
}

func (g *fakeGoogle) Close() { g.srv.Close() }

// add puts a file or folder in the fake Drive and returns its id.
func (g *fakeGoogle) add(f *fakeFile) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.next++
	if f.ID == "" {
		f.ID = fmt.Sprintf("fake%08d0000id", g.next)
	}
	if f.App == nil {
		f.App = map[string]string{}
	}
	g.files[f.ID] = f
	return f.ID
}

// path is "Folder/Name" of a file (one level below the root), for checks.
func (g *fakeGoogle) path(id string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	f := g.files[id]
	if f == nil {
		return ""
	}
	if len(f.Parents) > 0 && g.files[f.Parents[0]] != nil {
		return g.files[f.Parents[0]].Name + "/" + f.Name
	}
	return f.Name
}

func (g *fakeGoogle) set(fn func(g *fakeGoogle)) { g.mu.Lock(); fn(g); g.mu.Unlock() }

func gErr(w http.ResponseWriter, code int, reason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": msg, "errors": []map[string]string{{"reason": reason, "message": msg}}}})
}

func (g *fakeGoogle) meta(f *fakeFile) map[string]any {
	sum := md5.Sum(f.Data)
	m := map[string]any{"id": f.ID, "name": f.Name, "mimeType": f.Mime, "trashed": f.Trashed, "parents": f.Parents,
		"capabilities": map[string]bool{"canAddChildren": f.Mime == "application/vnd.google-apps.folder" && !f.ReadOnly}}
	if f.Mime != "application/vnd.google-apps.folder" {
		m["md5Checksum"] = hex.EncodeToString(sum[:])
		m["size"] = strconv.Itoa(len(f.Data))
	}
	return m
}

var (
	qName    = regexp.MustCompile(`name = '((?:[^'\\]|\\.)*)'`)
	qParent  = regexp.MustCompile(`'((?:[^'\\]|\\.)*)' in parents`)
	qMime    = regexp.MustCompile(`mimeType = '((?:[^'\\]|\\.)*)'`)
	qApp     = regexp.MustCompile(`appProperties has \{ key='((?:[^'\\]|\\.)*)' and value='((?:[^'\\]|\\.)*)' \}`)
	unescape = strings.NewReplacer(`\'`, `'`, `\\`, `\`)
)

func (g *fakeGoogle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p := r.URL.Path
	switch {
	case p == "/o/oauth2/v2/auth": // the sign-in page: says yes at once
		u, _ := url.Parse(r.URL.Query().Get("redirect_uri"))
		q := u.Query()
		q.Set("code", "good")
		q.Set("state", r.URL.Query().Get("state"))
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
		return
	case p == "/token":
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.PostForm.Get("client_id") != g.clientID || r.PostForm.Get("client_secret") != g.clientSecret {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":"invalid_client"}`)
			return
		}
		gt := r.PostForm.Get("grant_type")
		g.grants = append(g.grants, gt)
		switch {
		case gt == "authorization_code" && r.PostForm.Get("code") == "good" && r.PostForm.Get("redirect_uri") != "":
			g.revoked = false
			at := fmt.Sprintf("ya29.access-%d", len(g.grants))
			g.access[at] = true
			claims, _ := json.Marshal(map[string]any{"email": g.email, "email_verified": true})
			idt := "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": at, "expires_in": g.expiresIn, "refresh_token": g.refresh, "token_type": "Bearer",
				"scope": "openid https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/drive", "id_token": idt})
		case gt == "refresh_token":
			g.refreshSeen = append(g.refreshSeen, r.PostForm.Get("refresh_token"))
			if r.PostForm.Get("refresh_token") != g.refresh || g.revoked {
				w.WriteHeader(400)
				_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`)
				return
			}
			at := fmt.Sprintf("ya29.access-%d", len(g.grants))
			g.access[at] = true
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": at, "expires_in": g.expiresIn, "token_type": "Bearer", "scope": "https://www.googleapis.com/auth/drive"})
		default:
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"Bad Request"}`)
		}
		return
	case p == "/revoke":
		_ = r.ParseForm()
		if r.PostForm.Get("token") == g.refresh {
			g.revoked = true
			g.access = map[string]bool{}
		}
		w.WriteHeader(200)
		return
	}
	if !g.access[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] {
		gErr(w, 401, "authError", "Invalid Credentials")
		return
	}
	if g.down {
		gErr(w, 503, "backendError", "Backend Error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	switch {
	case p == "/drive/v3/about":
		_ = enc.Encode(map[string]any{"user": map[string]string{"emailAddress": g.email}})
	case p == "/drive/v3/files" && r.Method == "GET":
		q := r.URL.Query().Get("q")
		out := []map[string]any{}
		for _, f := range g.files {
			if m := qName.FindStringSubmatch(q); m != nil && f.Name != unescape.Replace(m[1]) {
				continue
			}
			if m := qParent.FindStringSubmatch(q); m != nil && (len(f.Parents) == 0 || f.Parents[0] != unescape.Replace(m[1])) {
				continue
			}
			if m := qMime.FindStringSubmatch(q); m != nil && f.Mime != unescape.Replace(m[1]) {
				continue
			}
			if m := qApp.FindStringSubmatch(q); m != nil && f.App[unescape.Replace(m[1])] != unescape.Replace(m[2]) {
				continue
			}
			if strings.Contains(q, "trashed = false") && f.Trashed {
				continue
			}
			out = append(out, g.meta(f))
		}
		_ = enc.Encode(map[string]any{"files": out})
	case p == "/drive/v3/files" && r.Method == "POST":
		var m struct {
			Name, MimeType string
			Parents        []string
		}
		_ = json.NewDecoder(r.Body).Decode(&m)
		if len(m.Parents) != 1 || g.files[m.Parents[0]] == nil {
			gErr(w, 404, "notFound", "File not found: parent.")
			return
		}
		g.next++
		f := &fakeFile{ID: fmt.Sprintf("fake%08d0000id", g.next), Name: m.Name, Mime: m.MimeType, Parents: m.Parents, App: map[string]string{}}
		g.files[f.ID] = f
		_ = enc.Encode(g.meta(f))
	case strings.HasPrefix(p, "/drive/v3/files/"):
		f := g.files[strings.TrimPrefix(p, "/drive/v3/files/")]
		if f == nil {
			gErr(w, 404, "notFound", "File not found.")
			return
		}
		switch r.Method {
		case "GET":
			if r.URL.Query().Get("alt") == "media" {
				w.Header().Set("Content-Type", f.Mime)
				_, _ = w.Write(f.Data)
				return
			}
			_ = enc.Encode(g.meta(f))
		case "PATCH":
			var m struct{ Name string }
			_ = json.NewDecoder(r.Body).Decode(&m)
			if m.Name != "" {
				f.Name = m.Name
			}
			_ = enc.Encode(g.meta(f))
		}
	case p == "/upload/drive/v3/files" && r.Method == "POST" && r.URL.Query().Get("uploadType") == "multipart":
		mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mt != "multipart/related" {
			gErr(w, 400, "badContent", "multipart/related expected")
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		part, err := mr.NextPart()
		if err != nil {
			gErr(w, 400, "badContent", "no metadata")
			return
		}
		var m struct {
			Name, MimeType string
			Parents        []string
			AppProperties  map[string]string
		}
		_ = json.NewDecoder(part).Decode(&m)
		part, err = mr.NextPart()
		if err != nil {
			gErr(w, 400, "badContent", "no media")
			return
		}
		data, _ := io.ReadAll(part)
		if len(m.Parents) != 1 || g.files[m.Parents[0]] == nil || g.files[m.Parents[0]].Trashed {
			gErr(w, 404, "notFound", "File not found: parent.")
			return
		}
		g.next++
		g.uploads++
		f := &fakeFile{ID: fmt.Sprintf("fake%08d0000id", g.next), Name: m.Name, Mime: m.MimeType, Parents: m.Parents, Data: data, App: m.AppProperties}
		g.files[f.ID] = f
		out := g.meta(f)
		if g.badMD5 {
			out["md5Checksum"] = "00000000000000000000000000000000"
		}
		_ = enc.Encode(out)
	default:
		gErr(w, 404, "notFound", "Not found: "+p)
	}
}
