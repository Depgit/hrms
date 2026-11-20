package realtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Writing employees to the portal (user's decision, 17 Nov 2026): create an employee, update their details and mark
// them left — and only when HR presses "Send to portal" in the HRMS and confirms the preview (api/portal_write.go).
// Nothing here runs by itself and nothing retries: one confirmed send is one form post.
//
// The portal has no API, so a write fills the employee form as a browser would: open the form (GET), keep its
// hidden fields (view state, event validation), its other controls as the page has them, set the HRMS's details
// on the controls that take them (found by label and name, empform.go), and press the form's save button — the
// only button pressed. Preview does all of that except the post. Every write checks that what it is about to send
// is exactly what HR was shown (Plan.Hash) and refuses otherwise.
//
// Where the form is (FormSpec) is fixed in code, like every other page of the client, and checked against the live
// portal by a read-only look (GET, `hrms realtime-forms`); until it is set, every write and preview answers
// ErrFormNotConfirmed. The buttons named in neverPress are refused even if a form shows them with a save caption.

// FormSpec says where the portal's employee form is and which button saves it.
type FormSpec struct {
	AddPath      string   // the new-employee form (GET), e.g. /EmployeeMaster.aspx
	EditPath     string   // an employee's form, {code} replaced by their EmpCode, e.g. /EmployeeMaster.aspx?EmpCode={code}
	LeftPath     string   // where an employee is marked left; "" = EditPath
	SaveCaptions []string // the save button's exact captions
}

// Spec is the portal's employee form. Empty paths: not confirmed on the live portal yet (see README, "Sending
// employees to the portal").
var Spec = FormSpec{SaveCaptions: []string{"Save", "Submit", "Update"}}

// neverPress are buttons never pressed, whatever their caption (compared lower case, without spaces).
var neverPress = regexp.MustCompile(`delete|remove|upload|import|export|transfer|gate|backup|database|devic|machine|clear|reset|close|cancel|batch|process`)

// ErrFormNotConfirmed: the employee form's page is not set in Spec yet.
var ErrFormNotConfirmed = errors.New("sending to the portal is not switched on: the portal's employee form has not been confirmed yet (Spec in internal/realtime)")

// ErrChanged: the form or the details changed after HR's preview.
var ErrChanged = errors.New("what would be sent is no longer what the preview showed; preview again")

// Write is one change to send.
type Write struct {
	Action string            // create, update, left
	Code   string            // the employee's EmpCode (update, left: which employee; create: the new code)
	Values map[string]string // FieldCode… → value (dates yyyy-mm-dd)
}

// PlanRow is one control as it will be posted.
type PlanRow struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Role     string `json:"role,omitempty"`  // the HRMS detail it carries; "" = left as the portal's form has it
	Value    string `json:"value"`           // as posted
	Shown    string `json:"shown,omitempty"` // a choice's text
	Required bool   `json:"required,omitempty"`
}

// Plan is what a write would post.
type Plan struct {
	Action   string    `json:"action"`
	Page     string    `json:"page"`
	Button   string    `json:"button"`
	Rows     []PlanRow `json:"rows"`
	Problems []string  `json:"problems,omitempty"` // any problem blocks the send
	Unused   []string  `json:"unused,omitempty"`   // HRMS details the form has no place for (not sent)
	Hash     string    `json:"hash"`
	post     url.Values
	button   string
}

// Result is the portal's answer to one post.
type Result struct {
	OK      bool      `json:"ok"`
	Unclear bool      `json:"unclear,omitempty"` // the reply said neither saved nor refused: check the portal
	Message string    `json:"message,omitempty"` // the portal's own words
	Sent    []PlanRow `json:"sent"`
	At      time.Time `json:"at"`
}

func (s FormSpec) confirmed() bool { return s.AddPath != "" && s.EditPath != "" }

// pagePath is the form's path for an action, with the employee's code filled in.
func (s FormSpec) pagePath(action, code string) (string, error) {
	p := ""
	switch action {
	case "create":
		p = s.AddPath
	case "update":
		p = s.EditPath
	case "left":
		p = s.LeftPath
		if p == "" {
			p = s.EditPath
		}
	default:
		return "", fmt.Errorf("unknown action %q", action)
	}
	if strings.Contains(p, "{code}") {
		if code == "" {
			return "", errors.New("no portal code")
		}
		p = strings.ReplaceAll(p, "{code}", url.QueryEscape(code))
	}
	return p, nil
}

// writePath reports whether a path is one of Spec's form pages.
func writePath(path string) bool {
	if !Spec.confirmed() {
		return false
	}
	for _, p := range []string{Spec.AddPath, Spec.EditPath, Spec.LeftPath} {
		if p == "" {
			continue
		}
		if i := strings.IndexByte(p, '?'); i >= 0 {
			p = p[:i]
		}
		if strings.EqualFold(p, path) {
			return true
		}
	}
	return false
}

// portalDate turns yyyy-mm-dd into the portal's dd/mm/yyyy.
func portalDate(s string) string {
	t, err := time.Parse("2006-01-02", strings.TrimSpace(s))
	if err != nil {
		return s
	}
	return t.Format("02/01/2006")
}

// placeholder: a list's "-- Select --" choice.
func placeholder(text string) bool {
	t := fold(text)
	return t == "" || strings.Contains(t, "select") || strings.HasPrefix(t, "--")
}

func fold(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

// pickOption is the choice of a list that is the value: the same value, the same text, or (gender) the same
// first letter. ok false when none is.
func pickOption(fl FormField, v string) (FormOption, bool) {
	for _, o := range fl.Options {
		if o.Value == v {
			return o, true
		}
	}
	for _, o := range fl.Options {
		if fold(o.Text) == fold(v) || fold(o.Value) == fold(v) {
			return o, true
		}
	}
	if fl.Role == FieldGender && v != "" {
		for _, o := range fl.Options {
			if t := fold(o.Text); t != "" && t[0] == fold(v)[0] && !strings.HasPrefix(t, "select") {
				return o, true
			}
		}
	}
	return FormOption{}, false
}

var reLeftChoice = regexp.MustCompile(`(?i)resign|left|leav|inactive|relieved|exit|separat`)

// buildPlan fills the form with the write's details. Problems block the send.
func buildPlan(page string, w Write) *Plan {
	f := ParseForm(page)
	pl := &Plan{Action: w.Action, post: url.Values{}}
	for _, h := range f.hidden {
		pl.post.Add(h[0], h[1])
	}
	vals := map[string]string{}
	for k, v := range w.Values {
		if v = strings.TrimSpace(v); v != "" {
			vals[k] = v
		}
	}
	if w.Action == "left" { // only the leaving date and status change
		vals = map[string]string{FieldLeft: vals[FieldLeft], FieldStatus: "left"}
	}
	used := map[string]bool{}
	for _, fl := range f.Fields {
		if fl.Kind == "hidden" {
			continue
		}
		v, has := vals[fl.Role]
		if fl.Role == "" {
			has = false
		}
		row := PlanRow{Name: fl.Name, Label: fl.Label, Required: fl.Required}
		listed := true
		if has {
			row.Role = fl.Role
			used[fl.Role] = true
		}
		switch fl.Kind {
		case "select", "radio":
			cur := fl.Value
			if has {
				want := v
				if fl.Role == FieldStatus {
					want = ""
					for _, o := range fl.Options {
						if reLeftChoice.MatchString(o.Text) || reLeftChoice.MatchString(o.Value) {
							want = o.Value
							break
						}
					}
				}
				o, ok := pickOption(fl, want)
				if !ok {
					listed = false
					pl.Problems = append(pl.Problems, fmt.Sprintf("%s: the portal's list has no %q", nonEmpty(fl.Label, FieldLabels[fl.Role]), v))
				}
				cur = o.Value
			}
			row.Value = cur
			for _, o := range fl.Options {
				if o.Value == cur {
					row.Shown = o.Text
				}
			}
			if fl.Kind == "radio" && cur == "" {
				break
			}
			pl.post.Set(fl.Name, cur)
		case "checkbox":
			on := fl.Checked
			if has && fl.Role == FieldStatus { // an "Active" box is cleared, a "Left/Resigned" box is ticked
				on = reLeftChoice.MatchString(fl.Label + " " + fl.Name)
			}
			if on {
				val := fl.Value
				if val == "" {
					val = "on"
				}
				pl.post.Add(fl.Name, val)
				row.Value = "ticked"
			} else {
				row.Value = "not ticked"
			}
		default:
			cur := fl.Value
			if has {
				cur = v
				if fl.Kind == "date" || fl.Role == FieldDOB || fl.Role == FieldDOJ || fl.Role == FieldLeft {
					cur = portalDate(v)
				}
			}
			row.Value = cur
			pl.post.Add(fl.Name, cur)
		}
		if listed && row.Required && (row.Value == "" || fl.Kind == "select" && placeholder(row.Shown)) {
			pl.Problems = append(pl.Problems, fmt.Sprintf("%s: the portal requires it and the HRMS has nothing to send", nonEmpty(fl.Label, fl.Name)))
		}
		pl.Rows = append(pl.Rows, row)
	}
	for _, k := range sortedKeys(vals) {
		if !used[k] && k != FieldStatus {
			pl.Unused = append(pl.Unused, FieldLabels[k])
		}
	}
	must := map[string][]string{"create": {FieldCode, FieldName}, "update": {FieldName}, "left": {}}[w.Action]
	for _, r := range must {
		if !used[r] {
			pl.Problems = append(pl.Problems, fmt.Sprintf("the portal's form has no place for the %s", strings.ToLower(FieldLabels[r])))
		}
	}
	if w.Action == "left" && !used[FieldLeft] && !used[FieldStatus] {
		pl.Problems = append(pl.Problems, "the portal's form has no leaving date or status to mark the employee left")
	}
	if w.Action != "create" {
		if c := f.FindRole(FieldCode); c != nil && rtNormCode(c.Value) != rtNormCode(w.Code) {
			pl.Problems = append(pl.Problems, fmt.Sprintf("the portal's form opened with code %q, not %q", c.Value, w.Code))
		}
	}
	for _, b := range f.Buttons {
		for _, cap := range Spec.SaveCaptions {
			if b.Caption == cap && pl.button == "" && !neverPress.MatchString(norm(b.Caption+" "+b.Name)) {
				pl.button, pl.Button = b.Name, b.Caption
			}
		}
	}
	if pl.button == "" {
		pl.Problems = append(pl.Problems, "the portal's form has no save button")
	} else {
		pl.post.Set(pl.button, pl.Button)
	}
	pl.Hash = planHash(pl)
	return pl
}

// planHash fingerprints what a plan posts (the visible controls and the button; not the view state, which changes
// on every opening of the page).
func planHash(pl *Plan) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s\n", pl.Action, pl.Page, pl.button)
	for _, r := range pl.Rows {
		fmt.Fprintf(h, "%s=%s\n", r.Name, r.Value)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func rtNormCode(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if t := strings.TrimLeft(s, "0"); t != "" {
		return t
	}
	if s != "" {
		return "0"
	}
	return ""
}

func nonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// open signs in and opens the write's form (GET).
func (c *Client) open(ctx context.Context, w Write) (string, string, error) {
	if !Spec.confirmed() {
		return "", "", ErrFormNotConfirmed
	}
	path, err := Spec.pagePath(w.Action, w.Code)
	if err != nil {
		return "", "", err
	}
	if err := c.SignIn(ctx); err != nil {
		return "", "", err
	}
	res, b, err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return "", "", err
	}
	if res.StatusCode != http.StatusOK || !writePath(res.Request.URL.Path) {
		return "", "", fmt.Errorf("the portal's employee form did not open (%d)", res.StatusCode)
	}
	return string(b), path, nil
}

// Preview signs in, opens the form and returns what the write would post. Nothing is posted.
func (c *Client) Preview(ctx context.Context, w Write) (*Plan, error) {
	page, path, err := c.open(ctx, w)
	if err != nil {
		return nil, err
	}
	pl := buildPlan(page, w)
	pl.Page = path
	pl.Hash = planHash(pl)
	return pl, nil
}

// CreateEmployee adds a new employee on the portal. expect is the Hash of the preview HR confirmed.
func (c *Client) CreateEmployee(ctx context.Context, code string, values map[string]string, expect string) (*Result, error) {
	return c.send(ctx, Write{Action: "create", Code: code, Values: values}, expect)
}

// UpdateEmployee changes an employee's details on the portal.
func (c *Client) UpdateEmployee(ctx context.Context, code string, values map[string]string, expect string) (*Result, error) {
	return c.send(ctx, Write{Action: "update", Code: code, Values: values}, expect)
}

// MarkLeft marks an employee left on the portal (leaving date yyyy-mm-dd).
func (c *Client) MarkLeft(ctx context.Context, code, leftOn, expect string) (*Result, error) {
	return c.send(ctx, Write{Action: "left", Code: code, Values: map[string]string{FieldLeft: leftOn}}, expect)
}

// autoSend marks an automatic write (api/portal_auto.go, user, 19 Nov 2026): the HRMS sends on its own after a
// joining, a change of details or an exit, so there is no preview to compare with. Every other check stays: the
// form is opened afresh, any problem stops the write before the post, and the post is made once, never retried.
const autoSend = "\x00auto"

// Post makes the write w once without a preview (the automatic sending). See send.
func (c *Client) Post(ctx context.Context, w Write) (*Result, error) {
	if w.Action == "left" && w.Values[FieldLeft] == "" {
		return nil, errors.New("no leaving date")
	}
	return c.send(ctx, w, autoSend)
}

// send opens the form afresh, checks the plan is the previewed one and has no problem, and posts it once.
func (c *Client) send(ctx context.Context, w Write, expect string) (*Result, error) {
	page, path, err := c.open(ctx, w)
	if err != nil {
		return nil, err
	}
	pl := buildPlan(page, w)
	pl.Page = path
	pl.Hash = planHash(pl)
	if len(pl.Problems) > 0 {
		return nil, errors.New(strings.Join(pl.Problems, "; "))
	}
	if expect != autoSend && (expect == "" || pl.Hash != expect) {
		return nil, ErrChanged
	}
	res, b, err := c.do(ctx, http.MethodPost, path, pl.post, nil)
	out := &Result{Sent: pl.Rows, At: time.Now()}
	if err != nil {
		// Sent or not is unknown: never retried; HR checks the portal.
		out.Unclear, out.Message = true, err.Error()
		return out, nil
	}
	reply := string(b)
	out.Message = Message(reply)
	switch ok, known := replyOK(out.Message); {
	case known:
		out.OK = ok
	case res.StatusCode == http.StatusOK && strings.EqualFold(res.Request.URL.Path, "/employeelist.aspx"):
		out.OK = true // the portal went back to the employee list: saved
		if out.Message == "" {
			out.Message = "Saved (the portal went back to the employee list)"
		}
	case res.StatusCode >= 400:
		out.Message = fmt.Sprintf("the portal answered %d %s", res.StatusCode, out.Message)
	default:
		out.Unclear = true
		if out.Message == "" {
			out.Message = "The portal's reply said neither saved nor refused"
		}
	}
	return out, nil
}

// ReadForm signs in, opens the new-employee form (GET only) and returns its controls: the HRMS's mapping page takes
// the portal's departments, designations, branches and shifts from it.
func (c *Client) ReadForm(ctx context.Context) (*Form, error) {
	page, _, err := c.open(ctx, Write{Action: "create"})
	if err != nil {
		return nil, err
	}
	return ParseForm(page), nil
}

// Confirmed reports whether the employee form's pages are set (Spec).
func Confirmed() bool { return Spec.confirmed() }
