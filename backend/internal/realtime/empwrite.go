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

// Writing employees to the portal (user's decision, 17 Nov 2026; automatic since 19 Nov, api/portal_auto.go): create
// an employee, update their details and mark them left. Nothing retries: one write is one form post.
//
// The portal has no API for employees (its Third_PartyApi.aspx page only pushes punches out to another system), so a
// write fills the employee form as a browser would: open the form (GET), keep its hidden fields (view state), its
// other controls as the page has them, set the HRMS's details on the controls that take them (FormSpec.Fields), and
// press the form's Save button — the only button pressed. Preview does all of that except the post. A write found
// the employee's form through the employee list (GET), which also shows whether a new code is already taken; after
// the post the HRMS looks again (GET) to see that the portal kept what was sent.
//
// The form (25 Nov 2026, read on the live portal with GETs only): EmployeeList.aspx's Add button opens
// Employee.aspx?RowId=0 (confirmed by the owner, 25 Nov; the blank form: the page fails without a RowId), each row of the list links to
// Employee.aspx?RowId=<row>, and Save is ctl00$MainContent$cmdsave. Sending is on (Spec.Send: the owner approved it
// on 25 Nov 2026, README "Sending employees to the portal"); with it off, previews and the mapping lists still work.
//
// A create also picks the punch machine in the form's "Select Devices" list (Device_List, FieldDevice): the machine
// the machine pool plans for the person (api/machines.go), matched by its serial number (the option's value, or the
// start of its text "RSS… / Branch"). A machine the list does not have leaves the form's own choice and adds a note
// (Plan.Notes), never a problem. The form's device buttons (Connect, Upload Name, Enable/Disable/Delete User) are
// never pressed (neverPress).

// FormSpec says where the portal's employee form is, which button saves it and which control takes each detail.
type FormSpec struct {
	AddPath      string            // the new-employee form (GET)
	EditPath     string            // an employee's form: {row} = the RowId the employee list links them to, {code} = their EmpCode
	LeftPath     string            // where an employee is marked left; "" = EditPath
	ListPath     string            // the employee list (GET): finds {row} by code and shows whether a new code is taken; "" = not used
	SaveButton   string            // the save button's name; "" = any button with a SaveCaptions caption
	SaveCaptions []string          // the save button's exact captions
	Fields       map[string]string // detail (FieldCode…) → control name; empty = found by label (roleRules)
	Required     []string          // details the portal's own check (validate()) requires
	Send         bool              // sending switched on (owner's approval); off: preview and reading only
}

const mc = "ctl00$MainContent$"

// Spec is the live portal's employee form (Employee.aspx, read 25 Nov 2026). Sending is on (owner, 25 Nov 2026).
var Spec = FormSpec{
	AddPath:      "/Employee.aspx?RowId=0",
	EditPath:     "/Employee.aspx?RowId={row}",
	ListPath:     "/EmployeeList.aspx",
	SaveButton:   mc + "cmdsave",
	SaveCaptions: []string{"Save"},
	Fields: map[string]string{
		FieldCode: mc + "Txtpaycode", FieldCard: mc + "Txtcardno", FieldName: mc + "Txtempname", FieldFather: mc + "txtfartherhusname",
		FieldDOB: mc + "txtDateofBirth", FieldDOJ: mc + "TxtDOJ", FieldGender: mc + "CboGender", FieldPhone: mc + "TxtPhoneNo",
		FieldEmail: mc + "TxtEmailAddress", FieldCompany: mc + "cbocompnayname", FieldBranch: mc + "cbobranchname", FieldDept: mc + "cbodeptname",
		FieldDesig: mc + "cbodesignation", FieldPolicy: mc + "cboofficetimepolicy", FieldShift: mc + "cboshiftname", FieldWeeklyOff: mc + "cboFWOff",
		FieldShiftStart: mc + "txtshiftstartdate", FieldLeft: mc + "txtreginnationdate", FieldDevice: mc + "Device_List",
	},
	Required: []string{FieldName, FieldCode, FieldCard, FieldCompany, FieldDept, FieldDesig, FieldPolicy, FieldShift},
	Send:     true,
}

// neverPress are buttons never pressed, whatever their caption (compared lower case, without spaces).
var neverPress = regexp.MustCompile(`delete|remove|upload|import|export|transfer|gate|backup|database|devic|machine|clear|reset|close|cancel|batch|process|connect|enable|disable|token|otp|verify`)

// ErrFormNotConfirmed: the employee form's page is not set in Spec.
var ErrFormNotConfirmed = errors.New("the portal's employee form is not set (Spec in internal/realtime)")

// ErrSendOff: sending is not switched on (Spec.Send).
var ErrSendOff = errors.New("sending to the portal is not switched on yet (owner's approval; Spec.Send in internal/realtime)")

// ErrChanged: the form or the details changed after HR's preview.
var ErrChanged = errors.New("what would be sent is no longer what the preview showed; preview again")

// Write is one change to send.
type Write struct {
	Action string            // create, update, left
	Code   string            // the employee's EmpCode (update, left: which employee; create: the new code)
	Values map[string]string // FieldCode… → value (dates yyyy-mm-dd)
	Notes  []string          // plain-language notes for HR that do not stop the write (e.g. no punch machine chosen)
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
	Notes    []string  `json:"notes,omitempty"`    // for HR; never blocks the send (e.g. no punch machine chosen)
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
	Notes   []string  `json:"notes,omitempty"` // the plan's notes (Plan.Notes)
	At      time.Time `json:"at"`
}

// known: the form's pages are set (previews and reading the lists work).
func (s FormSpec) known() bool { return s.AddPath != "" && s.EditPath != "" }

// confirmed: the form is set and sending is switched on.
func (s FormSpec) confirmed() bool { return s.known() && s.Send }

// pagePath is the form's path for an action, with the employee's code or portal row filled in.
func (s FormSpec) pagePath(action, code, row string) (string, error) {
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
	if strings.Contains(p, "{row}") {
		if row == "" {
			return "", errors.New("the employee's row on the portal is not known")
		}
		p = strings.ReplaceAll(p, "{row}", url.QueryEscape(row))
	}
	return p, nil
}

// writePath reports whether a path is one of Spec's form pages.
func writePath(path string) bool {
	if !Spec.known() {
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

// parseSpecForm reads the employee form with the controls' details as Spec names them.
func parseSpecForm(page string) *Form {
	f := ParseForm(page)
	f.applyRoles(Spec.Fields)
	for i := range f.Fields {
		for _, r := range Spec.Required {
			if r != "" && f.Fields[i].Role == r {
				f.Fields[i].Required = true
			}
		}
	}
	return f
}

// reEmailOK is the portal's own check of an email address (Employee.aspx validate()).
var reEmailOK = regexp.MustCompile(`^([A-Za-z0-9_\-\.])+@([A-Za-z0-9_\-\.])+\.([A-Za-z]{2,4})$`)

var reAllDigits = regexp.MustCompile(`^[0-9]+$`)

// buildPlan fills the form with the write's details. Problems (and the ones found before, pre) block the send.
func buildPlan(page string, w Write, pre ...string) *Plan {
	f := parseSpecForm(page)
	pl := &Plan{Action: w.Action, post: url.Values{}, Problems: append([]string(nil), pre...), Notes: append([]string(nil), w.Notes...)}
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
	if w.Action != "create" {
		delete(vals, FieldDevice) // the punch machine is chosen on a create only
	}
	if w.Action == "create" && vals[FieldShiftStart] == "" && vals[FieldDOJ] != "" {
		vals[FieldShiftStart] = vals[FieldDOJ] // the shift starts on the day they join
	}
	var skipped []string
	if e := vals[FieldEmail]; e != "" && len(Spec.Fields) > 0 && !reEmailOK.MatchString(e) {
		delete(vals, FieldEmail) // the portal's form refuses it; the email is not required there
		skipped = append(skipped, FieldLabels[FieldEmail]+" ("+e+": the portal's form does not take this address)")
	}
	if c := vals[FieldCard]; c != "" && len(Spec.Fields) > 0 && (!reAllDigits.MatchString(c) || len(c) > 9) {
		pl.Problems = append(pl.Problems, fmt.Sprintf("Card number %q: the portal takes up to 9 digits", c))
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
			if has && fl.Role == FieldDevice {
				// The planned machine, by serial; not in the list: the form's own choice and a note (never a problem).
				if o, ok := pickDevice(fl, v); ok {
					cur = o.Value
				} else {
					row.Role, has = "", false
					delete(used, FieldDevice)
					pl.Notes = append(pl.Notes, DeviceNote("machine "+v+" is not in the portal form's list of devices"))
				}
			} else if has {
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
	if d := vals[FieldDevice]; d != "" && !used[FieldDevice] && !hasDeviceNote(pl.Notes) {
		pl.Notes = append(pl.Notes, DeviceNote("the portal's form has no list of devices"))
	}
	for _, k := range sortedKeys(vals) {
		if !used[k] && k != FieldStatus && k != FieldDevice {
			pl.Unused = append(pl.Unused, FieldLabels[k])
		}
	}
	pl.Unused = append(pl.Unused, skipped...)
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
		if Spec.SaveButton != "" && b.Name != Spec.SaveButton {
			continue
		}
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

// DeviceNote is the note when no punch machine is chosen on a create.
func DeviceNote(why string) string {
	return "No punch machine chosen: " + why + "; HR adds them to a machine on the portal"
}

func hasDeviceNote(notes []string) bool {
	for _, n := range notes {
		if strings.HasPrefix(n, "No punch machine chosen") {
			return true
		}
	}
	return false
}

// pickDevice is the choice of the devices list that is the machine with this serial number: the same value, or a
// text that starts with it ("RSS20230761581 / Gulmohar Marg"). The list's "Select Device" is never picked.
func pickDevice(fl FormField, serial string) (FormOption, bool) {
	s := fold(serial)
	if s == "" {
		return FormOption{}, false
	}
	for _, o := range fl.Options {
		if fold(o.Value) == s && !placeholder(o.Text) {
			return o, true
		}
	}
	for _, o := range fl.Options {
		t := fold(o.Text)
		if strings.HasPrefix(t, s) && (len(t) == len(s) || !isAlnum(t[len(s)])) && !placeholder(o.Text) {
			return o, true
		}
	}
	return FormOption{}, false
}

func isAlnum(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
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

// opened is the write's form as the portal showed it.
type opened struct {
	page, path string
	row        string   // the employee's RowId on the portal (update, left)
	problems   []string // found on the employee list (a create's code already taken)
}

// open signs in, finds the employee on the list (GET) and opens the write's form (GET).
func (c *Client) open(ctx context.Context, w Write) (*opened, error) {
	if !Spec.known() {
		return nil, ErrFormNotConfirmed
	}
	if err := c.SignIn(ctx); err != nil {
		return nil, err
	}
	o := &opened{}
	if Spec.ListPath != "" && (w.Action != "create" || w.Code != "") {
		rows, err := c.listRows(ctx)
		if err != nil {
			return nil, err
		}
		r := FindCode(rows, w.Code)
		switch {
		case w.Action != "create" && r == nil:
			return nil, fmt.Errorf("employee code %s is not on the portal's employee list (active employees)", w.Code)
		case w.Action != "create":
			o.row = r.RowID
		case r != nil:
			o.problems = append(o.problems, fmt.Sprintf("The portal already has employee code %s (%s, card %s, %s): not created again. If it is the same person, set their Attendance code to %s",
				r.Code, r.Name, r.Card, nonEmpty(r.Branch, "no branch"), r.Code))
		default:
			if card := w.Values[FieldCard]; card != "" {
				if r := FindCard(rows, card); r != nil {
					o.problems = append(o.problems, fmt.Sprintf("The portal already has card number %s (%s, code %s): not created again", r.Card, r.Name, r.Code))
				}
			}
		}
	}
	path, err := Spec.pagePath(w.Action, w.Code, o.row)
	if err != nil {
		return nil, err
	}
	res, b, err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK || !writePath(res.Request.URL.Path) {
		return nil, fmt.Errorf("the portal's employee form did not open (%d)", res.StatusCode)
	}
	o.page, o.path = string(b), path
	return o, nil
}

// Preview signs in, opens the form and returns what the write would post. Nothing is posted; it works while
// sending is off.
func (c *Client) Preview(ctx context.Context, w Write) (*Plan, error) {
	o, err := c.open(ctx, w)
	if err != nil {
		return nil, err
	}
	pl := buildPlan(o.page, w, o.problems...)
	pl.Page = o.path
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

// send opens the form afresh, checks the plan is the previewed one and has no problem, posts it once, and looks
// on the portal (GET) whether it kept what was sent.
func (c *Client) send(ctx context.Context, w Write, expect string) (*Result, error) {
	if !Spec.Send {
		return nil, ErrSendOff
	}
	o, err := c.open(ctx, w)
	if err != nil {
		return nil, err
	}
	pl := buildPlan(o.page, w, o.problems...)
	pl.Page = o.path
	pl.Hash = planHash(pl)
	if len(pl.Problems) > 0 {
		return nil, errors.New(strings.Join(pl.Problems, "; "))
	}
	if expect != autoSend && (expect == "" || pl.Hash != expect) {
		return nil, ErrChanged
	}
	res, b, err := c.do(ctx, http.MethodPost, o.path, pl.post, nil)
	out := &Result{Sent: pl.Rows, Notes: pl.Notes, At: time.Now()}
	if err != nil {
		// Sent or not is unknown: never retried; HR checks the portal.
		out.Unclear, out.Message = true, err.Error()
		return out, nil
	}
	reply := string(b)
	out.Message = NewMessage(o.page, reply)
	ok, known := replyOK(out.Message)
	kept, checked, how := c.check(ctx, w, o, pl)
	switch {
	case checked && kept:
		out.OK = true
		if out.Message == "" || (known && !ok) {
			out.Message = strings.TrimSpace(how + " " + out.Message)
		}
	case known && !ok:
		// Refused in the portal's words.
	case checked:
		out.Unclear = true
		out.Message = strings.TrimSpace(nonEmpty(out.Message, "The portal's reply said neither saved nor refused") + "; " + how)
	case known:
		out.OK = true
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

// check looks on the portal (GET) after a post: a created employee is on the employee list with the code sent; an
// updated or left one's form shows every value sent. checked is false when the look itself failed (or the form
// has no list to look in).
func (c *Client) check(ctx context.Context, w Write, o *opened, pl *Plan) (kept, checked bool, how string) {
	if Spec.ListPath == "" {
		return false, false, ""
	}
	if w.Action == "create" {
		rows, err := c.listRows(ctx)
		if err != nil {
			return false, false, ""
		}
		if r := FindCode(rows, w.Code); r != nil {
			return true, true, fmt.Sprintf("Saved: on the portal's employee list as %s (code %s)", r.Name, r.Code)
		}
		return false, true, "employee code " + w.Code + " is not on the portal's employee list"
	}
	path, err := Spec.pagePath(w.Action, w.Code, o.row)
	if err != nil {
		return false, false, ""
	}
	res, b, err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil || res.StatusCode != http.StatusOK {
		return false, false, ""
	}
	f := parseSpecForm(string(b))
	var differ []string
	for _, r := range pl.Rows {
		if r.Role == "" {
			continue
		}
		if fl := f.FindRole(r.Role); fl == nil || strings.TrimSpace(fl.Value) != strings.TrimSpace(r.Value) {
			differ = append(differ, nonEmpty(r.Label, FieldLabels[r.Role]))
		}
	}
	if len(differ) > 0 {
		return false, true, "the employee's form on the portal does not show " + strings.Join(differ, ", ")
	}
	return true, true, "Saved: the employee's form on the portal shows what was sent"
}

// ReadForm signs in, opens the new-employee form (GET only) and returns its controls: the HRMS's mapping page takes
// the portal's departments, designations, branches and shifts from it.
func (c *Client) ReadForm(ctx context.Context) (*Form, error) {
	o, err := c.open(ctx, Write{Action: "create"})
	if err != nil {
		return nil, err
	}
	return parseSpecForm(o.page), nil
}

// Confirmed reports whether the employee form is set and sending is switched on (Spec.Send).
func Confirmed() bool { return Spec.confirmed() }

// FormKnown reports whether the employee form is set: previews and the mapping lists work, sending may be off.
func FormKnown() bool { return Spec.known() }
