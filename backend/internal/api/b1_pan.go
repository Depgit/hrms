package api

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// PAN verification (owner, 14 Nov 2026), behind a provider setting. HR Admin picks the provider in Masters › Company
// › PAN verification: None (off), Test (offline, for trying the app), or one of the real services the owner may
// sign up with later (Surepass, Cashfree, Signzy, Karza), with its API key, base URL and an on/off switch. The key
// is sealed like the Google Drive token (AES-256-GCM under APP_SECRET or the key file) and only its last four
// characters are shown again.
//
// The real providers are stubs: they answer "not configured" until a key is saved, and even then send nothing
// (no network call is made by this build, in tests or in development); the owner's integrator fills them in.
//
// A PAN is checked when it is entered on the joining form (HR's Joining details or the joiner's own link) and
// with "Verify PAN" on the employee's Statutory IDs. The result is kept with the PAN that was checked: Verified,
// Name mismatch or Invalid, the name the provider returned, when and by whom, and audited. Any other PAN on the
// record (a later correction) reads "Not checked" until it is checked again. The PAN itself stays masked as before.

// PAN check results.
const (
	panVerified   = "Verified"
	panMismatch   = "Name mismatch"
	panInvalid    = "Invalid"
	panNotChecked = "Not checked"
)

// panProviderNames are the choices of the setting, in order.
var panProviderNames = []string{"Format check", "None", "Test", "Surepass", "Cashfree", "Signzy", "Karza"}

// panResult is what a provider says about a PAN and a name.
type panResult struct {
	Status  string // panVerified, panMismatch or panInvalid
	Name    string // the holder's name as the provider returned it ("" when it gave none)
	Message string // a short explanation for HR
}

// panProvider checks a PAN against a name.
type panProvider interface {
	Verify(ctx context.Context, cfg panConfig, pan, name string) (panResult, error)
}

// errPanNotConfigured: the provider cannot be used yet.
var errPanNotConfigured = errors.New("not configured")

// panTest is the offline Test provider. A PAN in the right format whose 4th letter is P (a person's PAN) is
// looked up as the Income Tax Department numbers it: the 5th letter is the first letter of the holder's surname.
// The name's last word starting with that letter verifies; any other name is a name mismatch. A PAN whose 4th
// letter is not P, or in the wrong format, is invalid.
type panTest struct{}

func (panTest) Verify(_ context.Context, _ panConfig, pan, name string) (panResult, error) {
	pan = strings.ToUpper(strings.TrimSpace(pan))
	if !rePAN.MatchString(pan) {
		return panResult{Status: panInvalid, Message: "Not a PAN in the format ABCDE1234F"}, nil
	}
	if pan[3] != 'P' {
		return panResult{Status: panInvalid, Message: "Not an individual's PAN (the 4th letter of a person's PAN is P)"}, nil
	}
	words := strings.Fields(strings.ToUpper(name))
	if len(words) == 0 || words[len(words)-1][0] != pan[4] {
		return panResult{Status: panMismatch, Message: fmt.Sprintf("The PAN's holder has a surname starting with %c", pan[4])}, nil
	}
	return panResult{Status: panVerified, Name: strings.Join(words, " "), Message: "Test provider: PAN and name match"}, nil
}

// panFormat is the default (owner, 19 Nov 2026: "verification means only the pattern"): offline, nothing is sent
// anywhere. A PAN is valid when it is in the format ABCDE1234F and its 4th letter is P (an individual's PAN). The
// name is not compared.
type panFormat struct{}

func (panFormat) Verify(_ context.Context, _ panConfig, pan, _ string) (panResult, error) {
	pan = strings.ToUpper(strings.TrimSpace(pan))
	if !rePAN.MatchString(pan) {
		return panResult{Status: panInvalid, Message: "Not a PAN in the format ABCDE1234F"}, nil
	}
	if pan[3] != 'P' {
		return panResult{Status: panInvalid, Message: "Not an individual's PAN (the 4th letter of a person's PAN is P)"}, nil
	}
	return panResult{Status: panVerified, Message: "PAN format is valid (pattern check only)"}, nil
}

// panStub is a real provider not built yet: "not configured" without a key, and no request sent with one.
type panStub struct{ name string }

func (p panStub) Verify(_ context.Context, cfg panConfig, _, _ string) (panResult, error) {
	if cfg.Key == "" {
		return panResult{}, errPanNotConfigured
	}
	return panResult{}, fmt.Errorf("%w: the %s connection is not built yet, so nothing was sent", errPanNotConfigured, p.name)
}

var panProviders = map[string]panProvider{"Format check": panFormat{}, "Test": panTest{}, "Surepass": panStub{"Surepass"}, "Cashfree": panStub{"Cashfree"},
	"Signzy": panStub{"Signzy"}, "Karza": panStub{"Karza"}}

// panConfig is the saved setting with the key unsealed.
type panConfig struct {
	Provider, BaseURL, Key, KeyLast4, UpdatedBy string
	On                                          bool
	UpdatedAt                                   any
}

const panKeyAAD = "company_setting.pan_verify.api_key"

func (s *Server) panSeal(plain string) (string, error) {
	block, err := aes.NewCipher(s.drive.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plain), []byte(panKeyAAD))), nil
}

func (s *Server) panUnseal(v string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(s.drive.key)
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
	p, err := gcm.Open(nil, b[:gcm.NonceSize()], b[gcm.NonceSize():], []byte(panKeyAAD))
	return string(p), err
}

// panConfigLoad reads the setting (provider "Format check", on, when nothing is saved). A key that no longer unseals (the
// app's secret key changed) reads as no key.
func (s *Server) panConfigLoad(ctx context.Context, q store.Q) panConfig {
	cfg := panConfig{Provider: "Format check", On: true}
	r, err := q.C("company_setting").FindOne(ctx, bson.M{"_id": 1})
	if err != nil || r == nil {
		return cfg
	}
	m, _ := r["pan_verify"].(map[string]any)
	if m == nil {
		return cfg
	}
	cfg.Provider, cfg.BaseURL, cfg.KeyLast4, cfg.UpdatedBy = or(str(m["provider"]), "None"), str(m["baseUrl"]), str(m["keyLast4"]), str(m["updatedBy"])
	cfg.On, _ = m["on"].(bool)
	cfg.UpdatedAt = m["updatedAt"]
	if sealed := str(m["key"]); sealed != "" {
		if k, err := s.panUnseal(sealed); err == nil {
			cfg.Key = k
		} else {
			cfg.KeyLast4 = ""
		}
	}
	return cfg
}

func panSettingView(cfg panConfig) map[string]any {
	return map[string]any{"provider": cfg.Provider, "baseUrl": cfg.BaseURL, "on": cfg.On, "keySet": cfg.Key != "", "keyLast4": cfg.KeyLast4,
		"updatedBy": cfg.UpdatedBy, "updatedAt": cfg.UpdatedAt, "providers": panProviderNames,
		"ready": cfg.On && (cfg.Provider == "Test" || cfg.Provider == "Format check" || (cfg.Provider != "None" && cfg.Key != ""))}
}

func (s *Server) panSettingsGet(r *http.Request, c *Ctx) (any, error) {
	if c.Role != "HR Admin" {
		return nil, forbidden("")
	}
	return panSettingView(s.panConfigLoad(r.Context(), s.st.DB)), nil
}

// panSettingsPut saves the provider, base URL and switch; apiKey is set when given (never echoed), clearKey removes it.
func (s *Server) panSettingsPut(r *http.Request, c *Ctx) (any, error) {
	if c.Role != "HR Admin" {
		return nil, forbidden("")
	}
	var b struct {
		Provider string  `json:"provider"`
		BaseURL  string  `json:"baseUrl"`
		On       bool    `json:"on"`
		APIKey   *string `json:"apiKey"`
		ClearKey bool    `json:"clearKey"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Provider, b.BaseURL = strings.TrimSpace(b.Provider), strings.TrimSpace(b.BaseURL)
	if !containsStr(panProviderNames, b.Provider) {
		return nil, bad("Provider is one of %s", strings.Join(panProviderNames, ", "))
	}
	if b.BaseURL != "" {
		u, err := url.Parse(b.BaseURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || len(b.BaseURL) > 200 {
			return nil, bad("Base URL must be an https:// address")
		}
	}
	ctx := r.Context()
	cur := s.panConfigLoad(ctx, s.st.DB)
	key := cur.Key
	if b.ClearKey {
		key = ""
	}
	if b.APIKey != nil && strings.TrimSpace(*b.APIKey) != "" {
		key = strings.TrimSpace(*b.APIKey)
		if len(key) < 8 || len(key) > 400 {
			return nil, bad("The API key looks wrong: 8 to 400 characters")
		}
	}
	if b.On && b.Provider == "None" {
		return nil, bad("Choose a provider before switching PAN verification on")
	}
	sealed, last4 := "", ""
	if key != "" {
		var err error
		if sealed, err = s.panSeal(key); err != nil {
			return nil, err
		}
		last4 = key[max(0, len(key)-4):]
	}
	who := c.Name + " (" + c.EmployeeID + ")"
	doc := bson.M{"provider": b.Provider, "baseUrl": b.BaseURL, "on": b.On, "key": sealed, "keyLast4": last4, "updatedBy": who, "updatedAt": time.Now().UTC()}
	err := s.st.Tx(ctx, func(tx store.Q) error {
		if _, err := tx.C("company_setting").Update(ctx, bson.M{"_id": 1}, bson.M{"$set": bson.M{"pan_verify": doc}}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "PAN verification settings saved", "company_setting", "1", map[string]any{"provider": cur.Provider, "on": cur.On, "keySet": cur.Key != ""},
			map[string]any{"provider": b.Provider, "on": b.On, "baseUrl": b.BaseURL, "keySet": key != ""})
	})
	if err != nil {
		return nil, err
	}
	return panSettingView(s.panConfigLoad(ctx, s.st.DB)), nil
}

// panRun checks a PAN with the provider in the setting. It refuses when verification is off or not configured.
func (s *Server) panRun(ctx context.Context, pan, name string) (panResult, string, error) {
	cfg := s.panConfigLoad(ctx, s.st.DB)
	if !cfg.On || cfg.Provider == "None" {
		return panResult{}, cfg.Provider, bad("PAN verification is off. HR Admin sets it up in Masters › Company › PAN verification")
	}
	p := panProviders[cfg.Provider]
	if p == nil {
		return panResult{}, cfg.Provider, bad("Unknown PAN verification provider %s", cfg.Provider)
	}
	res, err := p.Verify(ctx, cfg, pan, name)
	if errors.Is(err, errPanNotConfigured) {
		return res, cfg.Provider, bad("PAN verification with %s is not configured: %v", cfg.Provider, err)
	}
	return res, cfg.Provider, err
}

// panMask shows a PAN's last four characters only, for the audit log.
func panMask(pan string) string {
	if len(pan) < 4 {
		return "****"
	}
	return strings.Repeat("*", len(pan)-4) + pan[len(pan)-4:]
}

// panView is the PAN check shown with a record's PAN: the status only while the PAN is the one checked.
func panView(g store.Row) map[string]any {
	pan := strings.ToUpper(str(g["pan"]))
	if pan == "" {
		return nil
	}
	if str(g["pan_checked"]) != pan || str(g["pan_status"]) == "" {
		return map[string]any{"status": panNotChecked}
	}
	return map[string]any{"status": g["pan_status"], "name": g["pan_name"], "at": g["pan_checked_at"], "by": g["pan_checked_by"]}
}

// employeePanVerify is "Verify PAN" on the employee's Statutory IDs: whoever edits government IDs, in scope.
func (s *Server) employeePanVerify(r *http.Request, c *Ctx) (any, error) {
	ctx, id := r.Context(), r.PathValue("id")
	if err := c.Need("emp.govid", 2); err != nil {
		return nil, err
	}
	if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
		return nil, err
	}
	e, err := s.st.DB.C("employee").FindOne(ctx, bson.M{"_id": id})
	if err != nil || e == nil {
		return nil, orNotFound(err)
	}
	g, err := s.st.DB.C("employee_gov_id").FindOne(ctx, bson.M{"employee_id": id})
	if err != nil {
		return nil, err
	}
	pan := strings.ToUpper(str(g["pan"]))
	if pan == "" {
		return nil, bad("%s has no PAN on record", e["name"])
	}
	res, provider, err := s.panRun(ctx, pan, str(e["name"]))
	if err != nil {
		return nil, err
	}
	by := c.Name + " (" + c.EmployeeID + ")"
	err = s.st.Tx(ctx, func(tx store.Q) error {
		at := tx.Now()
		if _, err := tx.C("employee_gov_id").Update(ctx, bson.M{"employee_id": id, "pan": g["pan"]}, bson.M{"$set": bson.M{"pan_status": res.Status,
			"pan_name": employeesNullIf(res.Name), "pan_checked_at": at, "pan_checked_by": by, "pan_checked": pan}}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "PAN checked for "+id+": "+res.Status, "employee", id, nil,
			map[string]any{"pan": panMask(pan), "status": res.Status, "nameReturned": res.Name, "provider": provider})
	})
	if err != nil {
		return nil, err
	}
	g, _ = s.st.DB.C("employee_gov_id").FindOne(ctx, bson.M{"employee_id": id})
	out := panView(g)
	out["message"] = res.Message
	return out, nil
}

// panCheckOffer checks the PAN on an offer's joining details when verification is on and the PAN is new (not the
// one checked last). Nothing happens while verification is off; a provider error is left for HR's next save.
func (s *Server) panCheckOffer(ctx context.Context, actor *Ctx, offerID string) {
	cfg := s.panConfigLoad(ctx, s.st.DB)
	if !cfg.On || cfg.Provider == "None" {
		return
	}
	o, err := s.st.DB.C("offer").FindOne(ctx, bson.M{"_id": offerID})
	if err != nil || o == nil {
		return
	}
	d, _ := o["data"].(map[string]any)
	pan := strings.ToUpper(strings.TrimSpace(str(d["pan"])))
	if pan == "" {
		return
	}
	if pc, _ := o["pan_check"].(map[string]any); pc != nil && str(pc["pan"]) == pan {
		return
	}
	res, provider, err := s.panRun(ctx, pan, str(o["candidate_name"]))
	if err != nil {
		return
	}
	by := "Joining form"
	if actor != nil {
		by = actor.Name + " (" + actor.EmployeeID + ")"
	}
	err = s.st.Tx(ctx, func(tx store.Q) error {
		at := tx.Now()
		if _, err := tx.C("offer").Update(ctx, bson.M{"_id": offerID, "data.pan": d["pan"]}, bson.M{"$set": bson.M{"pan_check": bson.M{"pan": pan, "status": res.Status,
			"name": res.Name, "at": at, "by": by}}}); err != nil {
			return err
		}
		return audit(ctx, tx, actor, "PAN checked on the joining form of "+offerID+": "+res.Status, "offer", offerID, nil,
			map[string]any{"pan": panMask(pan), "status": res.Status, "nameReturned": res.Name, "provider": provider})
	})
	if err != nil {
		log.Printf("PAN check of offer %s: %v", offerID, err)
	}
}

// offerPanView is an offer's PAN check for HR's screens (nil when the PAN is not the one checked).
func offerPanView(o store.Row) map[string]any {
	d, _ := o["data"].(map[string]any)
	pan := strings.ToUpper(strings.TrimSpace(str(d["pan"])))
	if pan == "" {
		return nil
	}
	pc, _ := o["pan_check"].(map[string]any)
	if pc == nil || str(pc["pan"]) != pan {
		return map[string]any{"status": panNotChecked}
	}
	return map[string]any{"status": pc["status"], "name": pc["name"], "at": pc["at"], "by": pc["by"]}
}
