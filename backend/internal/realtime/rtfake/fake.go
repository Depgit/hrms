// Package rtfake is a made-up Realtime portal for tests of the employee writes (internal/realtime empwrite.go and
// api/portal_write.go): the sign-in, the landing page, and an employee form (ASP.NET WebForms style: view state that
// changes on every opening, labels in table cells, RequiredFieldValidators as script, a list of departments, and the
// buttons Save, Delete, Upload Name in Device and Close). It records every post and fails the test when anything but
// Save is pressed or the view state is stale. Only tests import it.
package rtfake

import (
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/saltstayz/hrms/internal/realtime"
)

// Spec is where the fake's employee form is.
var Spec = realtime.FormSpec{AddPath: "/EmpMaster.aspx", EditPath: "/EmpMaster.aspx?EmpCode={code}", SaveCaptions: []string{"Save"}}

// Emp is an employee on the fake portal (field name → value as posted).
type Emp map[string]string

// Portal is the fake.
type Portal struct {
	T      *testing.T
	Srv    *httptest.Server
	mu     sync.Mutex
	Emps   map[string]Emp // by EmpCode
	Posts  []map[string]string
	Opened []string
	n      int
	vs     string
	open   string // the code of the employee whose form was opened last ("" = the new-employee form)
	// Refuse, when set, is the alert the next save answers with (a portal validation message).
	Refuse string
}

// New starts the fake; it closes with the test.
func New(t *testing.T) *Portal {
	p := &Portal{T: t, Emps: map[string]Emp{}}
	p.Srv = httptest.NewServer(p)
	t.Cleanup(p.Srv.Close)
	return p
}

// Config is the client configuration for the fake.
func (p *Portal) Config() realtime.Config {
	return realtime.Config{BaseURL: p.Srv.URL, User: "hr", Pass: "secret"}
}

// PostCount is how many employee-form posts the fake received.
func (p *Portal) PostCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.Posts)
}

const pre = "ctl00$MainContent$"

var fields = []struct{ id, label, kind string }{
	{"txtEmpCode", "Emp Code", "text"}, {"txtCardNo", "Card No", "text"}, {"txtProxCard", "Proximity Card No", "text"},
	{"txtEmpName", "Employee Name", "text"}, {"txtFatherName", "Father Name", "text"}, {"txtDOB", "Date Of Birth", "text"},
	{"txtDOJ", "Date Of Join", "text"}, {"ddlGender", "Gender", "select"}, {"txtMobile", "Phone No", "text"}, {"txtEmail", "Email ID", "text"},
	{"ddlDepartment", "Department", "select"}, {"ddlDesignation", "Designation", "select"}, {"ddlBranch", "Branch Name", "select"},
	{"ddlCompany", "Company Name", "select"}, {"ddlShift", "Shift Code", "select"}, {"txtShiftStart", "Shift Start Date", "text"},
	{"txtLeftDate", "Date Of Leaving", "text"}, {"ddlStatus", "Status", "select"},
}

var options = map[string][]string{
	"ddlGender":      {"0=--Select--", "M=Male", "F=Female"},
	"ddlDepartment":  {"0=--Select--", "1=Front Office", "2=Kitchen", "3=Housekeeping"},
	"ddlDesignation": {"0=--Select--", "11=GSA", "12=Commi I", "13=Room Attendant"},
	"ddlBranch":      {"0=--Select--", "21=SaltStayz Sector 29 Hub", "22=SaltStayz Saket"},
	"ddlCompany":     {"1=Saltstayz Hospitality"},
	"ddlShift":       {"0=--Select--", "GEN=GEN", "NGT=NIGHT"},
	"ddlStatus":      {"A=Active", "R=Resigned"},
}

var required = []string{"txtEmpCode", "txtEmpName", "txtDOJ", "ddlDepartment"}

func (p *Portal) form(e Emp) string {
	p.n++
	p.vs = fmt.Sprintf("vs-%d", p.n)
	var b strings.Builder
	fmt.Fprintf(&b, `<html><body><form method="post" action="./EmpMaster.aspx" id="form1"><input type="hidden" name="__EVENTTARGET" id="__EVENTTARGET" value="" />
<input type="hidden" name="__VIEWSTATE" id="__VIEWSTATE" value="%s" /><input type="hidden" name="__EVENTVALIDATION" value="ev-%d" /><table>`, p.vs, p.n)
	for _, f := range fields {
		id := "MainContent_" + f.id
		cur := e[pre+f.id]
		star := ""
		for _, r := range required {
			if r == f.id {
				star = ` <span style="color:red">*</span>`
			}
		}
		fmt.Fprintf(&b, "<tr><td class=\"lbl\">%s%s</td><td>", f.label, star)
		if f.kind == "select" {
			fmt.Fprintf(&b, `<select name="%s%s" id="%s">`, pre, f.id, id)
			for _, o := range options[f.id] {
				v, t, _ := strings.Cut(o, "=")
				sel := ""
				if v == cur {
					sel = ` selected="selected"`
				}
				fmt.Fprintf(&b, `<option%s value="%s">%s</option>`, sel, v, html.EscapeString(t))
			}
			b.WriteString(`</select>`)
		} else {
			fmt.Fprintf(&b, `<input name="%s%s" type="text" value="%s" id="%s" />`, pre, f.id, html.EscapeString(cur), id)
		}
		b.WriteString("</td></tr>\n")
	}
	b.WriteString(`</table><input type="submit" name="ctl00$MainContent$btnSave" value="Save" id="MainContent_btnSave" />
<input type="submit" name="ctl00$MainContent$btnDelete" value="Delete" /><input type="submit" name="ctl00$MainContent$btnUpload" value="Upload Name in Device" />
<input type="submit" name="ctl00$MainContent$btnClose" value="Close" /><span id="MainContent_lblMsg"></span><script type="text/javascript">`)
	for i, r := range required {
		fmt.Fprintf(&b, "var MainContent_RequiredFieldValidator%d = document.all ? document.all[\"MainContent_RequiredFieldValidator%d\"] : document.getElementById(\"MainContent_RequiredFieldValidator%d\");\n", i, i, i)
		fmt.Fprintf(&b, "MainContent_RequiredFieldValidator%d.controltovalidate = \"MainContent_%s\";\nMainContent_RequiredFieldValidator%d.evaluationfunction = \"RequiredFieldValidatorEvaluateIsValid\";\n", i, r, i)
	}
	b.WriteString("</script></form></body></html>")
	return b.String()
}

func (p *Portal) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
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
	case "GET /EmpMaster.aspx":
		if !signed() {
			http.Redirect(w, r, "/Default.aspx", http.StatusFound)
			return
		}
		code := r.URL.Query().Get("EmpCode")
		p.Opened = append(p.Opened, code)
		p.open = code
		e := Emp{}
		if code != "" {
			if e = p.Emps[code]; e == nil {
				fmt.Fprint(w, p.form(Emp{})+`<script>alert('Employee not found');</script>`)
				return
			}
		}
		fmt.Fprint(w, p.form(e))
	case "POST /EmpMaster.aspx":
		f := map[string]string{}
		for k := range r.PostForm {
			f[k] = r.PostForm.Get(k)
		}
		p.Posts = append(p.Posts, f)
		for _, b := range []string{"btnDelete", "btnUpload", "btnClose"} {
			if r.PostForm.Has(pre + b) {
				p.T.Errorf("fake portal: %s was pressed", b)
			}
		}
		if f[pre+"btnSave"] != "Save" || f["__VIEWSTATE"] != p.vs || f["__EVENTVALIDATION"] != fmt.Sprintf("ev-%d", p.n) {
			p.T.Errorf("fake portal: bad post (button %q, view state %q of %q)", f[pre+"btnSave"], f["__VIEWSTATE"], p.vs)
			http.Error(w, "bad post", http.StatusInternalServerError)
			return
		}
		reply := func(msg string) {
			fmt.Fprint(w, p.form(Emp(f))+`<script type="text/javascript">alert('`+msg+`');</script>`)
		}
		if p.Refuse != "" {
			msg := p.Refuse
			p.Refuse = ""
			reply(msg)
			return
		}
		for _, k := range required {
			if v := f[pre+k]; v == "" || v == "0" {
				reply("Please enter " + k)
				return
			}
		}
		code := f[pre+"txtEmpCode"]
		if p.open == "" && p.Emps[code] != nil {
			reply("Employee Code already exists")
			return
		}
		e := Emp{}
		for k, v := range f {
			if strings.HasPrefix(k, pre+"txt") || strings.HasPrefix(k, pre+"ddl") {
				e[k] = v
			}
		}
		p.Emps[code] = e
		reply("Record Saved Successfully")
	default:
		p.T.Errorf("fake portal: unexpected %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}
