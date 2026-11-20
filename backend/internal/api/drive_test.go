package api_test

// Google Drive document storage (drive.go) against a fake Google (drivefake_test.go): connecting with OAuth, the
// folder link, where each file lands and what it is called, replaced files, the staged copy, Drive being down,
// opening a file from Drive, moving existing files, who may see the settings, and the token never leaking.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/api"
	"github.com/saltstayz/hrms/internal/storage"
	"github.com/saltstayz/hrms/internal/store"
)

type driveEnv struct {
	c   *client
	app *api.Server
	st  *store.Store
	g   *fakeGoogle
	dir string        // the staging folder (STORAGE_DIR)
	log *bytes.Buffer // everything the server logged
}

// setupDrive is setupFull with the OAuth client set, Google pointed at the fake, and staging on a real folder.
func setupDrive(t *testing.T) *driveEnv {
	g := newFakeGoogle()
	t.Cleanup(g.Close)
	t.Setenv("GOOGLE_CLIENT_ID", g.clientID)
	t.Setenv("GOOGLE_CLIENT_SECRET", g.clientSecret)
	t.Setenv("GOOGLE_BASE_URL", g.srv.URL)
	t.Setenv("GOOGLE_REDIRECT_URL", "")
	t.Setenv("APP_SECRET", "")
	c, _, st := setupFull(t)
	var buf bytes.Buffer
	log.SetOutput(io.MultiWriter(os.Stderr, &buf))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	dir := t.TempDir()
	app := api.New(st, api.Options{DevOTP: true, Files: storage.Local{Root: dir}})
	srv := httptest.NewServer(app.Routes())
	t.Cleanup(srv.Close)
	c.base = srv.URL
	c.signIn("EMP-1002")
	return &driveEnv{c: c, app: app, st: st, g: g, dir: dir, log: &buf}
}

// req sends a request with extra headers and no redirect following; it returns the status, headers and body.
func (e *driveEnv) req(method, path, emp, role string, body any, hdr map[string]string) (int, http.Header, []byte) {
	e.c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r, _ := http.NewRequest(method, e.c.base+path, rd)
	if emp != "" {
		r.Header.Set("Authorization", "Bearer "+e.c.tokens[emp])
		r.Header.Set("X-Role", role)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := hc.Do(r)
	if err != nil {
		e.c.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, b
}

func (e *driveEnv) status(t *testing.T) map[string]any {
	t.Helper()
	s, b := e.c.do("GET", "/api/drive", "EMP-1002", "HR Admin", nil)
	if s != 200 {
		t.Fatalf("drive status %d %v", s, b)
	}
	return b
}

// connect runs the sign-in as the browser would: Connect, Google's page (the fake says yes), then the callback.
func (e *driveEnv) connect(t *testing.T) {
	t.Helper()
	s, h, b := e.req("POST", "/api/drive/connect", "EMP-1002", "HR Admin", nil, map[string]string{"Origin": "http://localhost:5173"})
	if s != 200 {
		t.Fatalf("connect %d %s", s, b)
	}
	var out struct{ URL string }
	_ = json.Unmarshal(b, &out)
	u, _ := url.Parse(out.URL)
	q := u.Query()
	if !strings.HasPrefix(out.URL, e.g.srv.URL+"/o/oauth2/v2/auth?") || q.Get("access_type") != "offline" || q.Get("prompt") != "consent" ||
		q.Get("redirect_uri") != "http://localhost:5173/api/drive/callback" || q.Get("client_id") != e.g.clientID ||
		q.Get("scope") != "https://www.googleapis.com/auth/drive openid email" || q.Get("state") == "" {
		t.Fatalf("Google sign-in address %s", out.URL)
	}
	cookie := cookieOf(h)
	s, h, b = e.req("GET", "/api/drive/callback?"+url.Values{"code": {"good"}, "state": {q.Get("state")}}.Encode(), "", "", nil, map[string]string{"Cookie": cookie})
	if s != 303 || h.Get("Location") != "http://localhost:5173/masters?tab=company&drive=connected" {
		t.Fatalf("callback %d %s %s", s, h.Get("Location"), b)
	}
}

func cookieOf(h http.Header) string {
	for _, c := range (&http.Response{Header: h}).Cookies() {
		if c.Name == "hrms_drive_oauth" {
			return c.Name + "=" + c.Value
		}
	}
	return ""
}

// folder makes a folder in the fake Drive and sets it as the place for documents.
func (e *driveEnv) folder(t *testing.T) string {
	t.Helper()
	root := e.g.add(&fakeFile{Name: "HRMS documents", Mime: "application/vnd.google-apps.folder"})
	s, b := e.c.do("PUT", "/api/drive/folder", "EMP-1002", "HR Admin", map[string]string{"link": "https://drive.google.com/drive/u/0/folders/" + root + "?usp=sharing"})
	if s != 200 || b["folder"].(map[string]any)["name"] != "HRMS documents" {
		t.Fatalf("folder %d %v", s, b)
	}
	return root
}

// stage puts a document on the server as an upload would (for owners whose upload rights live elsewhere).
func (e *driveEnv) stage(t *testing.T, kind, owner, emp, key, state string, data []byte) string {
	t.Helper()
	skey := kind + "/" + strings.NewReplacer("/", "_").Replace(owner) + "/" + key + "-" + time.Now().Format("150405.000000000") + ".pdf"
	if err := (storage.Local{Root: e.dir}).Put(context.Background(), skey, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	var empID any // NULL when none
	if emp != "" {
		empID = emp
	}
	id, err := e.st.DB.C("document").Insert(context.Background(), bson.D{{Key: "owner_kind", Value: kind}, {Key: "owner_id", Value: owner}, {Key: "employee_id", Value: empID},
		{Key: "doc_key", Value: key}, {Key: "file_name", Value: key + ".pdf"}, {Key: "content_type", Value: "application/pdf"}, {Key: "size_bytes", Value: len(data)},
		{Key: "storage_key", Value: skey}, {Key: "uploaded_by", Value: "test"}, {Key: "drive_state", Value: state}})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(id)
}

// addCandidate inserts a candidate row directly (onboarding stages owns the table): id, no and employee_id, with
// any other required column (NOT NULL, no default, from the schema registry) filled from the sample organisation.
func (e *driveEnv) addCandidate(t *testing.T, id string, no int, emp string) {
	t.Helper()
	ctx := context.Background()
	tbl := store.TableOf("candidate")
	var empID any // NULL when none
	if emp != "" {
		empID = emp
	}
	doc := bson.D{{Key: "_id", Value: id}, {Key: "no", Value: no}, {Key: "employee_id", Value: empID}}
	for _, col := range tbl.Columns {
		if !col.NotNull || col.Default.Kind != store.DefNone || col.Name == "id" || col.Name == "no" || col.Name == "employee_id" {
			continue
		}
		var v any = "Test candidate"
		fk := false
		for _, k := range tbl.FKs {
			if k.Col == col.Name { // the first row of the referenced table (ORDER BY the referenced column)
				fk = true
				ref, err := e.st.DB.C(k.RefTable).FindOne(ctx, bson.D{}, options.FindOne().SetSort(bson.D{{Key: k.RefCol, Value: 1}}))
				if err != nil || ref == nil {
					t.Fatalf("no %s row for candidate.%s: %v", k.RefTable, col.Name, err)
				}
				v = ref[k.RefCol]
			}
		}
		switch {
		case fk:
		case col.Array:
			v = []any{}
		case col.Type == store.TNumeric || col.Type == store.TInt2 || col.Type == store.TInt4 || col.Type == store.TInt8:
			v = 0
		case col.Type == store.TBool:
			v = false
		case col.Type == store.TDate:
			v = store.Today()
		case col.Type == store.TTimestamptz:
			v = time.Now()
		case col.Type == store.TJSONB:
			v = map[string]any{}
		case col.Type == store.TUUID:
			v = store.NewID()
		}
		doc = append(doc, bson.E{Key: col.Name, Value: v})
	}
	if _, err := e.st.DB.C("candidate").Insert(ctx, doc); err != nil {
		t.Fatal(err)
	}
}

func (e *driveEnv) doc(t *testing.T, id string) (state, fileID, name, skey string) {
	t.Helper()
	d, err := e.st.DB.C("document").FindOne(context.Background(), bson.D{{Key: "_id", Value: id}})
	if err == nil && d == nil {
		err = store.ErrNoRows
	}
	if err != nil {
		t.Fatal(err)
	}
	state, _ = d["drive_state"].(string)
	fileID, _ = d["drive_file_id"].(string) // '' when NULL
	name, _ = d["drive_name"].(string)
	skey, _ = d["storage_key"].(string)
	return
}

func (e *driveEnv) staged(skey string) bool {
	_, err := os.Stat(filepath.Join(e.dir, skey))
	return err == nil
}

func (e *driveEnv) sync(t *testing.T) {
	t.Helper()
	if _, err := e.app.SyncDrive(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
}

// uploadFile uploads through the API with chosen content.
func (e *driveEnv) uploadFile(emp, role, kind, owner, key string, content []byte) (int, map[string]any) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("ownerKind", kind)
	_ = w.WriteField("ownerId", owner)
	_ = w.WriteField("docKey", key)
	f, _ := w.CreateFormFile("file", "scan.pdf")
	_, _ = f.Write(content)
	_ = w.Close()
	r, _ := http.NewRequest("POST", e.c.base+"/api/files", &buf)
	r.Header.Set("Content-Type", w.FormDataContentType())
	r.Header.Set("Authorization", "Bearer "+e.c.tokens[emp])
	r.Header.Set("X-Role", role)
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		e.c.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func pdf(s string) []byte {
	return []byte("%PDF-1.4\n% " + s + "\n1 0 obj << >> endobj\ntrailer << >>\n%%EOF\n")
}

func TestDriveConnect(t *testing.T) {
	e := setupDrive(t)
	c := e.c
	ctx := context.Background()
	c.signIn("EMP-1004")
	c.signIn("EMP-1020")

	t.Run("only HR Admin sees or changes the settings", func(t *testing.T) {
		for _, x := range [][2]string{{"EMP-1004", "HRBP"}, {"EMP-1020", "Employee"}, {"EMP-1002", "Employee"}} {
			for _, r := range [][2]string{{"GET", "/api/drive"}, {"POST", "/api/drive/connect"}, {"PUT", "/api/drive/folder"}, {"POST", "/api/drive/disconnect"},
				{"POST", "/api/drive/move-existing"}, {"POST", "/api/drive/sync"}} {
				if s, _, b := e.req(r[0], r[1], x[0], x[1], map[string]string{"link": "x"}, nil); s != 403 {
					t.Fatalf("%s as %s: %s %s gave %d %s", x[0], x[1], r[0], r[1], s, b)
				}
			}
		}
	})

	t.Run("not connected: setup details and the redirect URI", func(t *testing.T) {
		st := e.status(t)
		if st["configured"] != true || st["connected"] != false || st["folder"] != nil || st["waiting"] != float64(0) {
			t.Fatal(st)
		}
		// Behind the Vite proxy the browser's address is the web app's; directly it is the API's.
		_, _, b := e.req("GET", "/api/drive", "EMP-1002", "HR Admin", nil, map[string]string{"Referer": "http://localhost:5173/masters?tab=company"})
		if !strings.Contains(string(b), `"redirectUri":"http://localhost:5173/api/drive/callback"`) {
			t.Fatalf("behind the proxy: %s", b)
		}
		_, _, b = e.req("GET", "/api/drive", "EMP-1002", "HR Admin", nil, nil)
		if !strings.Contains(string(b), `"redirectUri":"`+c.base+`/api/drive/callback"`) {
			t.Fatalf("direct: %s", b)
		}
		if s, b := c.do("PUT", "/api/drive/folder", "EMP-1002", "HR Admin", map[string]string{"link": "https://drive.google.com/drive/folders/abcdefghijk123"}); s != 400 || !strings.Contains(b["error"].(string), "Connect Google Drive first") {
			t.Fatal(s, b)
		}
	})

	t.Run("callback with a bad state is refused", func(t *testing.T) {
		s, h, b := e.req("POST", "/api/drive/connect", "EMP-1002", "HR Admin", nil, map[string]string{"Origin": "http://localhost:5173"})
		if s != 200 {
			t.Fatal(s, string(b))
		}
		var out struct{ URL string }
		_ = json.Unmarshal(b, &out)
		u, _ := url.Parse(out.URL)
		state, cookie := u.Query().Get("state"), cookieOf(h)
		if cookie == "" || !strings.Contains(h.Get("Set-Cookie"), "HttpOnly") {
			t.Fatalf("no state cookie: %v", h)
		}
		tampered := state[:len(state)-3] + "AAA"
		// Another person in the payload, the same signature.
		payload, sig, _ := strings.Cut(state, ".")
		raw, _ := base64.RawURLEncoding.DecodeString(payload)
		forged := base64.RawURLEncoding.EncodeToString(bytes.Replace(raw, []byte("EMP-1002"), []byte("EMP-1004"), 1)) + "." + sig
		if forged == state {
			t.Fatal("forged state is the same")
		}
		for _, bad := range []string{"", "nonsense", tampered, forged} {
			s, _, b := e.req("GET", "/api/drive/callback?"+url.Values{"code": {"good"}, "state": {bad}}.Encode(), "", "", nil, map[string]string{"Cookie": cookie})
			if s != 400 || !strings.Contains(string(b), "not valid or has expired") {
				t.Fatalf("bad state %q gave %d %s", bad, s, b)
			}
		}
		// A good state from another browser (no cookie) is refused too.
		s, h, _ = e.req("GET", "/api/drive/callback?"+url.Values{"code": {"good"}, "state": {state}}.Encode(), "", "", nil, nil)
		if s != 303 || !strings.Contains(h.Get("Location"), "drive=error") {
			t.Fatal(s, h.Get("Location"))
		}
		// Cancelled at Google.
		s, h, _ = e.req("GET", "/api/drive/callback?"+url.Values{"error": {"access_denied"}, "state": {state}}.Encode(), "", "", nil, map[string]string{"Cookie": cookie})
		if s != 303 || !strings.Contains(h.Get("Location"), "drive=error&msg=Google+sign-in+was+cancelled") {
			t.Fatal(s, h.Get("Location"))
		}
		if st := e.status(t); st["connected"] != false {
			t.Fatal("a refused callback must not connect", st)
		}
	})

	t.Run("callback with a good state connects", func(t *testing.T) {
		e.connect(t)
		st := e.status(t)
		if st["connected"] != true || st["email"] != "drive-admin@saltstayz.example" || !strings.Contains(st["connectedBy"].(string), "EMP-1002") {
			t.Fatal(st)
		}
		var enc []byte
		if s, _ := e.st.DB.C("drive_setting").FindOne(ctx, bson.D{}); s != nil {
			enc, _ = s["refresh_token"].([]byte)
		}
		if len(enc) == 0 || bytes.Contains(enc, []byte(e.g.refresh)) {
			t.Fatal("the refresh token must be stored encrypted")
		}
	})

	t.Run("the folder link", func(t *testing.T) {
		root := e.g.add(&fakeFile{Name: "Saltstayz HR documents", Mime: "application/vnd.google-apps.folder"})
		ro := e.g.add(&fakeFile{Name: "View only", Mime: "application/vnd.google-apps.folder", ReadOnly: true})
		file := e.g.add(&fakeFile{Name: "notes.pdf", Mime: "application/pdf", Data: pdf("x")})
		for link, want := range map[string]string{
			"not a link": "Paste the link of a Google Drive folder",
			"https://example.com/drive/folders/" + root:                     "Paste the link of a Google Drive folder",
			"https://drive.google.com/drive/folders/" + ro:                  "can only view this folder",
			"https://drive.google.com/file/d/" + file + "/view?usp=sharing": "Paste the link of a Google Drive folder",
			file: "This link is to a file, not a folder",
			"https://drive.google.com/drive/folders/doesNotExist123": "No folder with this link is visible to drive-admin@saltstayz.example",
		} {
			if s, b := c.do("PUT", "/api/drive/folder", "EMP-1002", "HR Admin", map[string]string{"link": link}); s != 400 || !strings.Contains(b["error"].(string), want) {
				t.Fatalf("%s: %d %v", link, s, b)
			}
		}
		for _, link := range []string{"https://drive.google.com/drive/folders/" + root, "https://drive.google.com/drive/u/1/folders/" + root + "?usp=sharing",
			"https://drive.google.com/open?id=" + root, "drive.google.com/drive/folders/" + root + "?resourcekey=0-abc", root} {
			s, b := c.do("PUT", "/api/drive/folder", "EMP-1002", "HR Admin", map[string]string{"link": link})
			if s != 200 || b["folder"].(map[string]any)["name"] != "Saltstayz HR documents" || b["folder"].(map[string]any)["id"] != root {
				t.Fatalf("%s: %d %v", link, s, b)
			}
		}
	})

	t.Run("token refresh", func(t *testing.T) {
		e.g.set(func(g *fakeGoogle) { g.access = map[string]bool{} }) // every access token so far has expired
		root := e.g.add(&fakeFile{Name: "After refresh", Mime: "application/vnd.google-apps.folder"})
		if s, b := c.do("PUT", "/api/drive/folder", "EMP-1002", "HR Admin", map[string]string{"link": root}); s != 200 {
			t.Fatal(s, b)
		}
		e.g.mu.Lock()
		seen := append([]string(nil), e.g.refreshSeen...)
		e.g.mu.Unlock()
		if len(seen) == 0 || seen[len(seen)-1] != e.g.refresh {
			t.Fatalf("the stored refresh token was not used: %v", seen)
		}
	})

	t.Run("disconnect", func(t *testing.T) {
		s, b := c.do("POST", "/api/drive/disconnect", "EMP-1002", "HR Admin", nil)
		if s != 200 || b["connected"] != false || b["email"] != "" || b["folder"] == nil {
			t.Fatal(s, b)
		}
		if !e.g.revoked {
			t.Fatal("disconnect should revoke the token at Google")
		}
		n, _ := e.st.DB.C("drive_setting").Count(ctx, bson.D{{Key: "refresh_token", Value: bson.D{{Key: "$ne", Value: nil}}}})
		if n != 0 {
			t.Fatal("token kept after disconnect")
		}
	})

	t.Run("the token never appears in a response, the audit log or the server log", func(t *testing.T) {
		e.connect(t)
		var all strings.Builder
		for _, p := range [][2]string{{"GET", "/api/drive"}, {"POST", "/api/drive/sync"}, {"GET", "/api/audit"}} {
			_, h, b := e.req(p[0], p[1], "EMP-1002", "HR Admin", nil, nil)
			all.Write(b)
			for k, v := range h {
				all.WriteString(k + strings.Join(v, ""))
			}
		}
		// Every audit entry as text: the action, then its before and after (as JSON), space-separated.
		entries, _ := e.st.DB.C("audit_log").Find(ctx, bson.D{})
		texts := []string{}
		for _, a := range entries {
			s := fmt.Sprint(a["action"])
			for _, k := range []string{"before", "after"} {
				if a[k] != nil {
					b, _ := json.Marshal(a[k])
					s += string(b)
				}
			}
			texts = append(texts, s)
		}
		audit := strings.Join(texts, " ")
		all.WriteString(audit)
		all.WriteString(e.log.String())
		for _, secret := range []string{e.g.refresh, "rt-SECRET", "ya29.", e.g.clientSecret} {
			if strings.Contains(all.String(), secret) {
				t.Fatalf("%q leaked", secret)
			}
		}
		for _, want := range []string{"Google Drive connected", "Google Drive folder set", "Google Drive disconnected"} {
			if !strings.Contains(audit, want) {
				t.Fatalf("audit log has no %q: %s", want, audit)
			}
		}
	})
}

func TestDriveUploads(t *testing.T) {
	e := setupDrive(t)
	c := e.c
	ctx := context.Background()
	for _, x := range []string{"EMP-1020", "EMP-1014", "EMP-1015"} {
		c.signIn(x)
	}
	// Uploaded before Drive is connected: waits on the server.
	s, up := e.uploadFile("EMP-1020", "Employee", "employee", "EMP-1020", "Claim bill", pdf("bill"))
	if s != 200 {
		t.Fatal(s, up)
	}
	early := up["id"].(string)
	if st, fid, _, skey := e.doc(t, early); st != "waiting" || fid != "" || !e.staged(skey) {
		t.Fatal("before Drive is connected the file waits on the server", st, fid)
	}
	if st := e.status(t); st["waiting"] != float64(1) || st["existing"] != float64(0) {
		t.Fatal(st)
	}
	e.connect(t)
	root := e.folder(t)
	e.sync(t)
	_, fid, _, skey := e.doc(t, early)
	if e.g.path(fid) != "EMP-1020/EMP-1020 Claim bill.pdf" || e.staged(skey) {
		t.Fatalf("waiting file after connecting: %q staged %v", e.g.path(fid), e.staged(skey))
	}

	t.Run("a candidate's documents: SI-0001/SI-0001 Aadhaar.pdf", func(t *testing.T) {
		e.addCandidate(t, "SI-0001", 1, "")
		// The label comes from Onboarding setup: renamed there to "Aadhaar", PAN keeps "PAN card".
		if _, err := e.st.DB.C("onb_document").UpdateMany(ctx, bson.D{{Key: "key", Value: "aadhaar"}}, bson.D{{Key: "$set", Value: bson.D{{Key: "label", Value: "Aadhaar"}}}}); err != nil {
			t.Fatal(err)
		}
		a := e.stage(t, "candidate", "SI-0001", "", "aadhaar", "waiting", pdf("aadhaar"))
		p := e.stage(t, "candidate", "SI-0001", "", "pan", "waiting", pdf("pan"))
		e.sync(t)
		for id, want := range map[string]string{a: "SI-0001/SI-0001 Aadhaar.pdf", p: "SI-0001/SI-0001 PAN card.pdf"} {
			st, fid, name, skey := e.doc(t, id)
			if st != "drive" || e.g.path(fid) != want || !strings.HasSuffix(want, "/"+name) || e.staged(skey) {
				t.Fatalf("got %s %q (%s), staged %v; want %s", st, e.g.path(fid), name, e.staged(skey), want)
			}
		}
		e.g.mu.Lock()
		folders := 0
		for _, f := range e.g.files {
			if f.Name == "SI-0001" && f.Parents[0] == root {
				folders++
			}
		}
		e.g.mu.Unlock()
		if folders != 1 {
			t.Fatalf("SI-0001 made %d times", folders)
		}
	})

	t.Run("an employee who came through a candidate keeps the SI folder", func(t *testing.T) {
		e.addCandidate(t, "SI-0002", 2, "EMP-1014")
		s, b := e.uploadFile("EMP-1002", "HR Admin", "employee", "EMP-1014", "pan", pdf("pan 1014"))
		if s != 200 {
			t.Fatal(s, b)
		}
		e.sync(t)
		if _, fid, _, _ := e.doc(t, b["id"].(string)); e.g.path(fid) != "SI-0002/SI-0002 PAN card.pdf" {
			t.Fatal(e.g.path(fid))
		}
	})

	t.Run("an employee without a candidate uses the Employee ID; company files go to Company", func(t *testing.T) {
		s, b := e.uploadFile("EMP-1002", "HR Admin", "employee", "EMP-1020", "pan", pdf("pan 1020"))
		if s != 200 {
			t.Fatal(s, b)
		}
		var logo bytes.Buffer
		logo.Write([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\x0aIDATx\x9cc\x00\x01\x00\x00\x05\x00\x01\x0d\x0a\x2d\xb4\x00\x00\x00\x00IEND\xaeB`\x82"))
		s2, l := e.uploadFile("EMP-1002", "HR Admin", "company", "company", "Payslip logo", logo.Bytes())
		if s2 != 200 {
			t.Fatal(s2, l)
		}
		legacy := e.stage(t, "offer", "OFR-1999", "", "photo", "waiting", pdf("photo"))
		e.sync(t)
		if _, fid, _, _ := e.doc(t, b["id"].(string)); e.g.path(fid) != "EMP-1020/EMP-1020 PAN card.pdf" {
			t.Fatal(e.g.path(fid))
		}
		if _, fid, _, _ := e.doc(t, l["id"].(string)); e.g.path(fid) != "Company/Payslip logo.png" {
			t.Fatal(e.g.path(fid))
		}
		if _, fid, _, _ := e.doc(t, legacy); e.g.path(fid) != "OFR-1999/OFR-1999 Passport-size photo.pdf" {
			t.Fatal(e.g.path(fid))
		}
	})

	t.Run("a replaced file is renamed, the current one keeps the clean name", func(t *testing.T) {
		_, b1 := e.uploadFile("EMP-1020", "Employee", "employee", "EMP-1020", "Cancelled cheque", pdf("cheque 1"))
		e.sync(t)
		_, b2 := e.uploadFile("EMP-1020", "Employee", "employee", "EMP-1020", "Cancelled cheque", pdf("cheque 2"))
		e.sync(t)
		_, f1, n1, _ := e.doc(t, b1["id"].(string))
		_, f2, _, _ := e.doc(t, b2["id"].(string))
		day := time.Now().In(mustZone()).Format("02 Jan 2006")
		if e.g.path(f1) != "EMP-1020/EMP-1020 Cancelled cheque (replaced "+day+").pdf" || n1 != "EMP-1020 Cancelled cheque (replaced "+day+").pdf" ||
			e.g.path(f2) != "EMP-1020/EMP-1020 Cancelled cheque.pdf" {
			t.Fatalf("old %q (%q), new %q", e.g.path(f1), n1, e.g.path(f2))
		}
		// Nothing is deleted in Drive.
		if e.g.files[f1] == nil || e.g.files[f1].Trashed {
			t.Fatal("the replaced file must stay in Drive")
		}
	})

	t.Run("the staged copy is deleted only after a confirmed upload", func(t *testing.T) {
		e.g.set(func(g *fakeGoogle) { g.badMD5 = true })
		_, b := e.uploadFile("EMP-1020", "Employee", "employee", "EMP-1020", "Tax proof", pdf("tax"))
		_, _ = e.app.SyncDrive(ctx)
		st, fid, _, skey := e.doc(t, b["id"].(string))
		if st != "waiting" || fid != "" || !e.staged(skey) {
			t.Fatalf("unconfirmed upload: %s %q staged %v", st, fid, e.staged(skey))
		}
		if st := e.status(t); !strings.Contains(st["lastError"].(string), "did not confirm") {
			t.Fatal(st)
		}
		e.g.set(func(g *fakeGoogle) { g.badMD5 = false })
		if s, b := c.do("POST", "/api/drive/sync", "EMP-1002", "HR Admin", nil); s != 200 || b["waiting"] != float64(0) {
			t.Fatal(s, b)
		}
		if st, fid, _, skey := e.doc(t, b["id"].(string)); st != "drive" || fid == "" || e.staged(skey) {
			t.Fatalf("after a confirmed upload: %s %q staged %v", st, fid, e.staged(skey))
		}
	})

	t.Run("Drive down: the upload still works, the file waits, then goes up", func(t *testing.T) {
		e.g.set(func(g *fakeGoogle) { g.down = true })
		t0 := time.Now()
		s, b := e.uploadFile("EMP-1002", "HR Admin", "employee", "EMP-1020", "photo", pdf("photo"))
		if s != 200 || time.Since(t0) > 3*time.Second {
			t.Fatal("upload while Drive is down", s, b)
		}
		if _, err := e.app.SyncDrive(ctx); err == nil {
			t.Fatal("sync should report Drive down")
		}
		id := b["id"].(string)
		st, _, _, skey := e.doc(t, id)
		status := e.status(t)
		if st != "waiting" || !e.staged(skey) || status["waiting"] != float64(1) || !strings.Contains(status["lastError"].(string), "not answering") {
			t.Fatal(st, e.staged(skey), status)
		}
		// Still opens, from the server.
		if s, body := c.getRaw("/api/files/"+id, "EMP-1020", "Employee"); s != 200 || !bytes.Equal(body, pdf("photo")) {
			t.Fatal("open while waiting", s)
		}
		e.g.set(func(g *fakeGoogle) { g.down = false })
		if s, b := c.do("POST", "/api/drive/sync", "EMP-1002", "HR Admin", nil); s != 200 || b["waiting"] != float64(0) {
			t.Fatal(s, b)
		}
		if st, fid, _, skey := e.doc(t, id); st != "drive" || e.g.path(fid) != "EMP-1020/EMP-1020 Passport-size photo.pdf" || e.staged(skey) {
			t.Fatal(st, e.g.path(fid))
		}
	})

	t.Run("opening a Drive file streams it with the usual access checks", func(t *testing.T) {
		_, b := e.uploadFile("EMP-1002", "HR Admin", "employee", "EMP-1020", "bankProof", pdf("bank proof"))
		e.sync(t)
		id := b["id"].(string)
		_, _, _, skey := e.doc(t, id)
		if e.staged(skey) {
			t.Fatal("should be in Drive only")
		}
		if s, body := c.getRaw("/api/files/"+id, "EMP-1020", "Employee"); s != 200 || !bytes.Equal(body, pdf("bank proof")) {
			t.Fatal("owner", s, string(body))
		}
		if s, body := c.getRaw("/api/files/"+id, "EMP-1002", "HR Admin"); s != 200 || !bytes.Equal(body, pdf("bank proof")) {
			t.Fatal("HR Admin", s)
		}
		for _, x := range []string{"EMP-1014", "EMP-1015"} {
			if s, _ := c.getRaw("/api/files/"+id, x, "Employee"); s != 403 {
				t.Fatalf("%s opened someone else's document from Drive: %d", x, s)
			}
		}
		if s, _ := c.getRaw("/api/files/"+id, "", ""); s != 401 {
			t.Fatal("no session", s)
		}
	})

	t.Run("a folder deleted in Drive is made again", func(t *testing.T) {
		var old string
		if f, _ := e.st.DB.C("drive_folder").FindOne(ctx, bson.D{{Key: "name", Value: "EMP-1020"}}); f != nil {
			old, _ = f["folder_id"].(string)
		}
		e.g.set(func(g *fakeGoogle) { g.files[old].Trashed = true })
		_, b := e.uploadFile("EMP-1002", "HR Admin", "employee", "EMP-1020", "addr", pdf("address"))
		e.sync(t)
		_, fid, _, _ := e.doc(t, b["id"].(string))
		e.g.mu.Lock()
		parent := e.g.files[fid].Parents[0]
		e.g.mu.Unlock()
		if parent == old || e.g.path(fid) != "EMP-1020/EMP-1020 Address proof.pdf" {
			t.Fatal("not uploaded into a new EMP-1020 folder", parent, e.g.path(fid))
		}
	})

	t.Run("disconnected: uploads wait, Drive files ask to reconnect", func(t *testing.T) {
		var inDrive string
		if d, _ := e.st.DB.C("document").FindOne(ctx, bson.D{{Key: "drive_state", Value: "drive"}, {Key: "owner_id", Value: "EMP-1020"}, {Key: "replaced_at", Value: nil}}); d != nil {
			inDrive, _ = d["id"].(string)
		}
		c.do("POST", "/api/drive/disconnect", "EMP-1002", "HR Admin", nil)
		s, raw := c.getRaw("/api/files/"+inDrive, "EMP-1020", "Employee")
		if s != 409 || !strings.Contains(string(raw), "Reconnect Google Drive to open this file") {
			t.Fatal(s, string(raw))
		}
		s, b := e.uploadFile("EMP-1002", "HR Admin", "employee", "EMP-1020", "edu", pdf("degree"))
		if s != 200 {
			t.Fatal(s, b)
		}
		e.sync(t)
		if st, _, _, skey := e.doc(t, b["id"].(string)); st != "waiting" || !e.staged(skey) {
			t.Fatal("while disconnected the file waits", st)
		}
		e.connect(t)
		e.sync(t)
		if st, fid, _, _ := e.doc(t, b["id"].(string)); st != "drive" || e.g.path(fid) != "EMP-1020/EMP-1020 Highest qualification certificate.pdf" {
			t.Fatal(st, e.g.path(fid))
		}
		if s, _ := c.getRaw("/api/files/"+inDrive, "EMP-1020", "Employee"); s != 200 {
			t.Fatal("opens again after reconnecting", s)
		}
	})
}

func mustZone() *time.Location {
	loc, err := time.LoadLocation(store.Zone())
	if err != nil {
		return time.Local
	}
	return loc
}

func TestDriveMoveExisting(t *testing.T) {
	e := setupDrive(t)
	c := e.c
	// Files from before Drive (kept on the server): a policy file, an employee's document and a replaced one.
	cur := e.stage(t, "employee", "EMP-1021", "EMP-1021", "aadhaar", "local", pdf("aadhaar new"))
	old := e.stage(t, "employee", "EMP-1021", "EMP-1021", "aadhaar", "local", pdf("aadhaar old"))
	if _, err := e.st.DB.C("document").Update(context.Background(), bson.D{{Key: "_id", Value: old}}, bson.D{{Key: "$set", Value: bson.D{{Key: "replaced_at", Value: time.Now()}}}}); err != nil {
		t.Fatal(err)
	}
	nom := e.stage(t, "nomination", "EMP-1021/1", "EMP-1021", "n0:aadhaar", "local", pdf("nominee"))
	if s, b := c.do("POST", "/api/drive/move-existing", "EMP-1002", "HR Admin", nil); s != 400 || !strings.Contains(b["error"].(string), "Connect Google Drive") {
		t.Fatal("moving needs a connection and a folder", s, b)
	}
	e.connect(t)
	e.folder(t)
	e.sync(t)
	st := e.status(t)
	if st["existing"] != float64(3) || st["waiting"] != float64(0) || st["inDrive"] != float64(0) {
		t.Fatal("existing files are not moved until asked", st)
	}
	s, b := c.do("POST", "/api/drive/move-existing", "EMP-1002", "HR Admin", nil)
	if s != 200 || b["moved"] != float64(3) || b["existing"] != float64(0) || b["waiting"] != float64(0) || b["inDrive"] != float64(3) {
		t.Fatal(s, b)
	}
	day := time.Now().In(mustZone()).Format("02 Jan 2006")
	for id, want := range map[string]string{cur: "EMP-1021/EMP-1021 Aadhaar card.pdf", old: "EMP-1021/EMP-1021 Aadhaar card (replaced " + day + ").pdf",
		nom: "EMP-1021/EMP-1021 Nominee Aadhaar copy 1 (Provident fund (EPF) nomination).pdf"} {
		st, fid, _, skey := e.doc(t, id)
		if st != "drive" || e.g.path(fid) != want || e.staged(skey) {
			t.Fatalf("%s: %q staged %v, want %s", st, e.g.path(fid), e.staged(skey), want)
		}
	}
	audit, _ := e.st.DB.C("audit_log").Count(context.Background(), bson.D{{Key: "action", Value: "Moving 3 existing files to Google Drive"}})
	if audit != 1 {
		t.Fatal("move not audited")
	}
}

// The upload job: a new upload goes to Drive on its own, and when Drive is down the job retries until it works.
func TestDriveJob(t *testing.T) {
	e := setupDrive(t)
	c := e.c
	c.signIn("EMP-1020")
	e.connect(t)
	e.folder(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := e.app.StartJobs(ctx); err != nil {
		t.Fatal(err)
	}
	defer e.app.StopJobs(context.Background())
	e.g.set(func(g *fakeGoogle) { g.down = true })
	s, b := e.uploadFile("EMP-1002", "HR Admin", "employee", "EMP-1020", "pan", pdf("pan"))
	if s != 200 {
		t.Fatal(s, b)
	}
	id := b["id"].(string)
	wait := func(what string, ok func() bool) {
		deadline := time.Now().Add(30 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for " + what)
			}
			time.Sleep(150 * time.Millisecond)
		}
	}
	wait("a failed try", func() bool {
		d, _ := e.st.DB.C("document").FindOne(context.Background(), bson.D{{Key: "_id", Value: id}})
		n, _ := d["drive_attempts"].(int32)
		return n > 0
	})
	if st, _, _, skey := e.doc(t, id); st != "waiting" || !e.staged(skey) {
		t.Fatal("waits while Drive is down", st)
	}
	e.g.set(func(g *fakeGoogle) { g.down = false })
	wait("the retry", func() bool { st, _, _, _ := e.doc(t, id); return st == "drive" })
	if _, fid, _, skey := e.doc(t, id); e.g.path(fid) != "EMP-1020/EMP-1020 PAN card.pdf" || e.staged(skey) {
		t.Fatal(e.g.path(fid), e.staged(skey))
	}
}
