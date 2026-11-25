package realtime

import (
	"html"
	"regexp"
	"sort"
	"strings"
)

// Reading the portal's employee form (empwrite.go): every control of the page with its label, whether the page
// requires it, and, for a list, its choices. Pure functions over the page's HTML; nothing here talks to the portal.

// FormOption is one choice of a dropdown.
type FormOption struct {
	Value string `json:"value"`
	Text  string `json:"text"`
}

// FormField is one control of the employee form.
type FormField struct {
	Name     string       `json:"name"`            // the posted name (ctl00$MainContent$txtEmpName)
	ID       string       `json:"id,omitempty"`    // the element id (MainContent_txtEmpName)
	Label    string       `json:"label,omitempty"` // the text the page shows for it
	Kind     string       `json:"kind"`            // text, date, select, checkbox, radio, textarea, hidden
	Required bool         `json:"required,omitempty"`
	Value    string       `json:"value,omitempty"` // as the page has it (the selected choice for a list)
	Checked  bool         `json:"checked,omitempty"`
	Options  []FormOption `json:"options,omitempty"`
	Role     string       `json:"role,omitempty"` // the HRMS detail it takes (FieldCode…), "" for none
}

// FormButton is a button of the page. Only a save button (FormSpec.SaveCaptions) is ever pressed.
type FormButton struct {
	Name    string `json:"name"`
	Caption string `json:"caption"`
}

// Form is the parsed employee form.
type Form struct {
	Fields  []FormField  `json:"fields"`
	Buttons []FormButton `json:"buttons"`
	hidden  [][2]string  // the hidden inputs in page order (view state and friends)
}

// The HRMS details sent to the portal (Write.Values keys).
const (
	FieldCode   = "code"   // EmpCode
	FieldCard   = "card"   // CardNo
	FieldName   = "name"   // employee name
	FieldFather = "father" // father's name
	FieldDOB    = "dob"    // date of birth, yyyy-mm-dd (sent dd/mm/yyyy)
	FieldDOJ    = "doj"    // date of joining
	FieldGender = "gender" // Male, Female, Other
	FieldPhone  = "phone"
	FieldEmail  = "email"
	FieldDept   = "dept"   // the portal's department (as HR mapped it)
	FieldDesig  = "desig"  // the portal's designation
	FieldBranch = "branch" // the portal's branch
	FieldShift  = "shift"  // the portal's shift
	FieldLeft   = "left"   // the leaving date (mark left)
	FieldStatus = "status" // the employee's status on the portal (mark left: a "resigned/left" choice)
	// 25 Nov 2026, from the live form (Employee.aspx):
	FieldPolicy     = "policy"     // the portal's office time policy (as HR mapped the work policy)
	FieldWeeklyOff  = "wo"         // the portal's first weekly off (as HR mapped the work policy)
	FieldCompany    = "company"    // the portal's company (one choice on the live portal: left as the form has it)
	FieldShiftStart = "shiftstart" // the shift start date (a create sends the date of joining)
	// 25 Nov 2026 (owner): the punch machine a new employee's record goes on (the form's "Select Devices" list), the
	// machine's serial number as the machine pool plans it (api/machines.go). Create only.
	FieldDevice = "device"
)

// FieldLabels name the details for people.
var FieldLabels = map[string]string{FieldCode: "Employee code", FieldCard: "Card number", FieldName: "Name", FieldFather: "Father's name",
	FieldDOB: "Date of birth", FieldDOJ: "Date of joining", FieldGender: "Gender", FieldPhone: "Phone", FieldEmail: "Email",
	FieldDept: "Department", FieldDesig: "Designation", FieldBranch: "Branch", FieldShift: "Shift", FieldLeft: "Leaving date", FieldStatus: "Status",
	FieldPolicy: "Office time policy", FieldWeeklyOff: "Weekly off", FieldCompany: "Company", FieldShiftStart: "Shift start date",
	FieldDevice: "Punch machine"}

// roleRules find each detail's control by its label and name (lower case, letters and digits only). The first
// rule that matches a control wins and a detail takes one control; skip words keep look-alikes apart (the
// proximity card is not the card number, the shift start date is not the shift, the father's name is not the
// name).
var roleRules = []struct {
	role       string
	match, not *regexp.Regexp
}{
	{FieldFather, regexp.MustCompile(`father|fname|guardian`), nil},
	{FieldDOB, regexp.MustCompile(`birth|dob`), nil},
	{FieldLeft, regexp.MustCompile(`leav|left|resign|reliev|separat|dol`), regexp.MustCompile(`reason|type`)},
	{FieldDOJ, regexp.MustCompile(`join|doj`), nil},
	{FieldCard, regexp.MustCompile(`cardno|cardnumber|txtcard`), regexp.MustCompile(`proxim|rfid|pin|pass`)},
	{FieldCode, regexp.MustCompile(`empcode|employeecode|txtcode|empid|employeeid`), regexp.MustCompile(`old|prev|machine|device`)},
	{FieldEmail, regexp.MustCompile(`email|mail`), nil},
	{FieldPhone, regexp.MustCompile(`phone|mobile|contact|cell`), regexp.MustCompile(`emergency`)},
	{FieldGender, regexp.MustCompile(`gender|sex`), nil},
	{FieldDept, regexp.MustCompile(`depart|dept`), nil},
	{FieldDesig, regexp.MustCompile(`desig`), nil},
	{FieldBranch, regexp.MustCompile(`branch|location|site`), nil},
	{FieldShift, regexp.MustCompile(`shift`), regexp.MustCompile(`start|date|batch|rotat|weekly`)},
	{FieldStatus, regexp.MustCompile(`status|active`), regexp.MustCompile(`marital|marriage|pf|esi|shift|device`)},
	{FieldName, regexp.MustCompile(`empname|employeename|txtname|^name|fullname`), regexp.MustCompile(`father|company|branch|depart|desig|shift|user|login`)},
}

var (
	reTextarea  = regexp.MustCompile(`(?is)<textarea\b([^>]*)>(.*?)</textarea>`)
	reSelectAll = regexp.MustCompile(`(?is)<select\b([^>]*)>(.*?)</select>`)
	reOptionAll = regexp.MustCompile(`(?is)<option\b([^>]*)>(.*?)</option>`)
	reLabelFor  = regexp.MustCompile(`(?is)<label\b[^>]*\bfor="([^"]+)"[^>]*>(.*?)</label>`)
	reID        = reAttr("id")
	reNotWord   = regexp.MustCompile(`[^a-z0-9]+`)
	reAlert     = regexp.MustCompile(`(?is)alert\(\s*['"](.*?)['"]\s*\)`)
	reMsgSpan   = regexp.MustCompile(`(?is)<(?:span|div|label)\b[^>]*\bid="([^"]*(?:lbl|label|msg|message|error|status|result|summary)[^"]*)"[^>]*>(.*?)</(?:span|div|label)>`)
	// ASP.NET validators name the control they check: as an expando script (X.controltovalidate = "…" and
	// X.evaluationfunction = "…") or as attributes of the validator's span (requiredIDs).
	reEvalFn  = regexp.MustCompile(`(?is)([A-Za-z0-9_]+)\.evaluationfunction\s*=\s*"([^"]+)"`)
	reCtrlVal = regexp.MustCompile(`(?is)([A-Za-z0-9_]+)\.controltovalidate\s*=\s*"([^"]+)"`)
)

func norm(s string) string { return reNotWord.ReplaceAllString(strings.ToLower(s), "") }

func textOf(s string) string {
	return strings.TrimSpace(reSpace.ReplaceAllString(html.UnescapeString(reTag.ReplaceAllString(s, " ")), " "))
}

// ParseForm reads the employee form page.
func ParseForm(page string) *Form {
	if i := strings.Index(strings.ToLower(page), "<form"); i >= 0 {
		page = page[i:]
	}
	f := &Form{}
	labels := map[string]string{}
	for _, m := range reLabelFor.FindAllStringSubmatch(page, -1) {
		labels[html.UnescapeString(m[1])] = textOf(m[2])
	}
	required := requiredIDs(page)
	type at struct {
		pos int
		fl  FormField
	}
	var all []at
	seenRadio := map[string]int{}
	label := func(id string, pos int) string {
		if l := labels[id]; l != "" {
			return strings.TrimSuffix(strings.TrimSpace(l), ":")
		}
		return labelBefore(page, pos)
	}
	for _, loc := range reInput.FindAllStringIndex(page, -1) {
		t := page[loc[0]:loc[1]]
		n, ok := attr(reName, t)
		if !ok {
			continue
		}
		ty, _ := attr(reType, t)
		ty = strings.ToLower(ty)
		if ty == "" {
			ty = "text"
		}
		v, _ := attr(reValue, t)
		id, _ := attr(reID, t)
		switch ty {
		case "submit", "button", "image":
			f.Buttons = append(f.Buttons, FormButton{Name: n, Caption: strings.TrimSpace(v)})
			continue
		case "reset", "file":
			continue
		case "hidden":
			f.hidden = append(f.hidden, [2]string{n, v})
			continue
		}
		fl := FormField{Name: n, ID: id, Kind: ty, Value: v, Required: required[id] || reqAttr(t)}
		if ty == "checkbox" || ty == "radio" {
			fl.Checked = reChecked.MatchString(t)
		}
		if ty == "radio" { // one field per group, its choices as options
			opt := FormOption{Value: v, Text: label(id, loc[0])}
			if i, ok := seenRadio[n]; ok {
				all[i].fl.Options = append(all[i].fl.Options, opt)
				if fl.Checked {
					all[i].fl.Value, all[i].fl.Checked = v, true
				}
				continue
			}
			fl.Options = []FormOption{opt}
			if !fl.Checked {
				fl.Value = ""
			}
			fl.Label = label(id, loc[0])
			seenRadio[n] = len(all)
			all = append(all, at{loc[0], fl})
			continue
		}
		if ty == "text" && (strings.Contains(norm(n+id), "date") || strings.Contains(norm(n+id), "dob") || strings.Contains(norm(n+id), "doj")) {
			fl.Kind = "date"
		}
		fl.Label = label(id, loc[0])
		all = append(all, at{loc[0], fl})
	}
	for _, loc := range reSelectAll.FindAllStringSubmatchIndex(page, -1) {
		tag, body := page[loc[2]:loc[3]], page[loc[4]:loc[5]]
		n, ok := attr(reName, tag)
		if !ok {
			continue
		}
		id, _ := attr(reID, tag)
		fl := FormField{Name: n, ID: id, Kind: "select", Required: required[id] || reqAttr(tag)}
		first, sel := "", ""
		for i, o := range reOptionAll.FindAllStringSubmatch(body, -1) {
			v, has := attr(reValue, "<x "+o[1]+">")
			txt := textOf(o[2])
			if !has {
				v = txt
			}
			fl.Options = append(fl.Options, FormOption{Value: v, Text: txt})
			if i == 0 {
				first = v
			}
			if sel == "" && reSelected.MatchString(o[1]) {
				sel = v
			}
		}
		if sel == "" {
			sel = first
		}
		fl.Value = sel
		fl.Label = label(id, loc[0])
		all = append(all, at{loc[0], fl})
	}
	for _, loc := range reTextarea.FindAllStringSubmatchIndex(page, -1) {
		tag, body := page[loc[2]:loc[3]], page[loc[4]:loc[5]]
		n, ok := attr(reName, "<x "+tag+">")
		if !ok {
			continue
		}
		id, _ := attr(reID, "<x "+tag+">")
		all = append(all, at{loc[0], FormField{Name: n, ID: id, Kind: "textarea", Value: html.UnescapeString(body), Required: required[id], Label: label(id, loc[0])}})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].pos < all[j].pos })
	taken := map[string]bool{}
	for _, a := range all {
		fl := a.fl
		if fl.Kind != "hidden" {
			fl.Role = roleOf(fl, taken)
			if fl.Role != "" {
				taken[fl.Role] = true
			}
		}
		f.Fields = append(f.Fields, fl)
	}
	return f
}

// applyRoles gives the controls their details by their exact names (FormSpec.Fields, the live form's controls)
// instead of by label: every other control (the master page's OTP and search boxes, the "Auto Approved Gps Punch"
// tick, the branch-access ticks…) takes none.
func (f *Form) applyRoles(names map[string]string) {
	if len(names) == 0 {
		return
	}
	byName := map[string]string{}
	for role, n := range names {
		byName[n] = role
	}
	for i := range f.Fields {
		f.Fields[i].Role = byName[f.Fields[i].Name]
	}
}

// roleOf is the detail a control takes, by its label first, then its name.
func roleOf(fl FormField, taken map[string]bool) string {
	for _, key := range []string{norm(fl.Label), norm(fl.ID + " " + fl.Name[strings.LastIndex(fl.Name, "$")+1:])} {
		if key == "" {
			continue
		}
		for _, r := range roleRules {
			if taken[r.role] || !r.match.MatchString(key) || (r.not != nil && r.not.MatchString(key)) {
				continue
			}
			if r.role == FieldStatus && fl.Kind != "select" && fl.Kind != "checkbox" && fl.Kind != "radio" {
				continue
			}
			return r.role
		}
	}
	return ""
}

// labelBefore is the last piece of text before the control (the table cell or span in front of it).
func labelBefore(page string, pos int) string {
	from := pos - 600
	if from < 0 {
		from = 0
	}
	chunk := page[from:pos]
	// Only what follows the previous control.
	for _, cut := range []string{"<input", "<select", "</select>", "<textarea", "</textarea>"} {
		if i := strings.LastIndex(strings.ToLower(chunk), cut); i >= 0 {
			if j := strings.Index(chunk[i:], ">"); j >= 0 {
				chunk = chunk[i+j+1:]
			}
		}
	}
	parts := regexp.MustCompile(`(?s)<[^>]*>`).Split(chunk, -1)
	for i := len(parts) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(reSpace.ReplaceAllString(html.UnescapeString(parts[i]), " ")); t != "" && t != "*" && t != ":" {
			return strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(t, "*"), ":"))
		}
	}
	return ""
}

func reqAttr(tag string) bool {
	return regexp.MustCompile(`(?i)\srequired(\s|=|/|>)`).MatchString(tag) || regexp.MustCompile(`(?i)aria-required="true"`).MatchString(tag)
}

// requiredIDs are the controls a RequiredFieldValidator checks.
func requiredIDs(page string) map[string]bool {
	out := map[string]bool{}
	// Unobtrusive: data-val-evaluationfunction="RequiredFieldValidatorEvaluateIsValid" on the validator span.
	for _, m := range regexp.MustCompile(`(?is)<span\b[^>]*>`).FindAllString(page, -1) {
		ctl, ok := attr(reAttr("data-val-controltovalidate"), m)
		if !ok {
			if c2, ok2 := attr(reAttr("controltovalidate"), m); ok2 {
				ctl, ok = c2, true
			}
		}
		if !ok {
			continue
		}
		fn, _ := attr(reAttr("data-val-evaluationfunction"), m)
		if fn == "" {
			fn, _ = attr(reAttr("evaluationfunction"), m)
		}
		id, _ := attr(reID, m)
		if strings.Contains(strings.ToLower(fn), "required") || strings.Contains(strings.ToLower(id), "required") || strings.Contains(strings.ToLower(id), "rfv") {
			out[ctl] = true
		}
	}
	// Expando script: X.controltovalidate = "…"; X.evaluationfunction = "RequiredFieldValidatorEvaluateIsValid";
	ctrl := map[string]string{}
	for _, m := range reCtrlVal.FindAllStringSubmatch(page, -1) {
		ctrl[m[1]] = m[2]
	}
	for _, m := range reEvalFn.FindAllStringSubmatch(page, -1) {
		if strings.Contains(strings.ToLower(m[2]), "required") && ctrl[m[1]] != "" {
			out[ctrl[m[1]]] = true
		}
	}
	return out
}

// FindRole is the control taking a detail, or nil.
func (f *Form) FindRole(role string) *FormField {
	for i := range f.Fields {
		if f.Fields[i].Role == role {
			return &f.Fields[i]
		}
	}
	return nil
}

// Message is what the portal says on a page after a save: a script alert, or the text of a message label or
// validation summary. "" when it says nothing.
func Message(page string) string {
	for _, m := range reAlert.FindAllStringSubmatch(page, -1) {
		if t := strings.TrimSpace(strings.NewReplacer(`\'`, "'", `\"`, `"`, `\n`, " ").Replace(html.UnescapeString(m[1]))); t != "" {
			return t
		}
	}
	var parts []string
	for _, m := range reMsgSpan.FindAllStringSubmatch(page, -1) {
		id := strings.ToLower(m[1])
		if strings.Contains(id, "validator") && !strings.Contains(id, "summary") {
			continue
		}
		t := textOf(m[2])
		if t == "" || len(t) > 400 {
			continue
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, " ")
}

// NewMessage is what the portal says on the page after a save that it did not say on the form as opened: the
// employee form carries its own script alerts ("Please Enter Employee Name"… inside validate()) and many label spans
// ("lblEmployeeName"), which are the page, not a reply. Alerts new to the reply, and message spans whose text
// changed, are the reply.
func NewMessage(before, after string) string {
	seen := map[string]int{}
	for _, m := range reAlert.FindAllStringSubmatch(before, -1) {
		seen[m[1]]++
	}
	for _, m := range reAlert.FindAllStringSubmatch(after, -1) {
		if seen[m[1]] > 0 {
			seen[m[1]]--
			continue
		}
		if t := strings.TrimSpace(strings.NewReplacer(`\'`, "'", `\"`, `"`, `\n`, " ").Replace(html.UnescapeString(m[1]))); t != "" {
			return t
		}
	}
	was := map[string]string{}
	for _, m := range reMsgSpan.FindAllStringSubmatch(before, -1) {
		was[m[1]] = textOf(m[2])
	}
	var parts []string
	for _, m := range reMsgSpan.FindAllStringSubmatch(after, -1) {
		id := strings.ToLower(m[1])
		if strings.Contains(id, "validator") && !strings.Contains(id, "summary") {
			continue
		}
		t := textOf(m[2])
		if t == "" || len(t) > 400 || t == was[m[1]] {
			continue
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, " ")
}

var (
	reGood = regexp.MustCompile(`(?i)\b(saved|save successfully|successful|successfully|added|inserted|updated|record (has been )?(saved|updated))\b`)
	reBad  = regexp.MustCompile(`(?i)\b(error|exist|exists|already|invalid|required|fail|failed|not|duplicate|cannot|can't|please|wrong|enter|select)\b`)
)

// replyOK tells a success message from a refusal.
func replyOK(msg string) (ok, known bool) {
	switch {
	case msg == "":
		return false, false
	case reBad.MatchString(msg):
		return false, true
	case reGood.MatchString(msg):
		return true, true
	}
	return false, false
}
