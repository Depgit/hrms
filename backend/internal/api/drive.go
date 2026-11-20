package api

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/jobs"
	"github.com/saltstayz/hrms/internal/store"
)

// Document storage in the company's Google Drive (owner, 11 Nov 2026). An HR Admin connects a Google account
// (OAuth 2.0 web flow, offline access) and pastes a Drive folder link in Masters › Company. Every upload is staged
// on the server's disk as before (files.go), then a background job (jobs.go) puts it in a subfolder per person under that folder
// (SI-0001/SI-0001 Aadhaar card.pdf; the Employee ID for people without a candidate record; Company for company
// files), records the Drive file id on the `document` row and deletes the staged copy. Opening a document streams
// it from Drive through the app after the usual access checks, so nobody needs Drive access of their own.
// While Drive is not connected or not answering, files stay on the server ("waiting") and go up once it works.
// Only plain HTTPS calls to Google's REST endpoints (stdlib); the base URLs can point at a fake for tests.

// OAuth scopes. Writing into a folder the admin pasted needs the full `drive` scope: `drive.file` only covers files
// this app created or the user picked with the Google Picker, so the pasted folder would not be visible to it.
// `openid email` give the account's address for "Connected as".
const (
	driveFullScope    = "https://www.googleapis.com/auth/drive"
	driveScopes       = driveFullScope + " openid email"
	driveFolderMime   = "application/vnd.google-apps.folder"
	driveCookie       = "hrms_drive_oauth"
	driveCallbackPath = "/api/drive/callback"
	driveCompany      = "Company"
	driveSweepEvery   = time.Minute
)

// googleConfig is the OAuth client and Google's endpoints. GOOGLE_BASE_URL points every endpoint at one fake
// server (tests, demos); never set it in production.
type googleConfig struct {
	ClientID, ClientSecret string
	RedirectURL            string // GOOGLE_REDIRECT_URL; empty = worked out from where the app is opened
	AuthURL, TokenURL      string
	RevokeURL              string
	DriveURL, UploadURL    string
}

func googleFromEnv() googleConfig {
	c := googleConfig{
		ClientID: strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_ID")), ClientSecret: strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_SECRET")),
		RedirectURL: strings.TrimSpace(os.Getenv("GOOGLE_REDIRECT_URL")),
		AuthURL:     "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token",
		RevokeURL: "https://oauth2.googleapis.com/revoke",
		DriveURL:  "https://www.googleapis.com/drive/v3", UploadURL: "https://www.googleapis.com/upload/drive/v3",
	}
	if b := strings.TrimRight(strings.TrimSpace(os.Getenv("GOOGLE_BASE_URL")), "/"); b != "" {
		c.AuthURL, c.TokenURL, c.RevokeURL = b+"/o/oauth2/v2/auth", b+"/token", b+"/revoke"
		c.DriveURL, c.UploadURL = b+"/drive/v3", b+"/upload/drive/v3"
	}
	return c
}

func (g googleConfig) configured() bool { return g.ClientID != "" && g.ClientSecret != "" }

// driveService holds the OAuth client, the key for stored secrets and the short-lived access token.
type driveService struct {
	cfg       googleConfig
	key       []byte // AES-256 key: encrypts the refresh token, signs the OAuth state
	hc        *http.Client
	mu        sync.Mutex // the cached access token
	access    string
	accessExp time.Time
	accessFor string     // hash of the refresh token the cached access token came from
	folderMu  sync.Mutex // one subfolder lookup at a time, so a folder is created once
}

func newDriveService(key []byte) *driveService {
	if len(key) != 32 {
		if v := os.Getenv("APP_SECRET"); v != "" {
			h := sha256.Sum256([]byte(v))
			key = h[:]
		} else {
			// No key given (tests): a key for this process only; a saved connection then lasts until restart.
			key = make([]byte, 32)
			_, _ = rand.Read(key)
		}
	}
	return &driveService{cfg: googleFromEnv(), key: key,
		hc: &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 60 * time.Second, TLSHandshakeTimeout: 10 * time.Second}}}
}

// SecretKey returns the 32-byte key that encrypts stored secrets (the Google refresh token): from APP_SECRET when it
// is set (any text, hashed), else a random key kept in keyFile, created with mode 0600 on first start. The key
// stays outside the database, so a copy of the database alone cannot be used to reach the company's Drive.
func SecretKey(keyFile string) ([]byte, string, error) {
	if v := os.Getenv("APP_SECRET"); v != "" {
		h := sha256.Sum256([]byte(v))
		return h[:], "APP_SECRET", nil
	}
	for i := 0; i < 2; i++ {
		if b, err := os.ReadFile(keyFile); err == nil {
			k, err := hex.DecodeString(strings.TrimSpace(string(b)))
			if err != nil || len(k) != 32 {
				return nil, "", fmt.Errorf("%s is not a valid key file (64 hex characters)", keyFile)
			}
			return k, keyFile, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, "", err
		}
		if err := os.MkdirAll(filepath.Dir(keyFile), 0o750); err != nil {
			return nil, "", err
		}
		f, err := os.OpenFile(keyFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue // another process made it first: read that one
		} else if err != nil {
			return nil, "", err
		}
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			f.Close()
			return nil, "", err
		}
		_, err = f.WriteString(hex.EncodeToString(k) + "\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, "", err
		}
		return k, keyFile + " (created)", nil
	}
	return nil, "", fmt.Errorf("could not read or create %s", keyFile)
}

const sealAAD = "drive_setting.refresh_token"

func (d *driveService) seal(plain string) ([]byte, error) {
	block, err := aes.NewCipher(d.key)
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
	return gcm.Seal(nonce, nonce, []byte(plain), []byte(sealAAD)), nil
}

func (d *driveService) unseal(b []byte) (string, error) {
	block, err := aes.NewCipher(d.key)
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
	p, err := gcm.Open(nil, b[:gcm.NonceSize()], b[gcm.NonceSize():], []byte(sealAAD))
	return string(p), err
}

// ---------- OAuth state: signed, short-lived, bound to the browser by a cookie ----------

type oauthState struct {
	Emp      string `json:"e"`
	Nonce    string `json:"n"`
	Exp      int64  `json:"x"`
	Redirect string `json:"r"`
}

func (d *driveService) stateMAC(payload string) []byte {
	k := hmac.New(sha256.New, d.key)
	k.Write([]byte("drive-oauth-state"))
	m := hmac.New(sha256.New, k.Sum(nil))
	m.Write([]byte(payload))
	return m.Sum(nil)
}

func (d *driveService) signState(st oauthState) string {
	b, _ := json.Marshal(st)
	p := base64.RawURLEncoding.EncodeToString(b)
	return p + "." + base64.RawURLEncoding.EncodeToString(d.stateMAC(p))
}

func (d *driveService) verifyState(s string) (oauthState, bool) {
	var st oauthState
	p, sig, ok := strings.Cut(s, ".")
	if !ok || len(s) > 2048 {
		return st, false
	}
	mac, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(mac, d.stateMAC(p)) {
		return st, false
	}
	b, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil || json.Unmarshal(b, &st) != nil || time.Now().Unix() > st.Exp || st.Emp == "" || st.Nonce == "" || !strings.HasSuffix(st.Redirect, driveCallbackPath) {
		return oauthState{}, false
	}
	return st, true
}

// ---------- Settings ----------

type driveConf struct {
	FolderID, FolderName, FolderLink string
	Email, ConnectedBy, Broken       string
	ConnectedAt, LastUploadAt        *time.Time
	LastError                        string
	LastErrorAt                      *time.Time
	token                            []byte // encrypted refresh token; never leaves the server
}

func (c driveConf) connected() bool { return len(c.token) > 0 && c.Broken == "" }
func (c driveConf) ready() bool     { return c.connected() && c.FolderID != "" }

func (s *Server) driveConf(ctx context.Context) (driveConf, error) {
	var c driveConf
	r, err := s.st.DB.C("drive_setting").FindOne(ctx, driveSettingRow)
	if err != nil || r == nil { // no settings row yet: not connected
		return c, err
	}
	c = driveConf{FolderID: str(r["folder_id"]), FolderName: str(r["folder_name"]), FolderLink: str(r["folder_link"]), Email: str(r["account_email"]),
		ConnectedBy: str(r["connected_by"]), Broken: str(r["broken"]), ConnectedAt: driveTime(r["connected_at"]), LastUploadAt: driveTime(r["last_upload_at"]),
		LastError: str(r["last_error"]), LastErrorAt: driveTime(r["last_error_at"])}
	c.token, _ = r["refresh_token"].([]byte)
	return c, nil
}

// driveSettingRow is the one row of drive_setting.
var driveSettingRow = bson.D{{Key: "_id", Value: 1}}

// driveTime is a nullable timestamp column as *time.Time.
func driveTime(v any) *time.Time {
	if t, ok := v.(time.Time); ok {
		return &t
	}
	return nil
}

// driveSet is an update document that sets these columns.
func driveSet(cols ...bson.E) bson.D { return bson.D{{Key: "$set", Value: bson.D(cols)}} }

// driveDocByID matches one document row by its id.
func driveDocByID(id any) bson.D { return bson.D{{Key: "_id", Value: id}} }

// driveNotDraft matches documents whose owner is not an unsaved draft (an owner id starting DRAFT-).
var driveNotDraft = bson.E{Key: "owner_id", Value: bson.D{{Key: "$not", Value: bson.Regex{Pattern: "^DRAFT-"}}}}

// driveUnclaimed matches documents no worker holds now (no claim, or the claim ran out before now).
func driveUnclaimed(now time.Time) bson.D {
	return bson.D{{Key: "$or", Value: bson.A{bson.D{{Key: "drive_claim_until", Value: nil}}, bson.D{{Key: "drive_claim_until", Value: bson.D{{Key: "$lt", Value: now}}}}}}}
}

// ---------- Talking to Google ----------

// driveError is a problem with Google Drive, in plain words for the settings card and the person opening a file.
type driveError struct {
	msg    string
	status int    // Google's HTTP status; 0 when Google did not answer
	reason string // Google's error reason (notFound, storageQuotaExceeded …)
	stop   bool   // nothing else will work either: stop this round of uploads
	reauth bool   // the connection must be made again
}

func (e *driveError) Error() string { return e.msg }

var errDriveNotConnected = &driveError{msg: "Google Drive is not connected", stop: true, reauth: true}

func isDriveNotFound(err error) bool {
	var de *driveError
	return errors.As(err, &de) && de.status == 404
}

// googleErr turns an error answer from Google into plain words.
func googleErr(res *http.Response) *driveError {
	defer res.Body.Close()
	var g struct {
		Error struct {
			Message string `json:"message"`
			Errors  []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	_ = json.Unmarshal(b, &g)
	e := &driveError{status: res.StatusCode}
	if len(g.Error.Errors) > 0 {
		e.reason = g.Error.Errors[0].Reason
	}
	switch {
	case res.StatusCode == 401:
		e.msg, e.stop, e.reauth = "Google did not accept the saved sign-in. Reconnect Google Drive", true, true
	case e.reason == "storageQuotaExceeded":
		e.msg, e.stop = "The Google Drive is full. Free up space or add storage; waiting files go up after that", true
	case res.StatusCode == 429 || e.reason == "rateLimitExceeded" || e.reason == "userRateLimitExceeded":
		e.msg, e.stop = "Google Drive is busy (too many requests); it will try again", true
	case res.StatusCode == 403:
		e.msg, e.stop = "The connected Google account is not allowed to add files to the folder. Give it edit access to the folder", true
	case res.StatusCode == 404:
		e.msg = "Not found in Google Drive"
	case res.StatusCode >= 500:
		e.msg, e.stop = "Google Drive is not answering right now; it will try again", true
	default:
		e.msg = "Google Drive refused the request"
		if g.Error.Message != "" {
			e.msg += " (" + g.Error.Message + ")"
		}
	}
	return e
}

// driveAccess returns an access token, refreshing it with the stored refresh token when it is old (or when force).
func (s *Server) driveAccess(ctx context.Context, force bool) (string, error) {
	conf, err := s.driveConf(ctx)
	if err != nil {
		return "", err
	}
	if !conf.connected() {
		return "", errDriveNotConnected
	}
	rt, err := s.drive.unseal(conf.token)
	if err != nil {
		return "", &driveError{msg: "The saved Google sign-in cannot be read (the server's secret key changed). Reconnect Google Drive", stop: true, reauth: true}
	}
	d := s.drive
	d.mu.Lock()
	defer d.mu.Unlock()
	sum := sha(rt)
	if !force && d.access != "" && d.accessFor == sum && time.Until(d.accessExp) > time.Minute {
		return d.access, nil
	}
	tok, err := s.googleToken(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}})
	if err != nil {
		var de *driveError
		if errors.As(err, &de) && de.reauth { // only if the sign-in it used is still the saved one
			now := s.st.DB.Now()
			_, _ = s.st.DB.C("drive_setting").Update(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "refresh_token", Value: conf.token}},
				driveSet(bson.E{Key: "broken", Value: de.msg}, bson.E{Key: "last_error", Value: de.msg}, bson.E{Key: "last_error_at", Value: now}, bson.E{Key: "updated_at", Value: now}))
		}
		return "", err
	}
	d.access, d.accessFor, d.accessExp = tok.Access, sum, time.Now().Add(time.Duration(tok.ExpiresIn)*time.Second)
	return d.access, nil
}

func (d *driveService) forget() {
	d.mu.Lock()
	d.access, d.accessFor = "", ""
	d.mu.Unlock()
}

type googleTokens struct {
	Access    string `json:"access_token"`
	ExpiresIn int64  `json:"expires_in"`
	Refresh   string `json:"refresh_token"`
	Scope     string `json:"scope"`
	IDToken   string `json:"id_token"`
}

// googleToken calls Google's token endpoint (code exchange or refresh). Error answers carry no token, and only
// Google's error code is kept.
func (s *Server) googleToken(ctx context.Context, form url.Values) (googleTokens, error) {
	var t googleTokens
	form.Set("client_id", s.drive.cfg.ClientID)
	form.Set("client_secret", s.drive.cfg.ClientSecret)
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(tctx, "POST", s.drive.cfg.TokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := s.drive.hc.Do(req)
	if err != nil {
		return t, &driveError{msg: "Google sign-in is not answering right now; it will try again", stop: true}
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != 200 {
		var g struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &g)
		switch g.Error {
		case "invalid_grant":
			return t, &driveError{msg: "Google Drive access was withdrawn or has expired. Reconnect Google Drive", status: res.StatusCode, stop: true, reauth: true}
		case "invalid_client", "unauthorized_client":
			return t, &driveError{msg: "Google did not accept the OAuth client (check GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET)", status: res.StatusCode, stop: true}
		}
		if res.StatusCode >= 500 {
			return t, &driveError{msg: "Google sign-in is not answering right now; it will try again", status: res.StatusCode, stop: true}
		}
		return t, &driveError{msg: "Google sign-in failed (" + strings.TrimSpace(g.Error+" "+strconv.Itoa(res.StatusCode)) + ")", status: res.StatusCode, stop: true}
	}
	if err := json.Unmarshal(b, &t); err != nil || t.Access == "" {
		return googleTokens{}, &driveError{msg: "Google sign-in gave an answer the app could not read", stop: true}
	}
	if t.ExpiresIn <= 0 {
		t.ExpiresIn = 3600
	}
	return t, nil
}

// driveCall sends one request to the Drive API with the access token, refreshing the token once if Google says
// it has expired. The caller closes the body. ctype and body may be empty.
func (s *Server) driveCall(ctx context.Context, method, u, ctype string, body []byte) (*http.Response, error) {
	for try := 0; ; try++ {
		tok, err := s.driveAccess(ctx, try > 0)
		if err != nil {
			return nil, err
		}
		var rd io.Reader = http.NoBody
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		res, err := s.drive.hc.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &driveError{msg: "Google Drive is not answering right now; it will try again", stop: true}
		}
		if res.StatusCode == 401 && try == 0 {
			res.Body.Close()
			continue
		}
		return res, nil
	}
}

// driveJSON sends a request and reads a JSON answer into out (nil to ignore it).
func (s *Server) driveJSON(ctx context.Context, method, u, ctype string, body []byte, out any) error {
	tctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res, err := s.driveCall(tctx, method, u, ctype, body)
	if err != nil {
		return err
	}
	if res.StatusCode >= 300 {
		return googleErr(res)
	}
	defer res.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(out); err != nil {
		return &driveError{msg: "Google Drive gave an answer the app could not read; it will try again", stop: true}
	}
	return nil
}

type driveFile struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	MimeType     string `json:"mimeType"`
	Trashed      bool   `json:"trashed"`
	MD5          string `json:"md5Checksum"`
	Size         string `json:"size"`
	Capabilities struct {
		CanAddChildren bool `json:"canAddChildren"`
	} `json:"capabilities"`
}

// driveQ quotes a value inside a Drive search query.
func driveQ(s string) string { return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'" }

var reDriveID = regexp.MustCompile(`^[A-Za-z0-9_-]{10,200}$`)

func (s *Server) driveGetFile(ctx context.Context, id, fields string) (driveFile, error) {
	var f driveFile
	if !reDriveID.MatchString(id) {
		return f, &driveError{msg: "Not found in Google Drive", status: 404}
	}
	err := s.driveJSON(ctx, "GET", s.drive.cfg.DriveURL+"/files/"+id+"?"+url.Values{"fields": {fields}, "supportsAllDrives": {"true"}}.Encode(), "", nil, &f)
	return f, err
}

func (s *Server) driveFind(ctx context.Context, q string) ([]driveFile, error) {
	var list struct {
		Files []driveFile `json:"files"`
	}
	v := url.Values{"q": {q}, "fields": {"files(id,name,md5Checksum,size)"}, "supportsAllDrives": {"true"}, "includeItemsFromAllDrives": {"true"},
		"corpora": {"allDrives"}, "pageSize": {"10"}}
	err := s.driveJSON(ctx, "GET", s.drive.cfg.DriveURL+"/files?"+v.Encode(), "", nil, &list)
	return list.Files, err
}

// driveFolder returns the id of the subfolder `name` under root, from the cache, else found or created in Drive.
// A cached folder that was deleted or trashed in Drive is made again.
func (s *Server) driveFolder(ctx context.Context, root, name string) (string, error) {
	s.drive.folderMu.Lock()
	defer s.drive.folderMu.Unlock()
	var id string
	folders, this := s.st.DB.C("drive_folder"), driveFolderKey(root, name)
	if cached, err := folders.FindOne(ctx, this); err == nil && cached != nil {
		id = str(cached["folder_id"])
		f, err := s.driveGetFile(ctx, id, "id,trashed")
		if err == nil && !f.Trashed {
			return id, nil
		}
		if err != nil && !isDriveNotFound(err) {
			return "", err
		}
		if _, err := folders.Delete(ctx, this); err != nil {
			return "", err
		}
	}
	found, err := s.driveFind(ctx, fmt.Sprintf("name = %s and %s in parents and mimeType = %s and trashed = false", driveQ(name), driveQ(root), driveQ(driveFolderMime)))
	if err != nil {
		return "", err
	}
	if len(found) > 0 {
		id = found[0].ID
	} else {
		meta, _ := json.Marshal(map[string]any{"name": name, "mimeType": driveFolderMime, "parents": []string{root}})
		var f driveFile
		if err := s.driveJSON(ctx, "POST", s.drive.cfg.DriveURL+"/files?supportsAllDrives=true&fields=id", "application/json; charset=UTF-8", meta, &f); err != nil {
			if isDriveNotFound(err) {
				return "", &driveError{msg: "The Drive folder set in Masters › Company was not found: it was deleted, or it is no longer shared with the connected account. Paste the folder link again", status: 404, stop: true}
			}
			return "", err
		}
		id = f.ID
	}
	_, err = folders.Upsert(ctx, this, bson.M{"folder_id": id, "created_at": s.st.DB.Now()}, nil)
	return id, err
}

// driveFolderKey matches the cached subfolder `name` under root.
func driveFolderKey(root, name string) bson.D {
	return bson.D{{Key: "root_id", Value: root}, {Key: "name", Value: name}}
}

// driveUpload sends one file (multipart upload: metadata and bytes in one request). The document id goes into
// appProperties, so a retry after a lost answer finds the file instead of uploading it twice.
func (s *Server) driveUpload(ctx context.Context, parent, name, ctype, docID string, data []byte) (driveFile, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	pw, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	_ = json.NewEncoder(pw).Encode(map[string]any{"name": name, "parents": []string{parent}, "mimeType": ctype, "appProperties": map[string]string{"hrmsDocId": docID}})
	pw, _ = mw.CreatePart(textproto.MIMEHeader{"Content-Type": {ctype}})
	_, _ = pw.Write(data)
	_ = mw.Close()
	var f driveFile
	err := s.driveJSON(ctx, "POST", s.drive.cfg.UploadURL+"/files?uploadType=multipart&supportsAllDrives=true&fields=id,name,md5Checksum,size",
		"multipart/related; boundary="+mw.Boundary(), buf.Bytes(), &f)
	return f, err
}

func (s *Server) driveRename(ctx context.Context, id, name string) error {
	if !reDriveID.MatchString(id) {
		return &driveError{msg: "Not found in Google Drive", status: 404}
	}
	b, _ := json.Marshal(map[string]string{"name": name})
	return s.driveJSON(ctx, "PATCH", s.drive.cfg.DriveURL+"/files/"+id+"?supportsAllDrives=true&fields=id,name", "application/json; charset=UTF-8", b, nil)
}

// ---------- Names: SI-0001/SI-0001 Aadhaar card.pdf ----------

type driveDoc struct {
	ID, Kind, Owner, Employee, Key string
	FileName, CType, StorageKey    string
	Size                           int64
	UploadedAt                     time.Time
	ReplacedAt                     *time.Time
}

// driveDay is a date as people read it, in the business time zone: 11 Nov 2026.
func driveDay(t time.Time) string {
	if loc, err := time.LoadLocation(store.Zone()); err == nil {
		t = t.In(loc)
	}
	return t.Format("02 Jan 2006")
}

// cleanName keeps a name readable and safe as a Drive file name: no slashes or control characters, one space
// between words, at most 180 characters.
func cleanName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\':
			return '-'
		case unicode.IsControl(r):
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 180 {
		s = string(r[:180])
	}
	return s
}

// drivePerson is the folder for a person: the candidate's SI id when they came through a candidate record,
// otherwise the Employee ID (everyone hired before onboarding stages).
func (s *Server) drivePerson(ctx context.Context, emp string) string {
	x, err := s.st.DB.C("candidate").FindOne(ctx, bson.D{{Key: "employee_id", Value: emp}}, options.FindOne().SetSort(bson.D{{Key: "no", Value: 1}}))
	if si := str(onboardingCol(x, "id")); err == nil && si != "" {
		return si
	}
	return emp
}

// drivePlace works out the subfolder and the file name of a document.
func (s *Server) drivePlace(ctx context.Context, d driveDoc) (folder, name string) {
	ownerEmp, _, _ := strings.Cut(d.Owner, "/")
	switch d.Kind {
	case "company", "policy":
		folder = driveCompany
	case "candidate":
		folder = d.Owner
	case "employee", "nomination", "approval", "policy_ack":
		folder = s.drivePerson(ctx, ownerEmp)
	case "offer":
		// The offer of a candidate (OI-0001) goes with the candidate's SI folder; a legacy offer (OFR-1001) with its
		// employee's folder once there is one, else its own.
		o, _ := s.st.DB.C("offer").FindOne(ctx, driveDocByID(d.Owner))
		cand, emp := str(onboardingCol(o, "candidate_id")), d.Employee
		if emp == "" {
			emp = str(onboardingCol(o, "employee_id"))
		}
		switch {
		case cand != "":
			folder = cand
		case emp != "":
			folder = s.drivePerson(ctx, emp)
		default:
			folder = d.Owner
		}
	default:
		folder = ownerEmp
	}
	if folder = cleanName(folder); folder == "" {
		folder = "Other"
	}
	label := cleanName(s.driveLabel(ctx, d))
	if label == "" {
		label = "Document"
	}
	name = label
	if folder != driveCompany {
		name = folder + " " + label
	}
	if d.ReplacedAt != nil {
		name += " (replaced " + driveDay(*d.ReplacedAt) + ")"
	}
	ext := allowedTypes[d.CType]
	if ext == "" {
		ext = strings.ToLower(filepath.Ext(d.FileName))
	}
	return folder, cleanName(name) + ext
}

// driveLabel names the document: its label from Onboarding setup, the nomination scheme's document, the company
// policy and version, or the name the app uploads it under (Tax proof, Claim bill, Payslip logo …).
func (s *Server) driveLabel(ctx context.Context, d driveDoc) string {
	q := s.st.DB
	switch d.Kind {
	case "policy", "policy_ack":
		ver := d.Owner
		if d.Kind == "policy_ack" {
			_, ver, _ = strings.Cut(d.Owner, "/")
		}
		if title, v, ok := s.drivePolicyVersion(ctx, ver); ok {
			if !strings.HasPrefix(strings.ToLower(v), "v") {
				v = "v" + v
			}
			if d.Kind == "policy_ack" {
				return title + " " + v + " signed"
			}
			return title + " " + v
		}
	case "nomination":
		_, scheme, _ := strings.Cut(d.Owner, "/")
		who, docID, _ := strings.Cut(d.Key, ":")
		var x store.Row
		if id, ok := driveNumericID(scheme); ok {
			x, _ = onboardingOne(ctx, q, "nomination_scheme", id)
		}
		var docs []nomDoc
		if x != nil && onboardingDecodeJSON(x["docs"], &docs) == nil {
			name := str(x["name"])
			for _, x := range docs {
				if x.ID == docID {
					l := x.Label
					if i, err := strconv.Atoi(strings.TrimPrefix(who, "n")); strings.HasPrefix(who, "n") && err == nil {
						l += " " + strconv.Itoa(i+1)
					}
					return l + " (" + name + ")"
				}
			}
		}
	case "approval":
		return d.Key + " " + driveDay(d.UploadedAt)
	}
	if x, _ := q.C("onb_document").FindOne(ctx, bson.D{{Key: "key", Value: d.Key}}); x != nil && strings.TrimSpace(str(x["label"])) != "" {
		return str(x["label"])
	}
	if strings.HasPrefix(d.Key, "leave-") {
		return "Leave document " + driveDay(d.UploadedAt)
	}
	if d.Key == "resume" && d.Kind == "candidate" {
		return "Resume"
	}
	return d.Key
}

// drivePolicyVersion is the policy title and the version name of a company_policy_version id given as text.
func (s *Server) drivePolicyVersion(ctx context.Context, ver string) (title, version string, ok bool) {
	id, ok := driveNumericID(ver)
	if !ok {
		return "", "", false
	}
	v, _ := onboardingOne(ctx, s.st.DB, "company_policy_version", id)
	if v == nil {
		return "", "", false
	}
	p, _ := onboardingOne(ctx, s.st.DB, "company_policy", v["policy_id"])
	if p == nil {
		return "", "", false
	}
	return str(p["title"]), str(v["version"]), true
}

// driveNumericID reads a numeric id written as text; only its plain decimal form matches (no leading zeros or +).
func driveNumericID(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && strconv.FormatInt(n, 10) == s
}

// replacedName adds "(replaced 11 Nov 2026)" before the extension, so the current upload keeps the clean name.
func replacedName(name string, at time.Time) string {
	if strings.Contains(name, " (replaced ") {
		return name
	}
	ext := filepath.Ext(name)
	return strings.TrimSuffix(name, ext) + " (replaced " + driveDay(at) + ")" + ext
}

// ---------- Uploading ----------

// driveUploadOne puts one waiting document in Drive. It reports whether a file went up; (false, nil) means there was
// nothing to do (already in Drive, another worker has it, an unsaved draft, Drive not set up yet).
func (s *Server) driveUploadOne(ctx context.Context, id string) (bool, error) {
	conf, err := s.driveConf(ctx)
	if err != nil || !conf.ready() {
		return false, err
	}
	// Claimed for 10 minutes so two workers never upload the same file; files of an unsaved draft (offer or
	// policy) wait until they belong to a saved record.
	now := s.st.DB.Now()
	r, err := s.st.DB.C("document").FindOneAndUpdate(ctx, append(bson.D{{Key: "_id", Value: id}, {Key: "drive_state", Value: "waiting"}, driveNotDraft}, driveUnclaimed(now)...),
		driveSet(bson.E{Key: "drive_claim_until", Value: now.Add(10 * time.Minute)}))
	if err != nil {
		return false, err
	}
	if r == nil {
		return false, nil
	}
	d := driveDoc{ID: str(r["id"]), Kind: str(r["owner_kind"]), Owner: str(r["owner_id"]), Employee: str(r["employee_id"]), Key: str(r["doc_key"]),
		FileName: str(r["file_name"]), CType: str(r["content_type"]), StorageKey: str(r["storage_key"]), ReplacedAt: driveTime(r["replaced_at"])}
	d.Size, _ = r["size_bytes"].(int64)
	d.UploadedAt, _ = r["uploaded_at"].(time.Time)
	if err := s.drivePut(ctx, conf, d); err != nil {
		s.driveFailed(ctx, d, err)
		return false, err
	}
	return true, nil
}

func (s *Server) drivePut(ctx context.Context, conf driveConf, d driveDoc) error {
	folder, name := s.drivePlace(ctx, d)
	f, err := s.files.Open(ctx, d.StorageKey)
	if err != nil {
		return &driveError{msg: "The file waiting on the server is missing (" + d.FileName + "); upload it again"}
	}
	data, err := io.ReadAll(io.LimitReader(f, 64<<20+1))
	f.Close()
	if err != nil {
		return err
	}
	if len(data) > 64<<20 { // never send (and then delete) a cut-short copy
		return &driveError{msg: d.FileName + " is larger than 64 MB and stays on the server"}
	}
	parent, err := s.driveFolder(ctx, conf.FolderID, folder)
	if err != nil {
		return err
	}
	h := md5.Sum(data)
	sum := hex.EncodeToString(h[:])
	// An earlier try may have reached Drive although its answer was lost.
	var up driveFile
	found, err := s.driveFind(ctx, fmt.Sprintf("appProperties has { key='hrmsDocId' and value=%s } and %s in parents and trashed = false", driveQ(d.ID), driveQ(parent)))
	if err != nil {
		return err
	}
	for _, x := range found {
		if x.MD5 == sum {
			up = x
		}
	}
	if up.ID == "" {
		if up, err = s.driveUpload(ctx, parent, name, d.CType, d.ID, data); err != nil {
			if isDriveNotFound(err) { // the subfolder went away just now: make it again next time
				_, _ = s.st.DB.C("drive_folder").Delete(ctx, driveFolderKey(conf.FolderID, folder))
			}
			return err
		}
	} else {
		name = up.Name
	}
	// Confirmed only when Drive reports the same bytes back.
	if up.ID == "" || (up.MD5 != "" && up.MD5 != sum) || (up.Size != "" && up.Size != strconv.Itoa(len(data))) {
		return &driveError{msg: "Google Drive did not confirm the file (" + d.FileName + "); it will try again", stop: true}
	}
	err = s.st.Tx(ctx, func(tx store.Q) error {
		now := tx.Now()
		if _, err := tx.C("document").Update(ctx, driveDocByID(d.ID), driveSet(bson.E{Key: "drive_state", Value: "drive"}, bson.E{Key: "drive_file_id", Value: up.ID},
			bson.E{Key: "drive_name", Value: name}, bson.E{Key: "drive_folder", Value: folder}, bson.E{Key: "drive_uploaded_at", Value: now},
			bson.E{Key: "drive_replaced_named", Value: d.ReplacedAt != nil}, bson.E{Key: "drive_claim_until", Value: nil}, bson.E{Key: "drive_error", Value: nil})); err != nil {
			return err
		}
		_, err := tx.C("drive_setting").Update(ctx, driveSettingRow, driveSet(bson.E{Key: "last_upload_at", Value: now}))
		return err
	})
	if err != nil {
		return err
	}
	// Safely in Drive: the staged copy goes.
	if err := s.files.Delete(ctx, d.StorageKey); err != nil {
		log.Printf("drive: staged copy of document %s not deleted: %v", d.ID, err)
	}
	// The newer upload keeps the clean name: earlier versions already in Drive get "(replaced …)".
	if err := s.driveRenameReplaced(ctx, bson.D{{Key: "owner_kind", Value: d.Kind}, {Key: "owner_id", Value: d.Owner}, {Key: "doc_key", Value: d.Key}}); err != nil {
		log.Printf("drive: rename of replaced files: %v", err)
	}
	return nil
}

// driveFailed records a failed try: the file stays on the server and is tried again later (1, 2, 4 … minutes,
// at most an hour apart; the upload job also retries with its own backoff).
func (s *Server) driveFailed(ctx context.Context, d driveDoc, err error) {
	msg := "Could not upload " + d.FileName + "; it will try again"
	var de *driveError
	if errors.As(err, &de) {
		msg = de.msg
	} else {
		log.Printf("drive: upload of document %s: %v", d.ID, err)
	}
	ctx = context.WithoutCancel(ctx)
	now, docs := s.st.DB.Now(), s.st.DB.C("document")
	// The wait doubles with each try so far: 1 minute after the first failure, then 2, 4 … capped at an hour.
	wait := time.Hour
	if x, err := docs.FindOne(ctx, driveDocByID(d.ID)); err == nil && x != nil {
		if n := max(toInt(x["drive_attempts"]), 0); n < 6 {
			wait = time.Minute << n
		}
	}
	_, _ = docs.Update(ctx, driveDocByID(d.ID), bson.D{{Key: "$inc", Value: bson.D{{Key: "drive_attempts", Value: 1}}},
		{Key: "$set", Value: bson.D{{Key: "drive_error", Value: msg}, {Key: "drive_claim_until", Value: nil}, {Key: "drive_next_at", Value: now.Add(wait)}}}})
	_, _ = s.st.DB.C("drive_setting").Update(ctx, driveSettingRow, driveSet(bson.E{Key: "last_error", Value: msg}, bson.E{Key: "last_error_at", Value: now}))
}

// driveRenameReplaced renames replaced documents already in Drive to "… (replaced 11 Nov 2026).pdf" (the oldest
// replacements first, 50 at a time), among the documents that also match only (nil for all).
func (s *Server) driveRenameReplaced(ctx context.Context, only bson.D) error {
	docs := s.st.DB.C("document")
	f := append(bson.D{{Key: "drive_file_id", Value: bson.D{{Key: "$ne", Value: nil}}}, {Key: "replaced_at", Value: bson.D{{Key: "$ne", Value: nil}}},
		{Key: "drive_replaced_named", Value: false}}, only...)
	rows, err := docs.Find(ctx, f, options.Find().SetSort(bson.D{{Key: "replaced_at", Value: 1}, {Key: "_id", Value: 1}}).SetLimit(50))
	if err != nil {
		return err
	}
	for _, r := range rows {
		name := str(r["drive_name"])
		if name != "" {
			name = replacedName(name, r["replaced_at"].(time.Time))
			if err := s.driveRename(ctx, r["drive_file_id"].(string), name); err != nil && !isDriveNotFound(err) {
				return err
			}
		}
		if _, err := docs.Update(ctx, driveDocByID(r["id"]), driveSet(bson.E{Key: "drive_name", Value: strPtr(name)}, bson.E{Key: "drive_replaced_named", Value: true})); err != nil {
			return err
		}
	}
	return nil
}

// SyncDrive uploads what is waiting (oldest first) and renames replaced files. The periodic job runs it every
// minute; a round stops early when Drive is down, full or disconnected, and after five minutes.
func (s *Server) SyncDrive(ctx context.Context) (int, error) {
	conf, err := s.driveConf(ctx)
	if err != nil || !conf.ready() {
		return 0, err
	}
	// Waiting, due for a try now, and not held by another worker.
	now := s.st.DB.Now()
	due := bson.D{{Key: "$or", Value: bson.A{bson.D{{Key: "drive_next_at", Value: nil}}, bson.D{{Key: "drive_next_at", Value: bson.D{{Key: "$lte", Value: now}}}}}}}
	rows, err := s.st.DB.C("document").Find(ctx, bson.D{{Key: "drive_state", Value: "waiting"}, driveNotDraft, {Key: "$and", Value: bson.A{due, driveUnclaimed(now)}}},
		options.Find().SetSort(bson.D{{Key: "uploaded_at", Value: 1}, {Key: "_id", Value: 1}}).SetLimit(200).SetProjection(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return 0, err
	}
	n, start := 0, time.Now()
	for _, r := range rows {
		ok, err := s.driveUploadOne(ctx, r["id"].(string))
		if ok {
			n++
		}
		var de *driveError
		if err != nil && (!errors.As(err, &de) || de.stop) {
			return n, err
		}
		if time.Since(start) > 5*time.Minute {
			return n, nil
		}
	}
	return n, s.driveRenameReplaced(ctx, nil)
}

// DriveUploadArgs puts one new upload in Drive straight away.
type DriveUploadArgs struct {
	DocID string `json:"docId"`
}

func (DriveUploadArgs) Kind() string { return "drive_upload" }

func (DriveUploadArgs) InsertOpts() jobs.InsertOpts {
	// Retries with the queue's backoff (1s, 16s, 81s … about an hour and a half in all); the sweep carries on after that.
	return jobs.InsertOpts{MaxAttempts: 8, UniqueOpts: jobs.UniqueOpts{ByArgs: true, ByState: []jobs.JobState{
		jobs.JobStateAvailable, jobs.JobStatePending, jobs.JobStateRunning, jobs.JobStateRetryable, jobs.JobStateScheduled}}}
}

type driveUploadWorker struct {
	jobs.WorkerDefaults[DriveUploadArgs]
	s *Server
}

func (w *driveUploadWorker) Work(ctx context.Context, j *jobs.Job[DriveUploadArgs]) error {
	_, err := w.s.driveUploadOne(ctx, j.Args.DocID)
	var de *driveError
	if errors.As(err, &de) && de.reauth {
		return nil // waits on the server; the sweep sends it once Drive is connected again
	}
	return err
}

// DriveSweepArgs is the every-minute round for anything still waiting (and the "Move existing files" kick).
type DriveSweepArgs struct{}

func (DriveSweepArgs) Kind() string { return "drive_sweep" }

func (DriveSweepArgs) InsertOpts() jobs.InsertOpts {
	// One round queued or running at a time.
	return jobs.InsertOpts{MaxAttempts: 1, UniqueOpts: jobs.UniqueOpts{ByState: []jobs.JobState{
		jobs.JobStateAvailable, jobs.JobStatePending, jobs.JobStateRunning, jobs.JobStateScheduled}}}
}

type driveSweepWorker struct {
	jobs.WorkerDefaults[DriveSweepArgs]
	s *Server
}

func (w *driveSweepWorker) Timeout(*jobs.Job[DriveSweepArgs]) time.Duration { return 10 * time.Minute }

func (w *driveSweepWorker) Work(ctx context.Context, _ *jobs.Job[DriveSweepArgs]) error {
	if _, err := w.s.SyncDrive(ctx); err != nil {
		var de *driveError
		if !errors.As(err, &de) { // Drive's own problems are on the settings card already
			return err
		}
	}
	return nil
}

// driveQueue asks the job queue to put a new upload in Drive now. Best effort, after the upload is saved: an upload
// never waits for or fails because of Drive, and the sweep picks up anything missed.
func (s *Server) driveQueue(ctx context.Context, id string) {
	if s.jobs == nil {
		return
	}
	// Only while Drive is connected, working and has a folder.
	if ok, _ := s.st.DB.C("drive_setting").Exists(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "refresh_token", Value: bson.D{{Key: "$ne", Value: nil}}},
		{Key: "folder_id", Value: bson.D{{Key: "$ne", Value: nil}}}, {Key: "broken", Value: nil}}); !ok {
		return
	}
	if _, err := s.jobs.Insert(context.WithoutCancel(ctx), DriveUploadArgs{DocID: id}, nil); err != nil {
		log.Printf("drive: queue upload of %s: %v", id, err)
	}
}

// driveKick starts a sweep now (after connecting, setting the folder, or "Move existing files").
func (s *Server) driveKick(ctx context.Context) {
	if s.jobs == nil {
		return
	}
	if _, err := s.jobs.Insert(context.WithoutCancel(ctx), DriveSweepArgs{}, nil); err != nil {
		log.Printf("drive: queue sweep: %v", err)
	}
}

// ---------- Opening a document from Drive ----------

// driveStream sends a document from Drive to the person who opened it (access was checked by the caller).
func (s *Server) driveStream(w http.ResponseWriter, r *http.Request, fileID, name, ctype string) {
	if !reDriveID.MatchString(fileID) {
		writeErr(w, notFound("The file is missing from storage"))
		return
	}
	res, err := s.driveCall(r.Context(), "GET", s.drive.cfg.DriveURL+"/files/"+fileID+"?alt=media&supportsAllDrives=true", "", nil)
	if err != nil {
		var de *driveError
		if errors.As(err, &de) && de.reauth {
			writeErr(w, &apiError{Status: 409, Message: "Reconnect Google Drive to open this file"})
		} else {
			writeErr(w, &apiError{Status: 502, Message: "Google Drive did not answer. Try again in a minute"})
		}
		return
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		ge := googleErr(res)
		switch {
		case ge.status == 404:
			writeErr(w, notFound("This file is no longer in Google Drive (it may have been deleted there)"))
		case ge.reauth || ge.status == 403:
			writeErr(w, &apiError{Status: 409, Message: "Reconnect Google Drive to open this file"})
		default:
			writeErr(w, &apiError{Status: 502, Message: "Google Drive did not answer. Try again in a minute"})
		}
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename=%q`, name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if l := res.Header.Get("Content-Length"); l != "" {
		w.Header().Set("Content-Length", l)
	}
	_, _ = io.Copy(w, res.Body)
}

// ---------- Settings card (Masters › Company, HR Admin only) ----------

func (s *Server) driveRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/drive", s.a(s.driveGet))
	m.HandleFunc("POST /api/drive/connect", s.driveConnect)
	m.HandleFunc("GET /api/drive/callback", s.driveCallback)
	m.HandleFunc("PUT /api/drive/folder", s.a(s.driveFolderPut))
	m.HandleFunc("POST /api/drive/disconnect", s.a(s.driveDisconnect))
	m.HandleFunc("POST /api/drive/move-existing", s.a(s.driveMoveExisting))
	m.HandleFunc("POST /api/drive/sync", s.a(s.driveSyncNow))
}

func driveAdmin(c *Ctx) error {
	if c.Role != "HR Admin" {
		return forbidden("Only HR Admin can manage document storage")
	}
	return nil
}

// validOrigin accepts scheme://host[:port] and nothing else.
func validOrigin(o string) bool {
	u, err := url.Parse(o)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" &&
		!strings.ContainsAny(u.Host, " \"'<>\\")
}

// appOrigin is where the browser has the app: the Origin or Referer of the app's own request (both survive the
// Vite dev proxy, which rewrites Host), else the forwarded or direct host.
func appOrigin(r *http.Request) string {
	if o := r.Header.Get("Origin"); validOrigin(o) {
		return o
	}
	if u, err := url.Parse(r.Header.Get("Referer")); err == nil && validOrigin(u.Scheme+"://"+u.Host) {
		return u.Scheme + "://" + u.Host
	}
	proto := "http"
	if r.TLS != nil {
		proto = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p == "http" || p == "https" {
		proto = p
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = strings.TrimSpace(strings.Split(h, ",")[0])
	}
	if o := proto + "://" + host; validOrigin(o) {
		return o
	}
	return "http://localhost:8080"
}

// driveRedirect is the redirect URI to register in Google Cloud: GOOGLE_REDIRECT_URL, else the app's address
// followed by /api/drive/callback (behind the Vite proxy that is the web address, e.g. http://localhost:5173/…).
func (s *Server) driveRedirect(r *http.Request) string {
	if s.drive.cfg.RedirectURL != "" {
		return s.drive.cfg.RedirectURL
	}
	return appOrigin(r) + driveCallbackPath
}

func (s *Server) driveStatus(ctx context.Context, r *http.Request) (map[string]any, error) {
	conf, err := s.driveConf(ctx)
	if err != nil {
		return nil, err
	}
	// Documents by where they are: waiting for Drive (drafts apart), only on the server from before Drive, in Drive.
	counts, err := s.st.DB.C("document").Aggregate(ctx, bson.A{bson.D{{Key: "$group", Value: bson.D{
		{Key: "_id", Value: bson.D{{Key: "state", Value: "$drive_state"}, {Key: "draft", Value: bson.D{{Key: "$regexMatch", Value: bson.D{{Key: "input", Value: "$owner_id"}, {Key: "regex", Value: "^DRAFT-"}}}}}}},
		{Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}}}}}})
	if err != nil {
		return nil, err
	}
	var waiting, drafts, existing, inDrive int
	for _, c := range counts {
		k, _ := c["id"].(map[string]any)
		switch n := toInt(c["n"]); {
		case k["state"] == "waiting" && k["draft"] == true:
			drafts += n
		case k["state"] == "waiting":
			waiting += n
		case k["state"] == "local":
			existing += n
		case k["state"] == "drive":
			inDrive += n
		}
	}
	var folder any
	if conf.FolderID != "" {
		folder = map[string]string{"id": conf.FolderID, "name": conf.FolderName, "link": conf.FolderLink, "url": "https://drive.google.com/drive/folders/" + conf.FolderID}
	}
	out := map[string]any{
		"configured": s.drive.cfg.configured(), "redirectUri": s.driveRedirect(r), "redirectFixed": s.drive.cfg.RedirectURL != "",
		"connected": conf.connected(), "email": conf.Email, "connectedBy": conf.ConnectedBy, "connectedAt": conf.ConnectedAt,
		"broken": conf.Broken, "folder": folder, "waiting": waiting, "drafts": drafts, "existing": existing, "inDrive": inDrive,
		"lastUploadAt": conf.LastUploadAt, "lastError": conf.LastError, "lastErrorAt": conf.LastErrorAt,
	}
	if len(conf.token) == 0 {
		out["email"] = ""
	}
	return out, nil
}

func (s *Server) driveGet(r *http.Request, c *Ctx) (any, error) {
	if err := driveAdmin(c); err != nil {
		return nil, err
	}
	return s.driveStatus(r.Context(), r)
}

// driveConnect starts the Google sign-in: it returns Google's address for the browser to go to. The state is
// signed and short-lived, and its nonce is also set in a cookie, so the callback only accepts the sign-in this
// browser started.
func (s *Server) driveConnect(w http.ResponseWriter, r *http.Request) {
	c, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := driveAdmin(c); err != nil {
		writeErr(w, err)
		return
	}
	if !s.drive.cfg.configured() {
		writeErr(w, bad("Add GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET to backend/.env and restart the API first"))
		return
	}
	redirect := s.driveRedirect(r)
	nonce := randomHex(16)
	state := s.drive.signState(oauthState{Emp: c.EmployeeID, Nonce: nonce, Exp: time.Now().Add(15 * time.Minute).Unix(), Redirect: redirect})
	http.SetCookie(w, &http.Cookie{Name: driveCookie, Value: nonce, Path: "/api/drive", MaxAge: 900, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: strings.HasPrefix(redirect, "https://")})
	q := url.Values{"client_id": {s.drive.cfg.ClientID}, "redirect_uri": {redirect}, "response_type": {"code"}, "scope": {driveScopes},
		"access_type": {"offline"}, "prompt": {"consent"}, "include_granted_scopes": {"true"}, "state": {state}}
	writeJSON(w, 200, map[string]string{"url": s.drive.cfg.AuthURL + "?" + q.Encode(), "redirectUri": redirect})
}

// idTokenEmail reads the email claim of the id_token Google returned from the token endpoint (received directly
// from Google over TLS, so its signature need not be checked again).
func idTokenEmail(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var c struct {
		Email string `json:"email"`
	}
	_ = json.Unmarshal(b, &c)
	return c.Email
}

func drivePage(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Google Drive</title>
<body style="font-family:system-ui,sans-serif;max-width:560px;margin:60px auto;padding:0 16px;line-height:1.5"><h2>Google Drive</h2><p>%s</p>
<p><a href="/masters?tab=company">Back to Masters › Company</a></p></body>`, html.EscapeString(msg))
}

// driveCallback is where Google sends the browser back after sign-in. It has no session header (a plain browser
// redirect): the signed state says which HR Admin started it, and the cookie that it is the same browser.
func (s *Server) driveCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, ok := s.drive.verifyState(r.URL.Query().Get("state"))
	if !ok {
		drivePage(w, 400, "This Google sign-in link is not valid or has expired. Go back to Masters › Company and click Connect Google Drive again.")
		return
	}
	back := strings.TrimSuffix(st.Redirect, driveCallbackPath) + "/masters?tab=company"
	http.SetCookie(w, &http.Cookie{Name: driveCookie, Value: "", Path: "/api/drive", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	fail := func(msg string) {
		http.Redirect(w, r, back+"&drive=error&msg="+url.QueryEscape(msg), http.StatusSeeOther)
	}
	if ck, err := r.Cookie(driveCookie); err != nil || !hmac.Equal([]byte(ck.Value), []byte(st.Nonce)) {
		fail("The Google sign-in was started in another browser. Click Connect Google Drive again")
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		if e == "access_denied" {
			fail("Google sign-in was cancelled, so Drive is not connected")
		} else {
			fail("Google sign-in did not finish (" + cleanName(e) + ")")
		}
		return
	}
	c, err := loadCtx(ctx, s.st.DB, st.Emp, "HR Admin")
	if err != nil || c.Role != "HR Admin" {
		fail("Only HR Admin can connect Google Drive")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		fail("Google did not send a sign-in code. Click Connect Google Drive again")
		return
	}
	tok, err := s.googleToken(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {st.Redirect}})
	if err != nil {
		fail(err.Error())
		return
	}
	if !strings.Contains(" "+tok.Scope+" ", " "+driveFullScope+" ") {
		fail("Google Drive access was not allowed. Click Connect Google Drive again and allow access to Google Drive")
		return
	}
	if tok.Refresh == "" {
		fail("Google did not give lasting access. Remove Saltstayz HRMS at myaccount.google.com/permissions, then connect again")
		return
	}
	email := idTokenEmail(tok.IDToken)
	if email == "" {
		var about struct {
			User struct {
				Email string `json:"emailAddress"`
			} `json:"user"`
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", s.drive.cfg.DriveURL+"/about?fields=user(emailAddress)", nil)
		req.Header.Set("Authorization", "Bearer "+tok.Access)
		if res, err := s.drive.hc.Do(req); err == nil {
			if res.StatusCode == 200 {
				_ = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&about)
			}
			res.Body.Close()
		}
		email = about.User.Email
	}
	enc, err := s.drive.seal(tok.Refresh)
	if err != nil {
		fail("The sign-in could not be saved")
		return
	}
	err = s.st.Tx(ctx, func(tx store.Q) error {
		now := tx.Now()
		if _, err := tx.C("drive_setting").Update(ctx, driveSettingRow, driveSet(bson.E{Key: "account_email", Value: email}, bson.E{Key: "refresh_token", Value: enc},
			bson.E{Key: "connected_by", Value: c.Name + " (" + c.EmployeeID + ")"}, bson.E{Key: "connected_at", Value: now}, bson.E{Key: "broken", Value: nil},
			bson.E{Key: "updated_at", Value: now})); err != nil {
			return err
		}
		// Waiting files go up straight away.
		if err := driveRetryWaiting(ctx, tx); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Google Drive connected", "drive_setting", "1", nil, map[string]any{"account": email})
	})
	if err != nil {
		log.Printf("drive: save connection: %v", err)
		fail("The sign-in could not be saved")
		return
	}
	s.drive.mu.Lock()
	s.drive.access, s.drive.accessFor, s.drive.accessExp = tok.Access, sha(tok.Refresh), time.Now().Add(time.Duration(tok.ExpiresIn)*time.Second)
	s.drive.mu.Unlock()
	s.driveKick(ctx)
	http.Redirect(w, r, back+"&drive=connected", http.StatusSeeOther)
}

// driveRetryWaiting makes every waiting file due now, with its tries counted from zero again.
func driveRetryWaiting(ctx context.Context, tx store.Q) error {
	_, err := tx.C("document").UpdateMany(ctx, bson.D{{Key: "drive_state", Value: "waiting"}}, driveSet(bson.E{Key: "drive_next_at", Value: nil}, bson.E{Key: "drive_attempts", Value: 0}))
	return err
}

// driveFolderID reads a folder id from a Drive link (…/drive/folders/<id>, …/drive/u/0/folders/<id>?usp=sharing,
// …/open?id=<id>, …/folderview?id=<id>) or a bare id.
func driveFolderID(link string) string {
	link = strings.TrimSpace(link)
	if reDriveID.MatchString(link) {
		return link
	}
	if !strings.Contains(link, "://") {
		link = "https://" + link
	}
	u, err := url.Parse(link)
	if err != nil || (u.Hostname() != "drive.google.com" && u.Hostname() != "docs.google.com") {
		return ""
	}
	parts := strings.Split(u.Path, "/")
	for i, p := range parts {
		if p == "folders" && i+1 < len(parts) && reDriveID.MatchString(parts[i+1]) {
			return parts[i+1]
		}
	}
	if id := u.Query().Get("id"); reDriveID.MatchString(id) {
		return id
	}
	return ""
}

func (s *Server) driveFolderPut(r *http.Request, c *Ctx) (any, error) {
	if err := driveAdmin(c); err != nil {
		return nil, err
	}
	var b struct {
		Link string `json:"link"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	id := driveFolderID(b.Link)
	if id == "" {
		return nil, bad("Paste the link of a Google Drive folder (it looks like https://drive.google.com/drive/folders/…)")
	}
	conf, err := s.driveConf(ctx)
	if err != nil {
		return nil, err
	}
	if !conf.connected() {
		return nil, bad("Connect Google Drive first, then paste the folder link")
	}
	f, err := s.driveGetFile(ctx, id, "id,name,mimeType,trashed,capabilities(canAddChildren)")
	var de *driveError
	switch {
	case isDriveNotFound(err):
		return nil, bad("No folder with this link is visible to %s. Check the link, or share the folder with that account", conf.Email)
	case errors.As(err, &de):
		return nil, bad("%s", de.msg)
	case err != nil:
		return nil, err
	case f.MimeType != driveFolderMime:
		return nil, bad("This link is to a file, not a folder. Paste the link of a folder")
	case f.Trashed:
		return nil, bad("This folder is in the Drive trash. Restore it or choose another folder")
	case !f.Capabilities.CanAddChildren:
		return nil, bad("%s can only view this folder. Give it edit access (Editor, or Content manager in a shared drive)", conf.Email)
	}
	link := strings.TrimSpace(b.Link)
	if len(link) > 500 {
		link = "https://drive.google.com/drive/folders/" + f.ID
	}
	err = s.st.Tx(ctx, func(tx store.Q) error {
		if _, err := tx.C("drive_setting").Update(ctx, driveSettingRow, driveSet(bson.E{Key: "folder_id", Value: f.ID}, bson.E{Key: "folder_name", Value: f.Name},
			bson.E{Key: "folder_link", Value: link}, bson.E{Key: "updated_at", Value: tx.Now()})); err != nil {
			return err
		}
		if err := driveRetryWaiting(ctx, tx); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Google Drive folder set", "drive_setting", "1", map[string]any{"folder": conf.FolderName, "id": conf.FolderID},
			map[string]any{"folder": f.Name, "id": f.ID})
	})
	if err != nil {
		return nil, err
	}
	s.driveKick(ctx)
	return s.driveStatus(ctx, r)
}

func (s *Server) driveDisconnect(r *http.Request, c *Ctx) (any, error) {
	if err := driveAdmin(c); err != nil {
		return nil, err
	}
	ctx := r.Context()
	conf, err := s.driveConf(ctx)
	if err != nil {
		return nil, err
	}
	if len(conf.token) > 0 {
		// Tell Google too (best effort): the app's access ends at once, not only on this server.
		if rt, err := s.drive.unseal(conf.token); err == nil {
			tctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			req, _ := http.NewRequestWithContext(tctx, "POST", s.drive.cfg.RevokeURL, strings.NewReader(url.Values{"token": {rt}}.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if res, err := s.drive.hc.Do(req); err == nil {
				res.Body.Close()
			}
			cancel()
		}
	}
	err = s.st.Tx(ctx, func(tx store.Q) error {
		if _, err := tx.C("drive_setting").Update(ctx, driveSettingRow, driveSet(bson.E{Key: "refresh_token", Value: nil}, bson.E{Key: "account_email", Value: nil},
			bson.E{Key: "connected_by", Value: nil}, bson.E{Key: "connected_at", Value: nil}, bson.E{Key: "broken", Value: nil}, bson.E{Key: "updated_at", Value: tx.Now()})); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Google Drive disconnected", "drive_setting", "1", map[string]any{"account": conf.Email}, nil)
	})
	if err != nil {
		return nil, err
	}
	s.drive.forget()
	return s.driveStatus(ctx, r)
}

// driveMoveExisting sends the files uploaded before Drive was set up (kept on the server until now) to Drive.
func (s *Server) driveMoveExisting(r *http.Request, c *Ctx) (any, error) {
	if err := driveAdmin(c); err != nil {
		return nil, err
	}
	ctx := r.Context()
	conf, err := s.driveConf(ctx)
	if err != nil {
		return nil, err
	}
	if !conf.ready() {
		return nil, bad("Connect Google Drive and set the folder first")
	}
	var n int64
	err = s.st.Tx(ctx, func(tx store.Q) error {
		moved, err := tx.C("document").UpdateMany(ctx, bson.D{{Key: "drive_state", Value: "local"}},
			driveSet(bson.E{Key: "drive_state", Value: "waiting"}, bson.E{Key: "drive_next_at", Value: nil}, bson.E{Key: "drive_attempts", Value: 0}))
		if err != nil {
			return err
		}
		n = moved
		return audit(ctx, tx, c, fmt.Sprintf("Moving %d existing files to Google Drive", n), "drive_setting", "1", nil, map[string]any{"files": n})
	})
	if err != nil {
		return nil, err
	}
	s.driveSyncSoon(ctx)
	out, err := s.driveStatus(ctx, r)
	if out != nil {
		out["moved"] = n
	}
	return out, err
}

// driveSyncNow is "Try now": waiting files go up without waiting for their next try.
func (s *Server) driveSyncNow(r *http.Request, c *Ctx) (any, error) {
	if err := driveAdmin(c); err != nil {
		return nil, err
	}
	ctx := r.Context()
	if _, err := s.st.DB.C("document").UpdateMany(ctx, bson.D{{Key: "drive_state", Value: "waiting"}}, driveSet(bson.E{Key: "drive_next_at", Value: nil})); err != nil {
		return nil, err
	}
	s.driveSyncSoon(ctx)
	return s.driveStatus(ctx, r)
}

// driveSyncSoon runs a sweep on the job queue, or here when the queue is not running (tests).
func (s *Server) driveSyncSoon(ctx context.Context) {
	if s.jobs != nil {
		s.driveKick(ctx)
		return
	}
	if _, err := s.SyncDrive(ctx); err != nil {
		var de *driveError
		if !errors.As(err, &de) {
			log.Printf("drive: sync: %v", err)
		}
	}
}
