// Package rtfake is a made-up Realtime portal for tests of the employee writes (internal/realtime empwrite.go and
// api/portal_write.go). It mirrors the live portal's pages as read on 25 Nov 2026 (GETs only): the sign-in, the
// landing page, the employee list (grid2: EmpName, Cardno, EmpCode, Dept_Name, Desig_Name, pwd, Branch; each row
// links to Employee.aspx?RowId=<row>) and the employee form Employee.aspx?RowId=<row> (RowId=0 = the blank form of
// the list's Add button; no RowId = a server error), with the live form's control names, lists and buttons: the
// master page's OTP boxes, "Auto Approved Gps Punch", the branch-access ticks, the device buttons (Connect, upload
// Name, Enable/Disable/Delete User) and Save/Cancel. The view state changes on every opening and there is no event
// validation, as on the live page. It records every post and fails the test when anything but Save is pressed,
// when the view state is stale, or when any other page is asked for. Only tests import it.
//
// What the live portal answers after Save is not known (nothing was ever posted to it): the fake answers with an
// alert by default, or with the form and no message (Silent), so both ways of telling a save are tested.
package rtfake

import (
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/saltstayz/hrms/internal/realtime"
)

// Spec is the live portal's form (realtime.Spec) with sending switched on.
var Spec = func() realtime.FormSpec { s := realtime.Spec; s.Send = true; return s }()

// Emp is an employee on the fake portal (control name → value as posted).
type Emp map[string]string

// Portal is the fake.
type Portal struct {
	T      *testing.T
	Srv    *httptest.Server
	mu     sync.Mutex
	Emps   map[string]Emp // by EmpCode as posted
	rows   map[string]string
	next   int
	Posts  []map[string]string
	Opened []string // the RowIds of the forms opened
	vs     string
	n      int
	// Refuse, when set, is the alert the next save answers with (a portal validation message).
	Refuse string
	// Silent: a save answers with the form and no message (the HRMS then looks on the list or the form).
	Silent bool
	// Drop: the next save answers as saved but keeps nothing.
	Drop bool
}

const pre = "ctl00$MainContent$"

// New starts the fake; it closes with the test.
func New(t *testing.T) *Portal {
	p := &Portal{T: t, Emps: map[string]Emp{}, rows: map[string]string{}, next: 167300}
	p.Srv = httptest.NewServer(p)
	t.Cleanup(p.Srv.Close)
	return p
}

// Config is the client configuration for the fake.
// Remove deletes the employee with this code from the fake, as HR deleting them on the portal would.
func (p *Portal) Remove(code string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.Emps, code)
	for r, c := range p.rows {
		if c == code {
			delete(p.rows, r)
		}
	}
}

func (p *Portal) Config() realtime.Config {
	return realtime.Config{BaseURL: p.Srv.URL, User: "hr", Pass: "secret"}
}

// PostCount is how many employee-form posts the fake received.
func (p *Portal) PostCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.Posts)
}

// Add puts an employee on the fake portal (as if HR had keyed them in) and returns their RowId.
func (p *Portal) Add(code, name string, more Emp) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := Emp{pre + "Txtpaycode": code, pre + "Txtempname": name, pre + "Txtcardno": pad8(code), pre + "cbodeptname": "7", pre + "cbodesignation": "6",
		pre + "cbobranchname": "10", pre + "cbocompnayname": "1", pre + "cboofficetimepolicy": "2", pre + "cboshiftname": "2", pre + "cboshifttype": "1", pre + "cboFWOff": "1"}
	for k, v := range more {
		e[k] = v
	}
	p.next++
	row := strconv.Itoa(p.next)
	p.rows[row] = code
	p.Emps[code] = e
	return row
}

func pad8(s string) string {
	for len(s) < 8 {
		s = "0" + s
	}
	return s
}

// The live form's lists (value=text), shortened.
var lists = map[string][]string{
	"DropDownList1":           {"=", "91=+91"},
	"Device_List":             {"Select Device=0", "RSS20230761581=RSS20230761581 / Gulmohar Marg"},
	"cboMobileAttendanceMode": {"No Use=No Use", "GPS Attendance=GPS Attendance", "Face Match=Face Match"},
	"CboGender":               {"Select Gender=Select Gender", "Male=Male", "Female=Female", "Other=Other"},
	"cbocompnayname":          {"1=Saltstayz"},
	"cbobranchname":           {"2=Saltstayz Golf Course Road", "8=Saltstayz Basil", "10=Corporate Office", "48=Saltstayz Gulmohar Marg"},
	"cbodeptname":             {"18=", "7=Centre", "4=Front Office", "3=Housekeeping", "2=Kitchen"},
	"cbodesignation":          {"3=UT", "6=Manager", "22=Associate", "1041=GSA", "1042=Commi I"},
	"cboofficetimepolicy":     {"2=GEN", "3=OPR"},
	"cboshifttype":            {"1=Fixed", "3=Rotational"},
	"cboshiftname":            {"2=GEN", "5=NST", "7=SFT"},
	"cboFWOff":                {"1=Sunday", "2=Monday", "3=Tuesday", "4=Wednesday", "5=Thursday", "6=Friday", "7=Saturday", "8=None"},
	"cboSWOtype":              {"1=None", "2=Half", "3=Full"},
	"cboSWOff":                {"1=Sunday", "7=Saturday", "8=None"},
}

// defaults of the blank form (RowId=0), as on the live page.
var defaults = Emp{pre + "Device_List": "Select Device", pre + "cboMobileAttendanceMode": "GPS Attendance", pre + "CboGender": "Select Gender",
	pre + "cbocompnayname": "1", pre + "cbobranchname": "2", pre + "cbodeptname": "18", pre + "cbodesignation": "3", pre + "cboofficetimepolicy": "2",
	pre + "TxtDOJ": "01/01/2026", pre + "txtshiftstartdate": "01/01/2026", pre + "cboshifttype": "1", pre + "cboshiftname": "2", pre + "cboFWOff": "1",
	pre + "cboSWOtype": "1", pre + "cboSWOff": "8"}

// The form's controls in page order: name (without the prefix for MainContent), the text before it, kind.
var controls = []struct{ name, label, kind string }{
	{"ctl00$txtemailid", "Email ID", "text"}, {"ctl00$DropDownList1", "Country Code", "select"}, {"ctl00$txtphoneno", "Phone No", "text"},
	{"ctl00$txtPhoneOtp", "Enter OTP", "text"}, {"ctl00$txtemailOTP", "Enter Email OTP", "text"},
	{"Device_List", "Select Devices", "select"}, {"Chknotification", "Allow Notifications", "checkbox"}, {"chkActiveFlag", "Auto Approved Gps Punch", "checkbox"},
	{"cboMobileAttendanceMode", "Mobile Attendance Mode", "select"}, {"Txtempname", "Name", "text"}, {"txtfartherhusname", "Father/Husband Name", "text"},
	{"Txtcardno", "(Biometric ID)", "text"}, {"Txtpaycode", "Employee Code", "text"}, {"txtpcardno", "Proximity Card No.", "text"},
	{"txtDateofBirth", "Date of Birth", "text"}, {"TxtPhoneNo", "Mobile No.", "text"}, {"TxtEmailAddress", "Email Address", "text"},
	{"Txtverifyno", "Govt. UID", "text"}, {"txtaddress", "Address", "textarea"}, {"CboGender", "Gender", "select"},
	{"cbocompnayname", "Company Name", "select"}, {"cbobranchname", "Branch Name", "select"}, {"cbodeptname", "Dept. Name", "select"},
	{"cbodesignation", "Designation", "select"}, {"cboofficetimepolicy", "Office Time Policy", "select"}, {"TxtDOJ", "Date Of Joining", "text"},
	{"txtshiftstartdate", "Shift Start Date", "text"}, {"txtreginnationdate", "Resignation Date", "text"},
	{"chkbranchccess$0", "Saltstayz Golf Course Road", "checkbox"}, {"chkbranchccess$1", "Saltstayz Basil", "checkbox"},
	{"cboshifttype", "Shift Type", "select"}, {"cboshiftname", "Shift Name", "select"}, {"cboFWOff", "First Weekly Off", "select"},
	{"cboSWOtype", "Second WO Type", "select"}, {"cboSWOff", "Second Weekly Off", "select"},
}

var buttons = [][2]string{{"ctl00$Btn_SendOTP", "Send OTP"}, {"ctl00$save", " VERIFY OTP"}, {pre + "Btnconnect", "Connect"}, {pre + "Btnuploadname", "upload Name"},
	{pre + "BtnEnableUser", "Enable User"}, {pre + "BtnDisableUser", "Disable User"}, {pre + "BtnDeleteUser", "Delete User"}, {pre + "deleteImg", ""},
	{pre + "btn_GetChatid", "Verify Token & Get Chat_Id"}, {pre + "cmdsave", "Save"}, {pre + "cmdcancel", "Cancel"}}

func full(n string) string {
	if strings.HasPrefix(n, "ctl00$") {
		return n
	}
	return pre + n
}

func (p *Portal) form(row string, e Emp) string {
	p.n++
	p.vs = fmt.Sprintf("vs-%d", p.n)
	var b strings.Builder
	fmt.Fprintf(&b, `<html><body><form method="post" action="./Employee.aspx?RowId=%s" id="form1" enctype="multipart/form-data">
<input type="hidden" name="__EVENTTARGET" id="__EVENTTARGET" value="" /><input type="hidden" name="__EVENTARGUMENT" id="__EVENTARGUMENT" value="" />
<input type="hidden" name="__LASTFOCUS" id="__LASTFOCUS" value="" /><input type="hidden" name="__VIEWSTATE" id="__VIEWSTATE" value="%s" />
<input type="hidden" name="__VIEWSTATEGENERATOR" id="__VIEWSTATEGENERATOR" value="1E0B1F3C" /><input name="ctl00$Hidden1" type="hidden" id="Hidden1" value="ab" />
<span id="MainContent_lblAddEditEmployeeDetails">Add/Edit Employee Details</span><span id="MainContent_lblmsg">Message: </span>
<span id="MainContent_msgdevice"></span><span id="MainContent_lblmessage" style="color:#FF3300;"></span><table>`, row, p.vs)
	for _, c := range controls {
		n := full(c.name)
		id := strings.ReplaceAll(strings.TrimPrefix(n, "ctl00$"), "$", "_")
		cur, has := e[n]
		if !has {
			cur = defaults[n]
		}
		fmt.Fprintf(&b, "<tr><td><span id=\"MainContent_lbl%s\">%s</span></td><td>", strings.NewReplacer("$", "", "ctl00", "").Replace(c.name), html.EscapeString(c.label))
		switch c.kind {
		case "select":
			fmt.Fprintf(&b, `<select name="%s" id="%s">`, n, id)
			for _, o := range lists[strings.TrimPrefix(c.name, "ctl00$")] {
				v, t, _ := strings.Cut(o, "=")
				sel := ""
				if v == cur {
					sel = ` selected="selected"`
				}
				fmt.Fprintf(&b, `<option%s value="%s">%s</option>`, sel, html.EscapeString(v), html.EscapeString(t))
			}
			b.WriteString(`</select>`)
		case "checkbox":
			chk := ""
			if cur != "" {
				chk = ` checked="checked"`
			}
			fmt.Fprintf(&b, `<input id="%s" type="checkbox" name="%s"%s />`, id, n, chk)
		case "textarea":
			fmt.Fprintf(&b, `<textarea name="%s" rows="2" id="%s">%s</textarea>`, n, id, html.EscapeString(cur))
		default:
			fmt.Fprintf(&b, `<input name="%s" type="text" value="%s" id="%s" class="form-control" />`, n, html.EscapeString(cur), id)
		}
		b.WriteString("</td></tr>\n")
	}
	b.WriteString("</table>")
	for _, bt := range buttons {
		fmt.Fprintf(&b, `<input type="submit" name="%s" value="%s" />`, bt[0], html.EscapeString(bt[1]))
	}
	// The live page's own script: alerts that are not a reply.
	b.WriteString(`<script type="text/javascript">function hi() { alert('hello'); }
function validate() { var t = document.getElementById("MainContent_Txtempname").value; if (t == "") { alert("Please Enter Employee Name"); return false; }
t = document.getElementById("MainContent_cbodeptname").value; if (t == "") { alert("Please Enter Department Name"); return false; } return true; }</script></form></body></html>`)
	return b.String()
}

func (p *Portal) list() string {
	var b strings.Builder
	b.WriteString(`<html><body><form method="post" action="./EmployeeList.aspx" id="form1"><input type="hidden" name="__VIEWSTATE" value="lst" />
<input type="submit" name="ctl00$MainContent$BtnAdd" value="Add" /><input type="submit" name="ctl00$MainContent$BtnDel" value="Delete" />
<input type="submit" name="ctl00$MainContent$Button2" value="Export Excel" />
<table id='grid2' cellspacing='0' rules='all' border='1'><thead><tr><th><input id='chkallop' type='checkbox' name='chkallop'></th><th>Photo</th><th>EmpName</th><th>Cardno</th><th>EmpCode</th><th>Dept_Name</th><th>Desig_Name</th><th>pwd</th><th>Branch</th><th> Salary Setup </th></tr></thead><tbody>`)
	ids := make([]string, 0, len(p.rows))
	for r := range p.rows {
		ids = append(ids, r)
	}
	sort.Strings(ids)
	text := func(list, v string) string {
		for _, o := range lists[list] {
			if k, t, _ := strings.Cut(o, "="); k == v {
				return t
			}
		}
		return ""
	}
	for _, r := range ids {
		e := p.Emps[p.rows[r]]
		if e == nil || e[pre+"txtreginnationdate"] != "" { // the list shows active employees only
			continue
		}
		a := func(s string) string {
			return fmt.Sprintf("<td><a href=Employee.aspx?RowId=%s> %s</a></td>", r, html.EscapeString(s))
		}
		fmt.Fprintf(&b, "<tr><td><input type='checkbox' id='chk1' name='chk1' value=%s /></td><td><img src='images/profile-icon.png' /></td>%s%s%s%s%s%s%s<td><a href='Paysetup_new.aspx?RowId=%s'>( NA ) Manage Salary</a></td></tr>",
			r, a(e[pre+"Txtempname"]), a(e[pre+"Txtcardno"]), a(e[pre+"Txtpaycode"]), a(text("cbodeptname", e[pre+"cbodeptname"])),
			a(text("cbodesignation", e[pre+"cbodesignation"])), a("DEVICE-PASSWORD"), a(text("cbobranchname", e[pre+"cbobranchname"])), r)
	}
	b.WriteString("</tbody></table></form></body></html>")
	return b.String()
}

func (p *Portal) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = r.ParseMultipartForm(1 << 20)
	_ = r.ParseForm()
	signed := func() bool { c, _ := r.Cookie("S"); return c != nil }
	switch r.Method + " " + r.URL.Path {
	case "GET /loginnew.aspx":
		fmt.Fprint(w, `<form><input type="hidden" name="__VIEWSTATE" value="l1" /><select name="Cbologinas"><option value="A">A</option></select>
			<input name="TextBox2" type="text" /><input name="TextBox3" type="password" /><input type="submit" name="Button1" value="Log In" />
			<input type="submit" name="Button2" value="Update software Database" /></form>`)
	case "POST /loginnew.aspx":
		if r.PostForm.Has("Button2") || r.PostForm.Get("TextBox3") != "secret" {
			fmt.Fprint(w, "<form>Invalid</form>")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "S", Value: "1", Path: "/"})
		http.Redirect(w, r, "/Welcome.aspx", http.StatusFound)
	case "GET /Welcome.aspx":
		fmt.Fprint(w, "<html>Welcome</html>")
	case "GET /EmployeeList.aspx":
		if !signed() {
			http.Redirect(w, r, "/Default.aspx", http.StatusFound)
			return
		}
		fmt.Fprint(w, p.list())
	case "GET /Employee.aspx":
		if !signed() {
			http.Redirect(w, r, "/Default.aspx", http.StatusFound)
			return
		}
		row, ok := r.URL.Query()["RowId"]
		if !ok || row[0] == "" {
			http.Error(w, "Object reference not set to an instance of an object.", http.StatusInternalServerError)
			return
		}
		p.Opened = append(p.Opened, row[0])
		e := Emp{}
		if row[0] != "0" {
			if e = p.Emps[p.rows[row[0]]]; e == nil {
				http.Error(w, "Object reference not set to an instance of an object.", http.StatusInternalServerError)
				return
			}
		}
		fmt.Fprint(w, p.form(row[0], e))
	case "POST /Employee.aspx":
		p.save(w, r)
	default:
		p.T.Errorf("fake portal: unexpected %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

func (p *Portal) save(w http.ResponseWriter, r *http.Request) {
	row := r.URL.Query().Get("RowId")
	f := map[string]string{}
	for k := range r.PostForm {
		f[k] = r.PostForm.Get(k)
	}
	p.Posts = append(p.Posts, f)
	for _, b := range buttons {
		if b[0] != pre+"cmdsave" && r.PostForm.Has(b[0]) {
			p.T.Errorf("fake portal: %q was pressed", b[1])
		}
	}
	if f[pre+"cmdsave"] != "Save" || f["__VIEWSTATE"] != p.vs || f["__EVENTVALIDATION"] != "" {
		p.T.Errorf("fake portal: bad post (button %q, view state %q of %q)", f[pre+"cmdsave"], f["__VIEWSTATE"], p.vs)
		http.Error(w, "bad post", http.StatusInternalServerError)
		return
	}
	reply := func(msg string) {
		out := p.form(row, Emp(f))
		if msg != "" && !p.Silent {
			out += `<script type="text/javascript">alert('` + msg + `');</script>`
		}
		fmt.Fprint(w, out)
	}
	if p.Refuse != "" {
		msg := p.Refuse
		p.Refuse = ""
		fmt.Fprint(w, p.form(row, Emp(f))+`<script type="text/javascript">alert('`+msg+`');</script>`)
		return
	}
	// The page's own checks (validate()), made again here.
	switch {
	case f[pre+"Txtempname"] == "":
		reply("Please Enter Employee Name")
		return
	case f[pre+"Txtpaycode"] == "":
		reply("Please Enter Employee Code.")
		return
	case len(f[pre+"Txtcardno"]) != 8:
		reply("Please Enter Card No.")
		return
	case f[pre+"cbodeptname"] == "18" || f[pre+"cbodeptname"] == "":
		reply("Please Enter Department Name")
		return
	}
	code := f[pre+"Txtpaycode"]
	e := Emp{}
	for k, v := range f {
		if strings.HasPrefix(k, pre) && k != pre+"cmdsave" {
			e[k] = v
		}
	}
	if p.Drop {
		p.Drop = false
		reply("Record Saved Successfully")
		return
	}
	if row == "0" {
		for _, c := range p.rows {
			if strings.TrimLeft(c, "0") == strings.TrimLeft(code, "0") {
				reply("Employee Code Already Exist")
				return
			}
		}
		p.next++
		row = strconv.Itoa(p.next)
		p.rows[row] = code
	} else if p.rows[row] == "" {
		reply("Record not found")
		return
	} else if old := p.rows[row]; old != code {
		delete(p.Emps, old)
		p.rows[row] = code
	}
	p.Emps[code] = e
	reply("Record Saved Successfully")
}
