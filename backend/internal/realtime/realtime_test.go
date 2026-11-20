package realtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

var ist = time.FixedZone("IST", 5*3600+1800)

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/rawpunch_fake.xls") // made-up names and codes
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParse(t *testing.T) {
	rows, err := Parse(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("rows: %d %+v", len(rows), rows)
	}
	r := rows[1]
	if r.EmpCode != "1050" || r.CardNo != "00001050" || r.Name != "Asha Test" || r.Department != "Front Office" || r.Date != "2026-11-13" ||
		strings.Join(r.Times, " ") != "09:53 12:07 19:15" {
		t.Fatalf("row: %+v", r)
	}
	if len(rows[0].Times) != 0 {
		t.Fatal("no swipes on row 1")
	}
	if strings.Join(rows[2].Times, " ") != "08:22 19:52" {
		t.Fatalf("8:22 is padded: %v", rows[2].Times)
	}
	if rows[4].EmpCode != "777" || strings.Join(rows[4].Times, " ") != "13:42" {
		t.Fatalf("after the repeated header, only valid times: %+v", rows[4])
	}
	if _, err := Parse([]byte("not excel")); err == nil {
		t.Fatal("not an xls")
	}
}

func TestSwipesPastMidnight(t *testing.T) {
	r := Row{Date: "2026-11-13", Times: []string{"22:10", "06:05"}}
	sw := r.Swipes(ist)
	if len(sw) != 2 || sw[0].Format("2006-01-02 15:04") != "2026-11-13 22:10" || sw[1].Format("2006-01-02 15:04") != "2026-11-14 06:05" {
		t.Fatalf("%v", sw)
	}
	r = Row{Date: "2026-11-13", Times: []string{"09:00", "13:00", "18:30"}}
	if sw = r.Swipes(ist); sw[2].Format("2006-01-02 15:04") != "2026-11-13 18:30" {
		t.Fatalf("%v", sw)
	}
	if (Row{Date: "bad", Times: []string{"09:00"}}).Swipes(ist) != nil {
		t.Fatal("no date, no swipes")
	}
}

func TestFromEnv(t *testing.T) {
	env := map[string]string{"REALTIME_URL": "http://203.0.113.10:83/", "REALTIME_USER": "u", "REALTIME_PASS": "p"}
	c, ok := FromEnv(func(k string) string { return env[k] })
	if !ok || c.BaseURL != "http://203.0.113.10:83" || c.Host() != "203.0.113.10:83" {
		t.Fatal(c, ok)
	}
	delete(env, "REALTIME_PASS")
	if _, ok := FromEnv(func(k string) string { return env[k] }); ok {
		t.Fatal("password missing")
	}
}

// fakePortal imitates the five pages the client reads, and fails the test if anything else is asked for.
type fakePortal struct {
	t        *testing.T
	xls      []byte
	mu       sync.Mutex
	paths    []string
	loaded   bool
	welcome  bool
	date     string
	password string
	empCSV   []byte
}

func (f *fakePortal) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	_ = r.ParseForm()
	for k := range r.PostForm {
		if k == "Button2" || strings.HasSuffix(k, "cmdsave") || strings.HasSuffix(k, "BtnAdd") || strings.HasSuffix(k, "btnClose") {
			f.t.Errorf("a button other than the report's was pressed: %s", k)
		}
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /loginnew.aspx":
		fmt.Fprint(w, `<form><input type="hidden" name="__VIEWSTATE" value="v1" /><select name="Cbologinas"><option value="Attendance &amp; Payroll">A</option></select>
			<input name="TextBox2" type="text" /><input name="TextBox3" type="password" /><input type="submit" name="Button1" value="Log In" />
			<input type="submit" name="Button2" value="Update software Database" /></form>`)
	case "POST /loginnew.aspx":
		if r.PostForm.Get("__VIEWSTATE") != "v1" || r.PostForm.Get("Cbologinas") != "Attendance & Payroll" || r.PostForm.Get("Button1") != "Log In" ||
			r.PostForm.Get("TextBox2") != "hr" || r.PostForm.Get("TextBox3") != f.password {
			fmt.Fprint(w, `<form>Invalid user</form>`)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "Application_SessionId", Value: "s1", Path: "/"})
		http.Redirect(w, r, "/Welcome.aspx", http.StatusFound)
	case "GET /Welcome.aspx":
		if c, _ := r.Cookie("Application_SessionId"); c != nil {
			f.welcome = true
		}
		fmt.Fprint(w, `<html>Dashboard</html>`)
	case "GET /Daily.aspx":
		if c, _ := r.Cookie("Application_SessionId"); c == nil {
			http.Redirect(w, r, "/Default.aspx", http.StatusFound)
			return
		}
		fmt.Fprint(w, `<html><form method="post"><input type="hidden" name="__VIEWSTATE" value="d1" />
			<input name="ctl00$MainContent$txtdate" type="text" value="16/11/2026" />
			<input id="x" type="radio" name="ctl00$MainContent$Cmpny" value="SelectAllCommany" checked="checked" />
			<input type="checkbox" name="ctl00$MainContent$CheckBox2$0" value="17" checked="checked" />
			<input type="checkbox" name="ctl00$MainContent$CheckBox2$1" value="40" />
			<input type="radio" name="ctl00$MainContent$Daily" value="optperfomence" checked="checked" />
			<input type="radio" name="ctl00$MainContent$Daily" value="AllIn_OutReport" />
			<select name="ctl00$MainContent$DropDownList1"><option value="1">1</option><option selected="selected" value="2">2</option></select>
			<input type="submit" name="ctl00$MainContent$cmdShowReport" value="ShowReport" /><input type="submit" name="ctl00$MainContent$btnClose" value="Close" /></form></html>`)
	case "POST /Daily.aspx":
		p := r.PostForm
		if p.Get("ctl00$MainContent$Daily") != "AllIn_OutReport" || p.Get("ctl00$MainContent$cmdShowReport") != "ShowReport" || p.Get("__VIEWSTATE") != "d1" ||
			p.Get("ctl00$MainContent$CheckBox2$0") != "17" || p.Has("ctl00$MainContent$CheckBox2$1") || p.Get("ctl00$MainContent$DropDownList1") != "2" ||
			p.Get("ctl00$MainContent$Cmpny") != "SelectAllCommany" {
			f.t.Errorf("daily form: %v", p)
		}
		f.date = p.Get("ctl00$MainContent$txtdate")
		http.Redirect(w, r, "/ReportViewer.aspx", http.StatusFound)
	case "GET /ReportViewer.aspx":
		if r.Header.Get("Referer") != "" || r.Header.Get("Accept-Encoding") != "identity" {
			f.t.Errorf("headers: %v", r.Header)
		}
		fmt.Fprint(w, `<form><input type="hidden" name="__VIEWSTATE" value="r1" /><input type="hidden" name="ctl00$MainContent$ReportViewer1$ctl10$ScrollPosition" /><input name="ctl00$Hidden1" type="hidden" value="20" />
			<span id="ctl00_MainContent_ReportViewer1_ctl10_Reserved_AsyncLoadTarget"></span>
			<script>Sys.WebForms.PageRequestManager._initialize('ctl00$ScriptManager1', 'form1');
			x({"ExportUrlBase":"/Reserved.ReportViewerWebControl.axd?ControlID=abc\u0026OpType=Export\u0026FileName=AllInout\u0026Format="})</script></form>`)
	case "POST /ReportViewer.aspx":
		if r.PostForm.Get("__EVENTTARGET") != "ctl00$MainContent$ReportViewer1$ctl10$Reserved_AsyncLoadTarget" || r.PostForm.Get("__VIEWSTATE") != "r1" ||
			r.Header.Get("X-MicrosoftAjax") != "Delta=true" {
			f.t.Errorf("async load: %v", r.PostForm)
		}
		f.loaded = true
		fmt.Fprint(w, "1|#||4|")
	case "GET /Reserved.ReportViewerWebControl.axd":
		if !f.loaded || !f.welcome || r.URL.Query().Get("Format") != "EXCEL" || r.URL.Query().Get("ControlID") != "abc" {
			http.Error(w, "no report session", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.ms-excel")
		_, _ = w.Write(f.xls)
	case "GET /EmployeeList.aspx":
		if c, _ := r.Cookie("Application_SessionId"); c == nil {
			http.Redirect(w, r, "/Default.aspx", http.StatusFound)
			return
		}
		fmt.Fprint(w, empListPage)
	case "POST /EmployeeList.aspx":
		p := r.PostForm
		var pressed []string
		for k := range p {
			for _, b := range []string{"BtnAdd", "BtnDel", "Btn_uploadName", "BtnImportFromExcel", "Button2", "btnassinebatchshift", "Button1"} {
				if strings.HasSuffix(k, "$"+b) {
					pressed = append(pressed, b)
				}
			}
		}
		if len(pressed) != 1 || pressed[0] != "Button2" || p.Get("ctl00$MainContent$Button2") != "Export Excel" || p.Get("__VIEWSTATE") != "e1" ||
			p.Get("ctl00$MainContent$dd") != "RadioButton1" || p.Has("ctl00$MainContent$lstBoxTest") {
			f.t.Errorf("employee list form: pressed %v, %v", pressed, p)
			http.Error(w, "bad form", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/text")
		w.Header().Set("Content-Disposition", "attachment;filename=TotalEmployee.csv")
		_, _ = w.Write(f.empCSV)
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

func TestClientReport(t *testing.T) {
	fp := &fakePortal{t: t, xls: fixture(t), password: "secret"}
	srv := httptest.NewServer(fp)
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, User: "hr", Pass: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Report(context.Background(), time.Date(2026, 11, 13, 0, 0, 0, 0, ist))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Parse(b)
	if err != nil || len(rows) != 5 {
		t.Fatal(err, len(rows))
	}
	if fp.date != "13/11/2026" {
		t.Fatalf("date sent: %q", fp.date)
	}
	want := "GET /loginnew.aspx,POST /loginnew.aspx,GET /Welcome.aspx,GET /Daily.aspx,POST /Daily.aspx,GET /ReportViewer.aspx,POST /ReportViewer.aspx,GET /Reserved.ReportViewerWebControl.axd"
	if got := strings.Join(fp.paths, ","); got != want {
		t.Fatalf("requests:\n%s\nwant\n%s", got, want)
	}

	// A wrong password.
	c2, _ := New(Config{BaseURL: srv.URL, User: "hr", Pass: "wrong"})
	if _, err := c2.Report(context.Background(), time.Now()); !errors.Is(err, ErrSignIn) {
		t.Fatalf("wrong password: %v", err)
	}
}

func TestClientRefusesOtherPages(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, User: "u", Pass: "p"})
	for _, p := range []string{"/updatedb.aspx", "/DailyAccessReport.aspx", "/Data_ForErp.aspx", "/backupdb.aspx", "http://example.com/loginnew.aspx"} {
		if _, _, err := c.do(context.Background(), http.MethodGet, p, nil, nil); !errors.Is(err, ErrNotAllowed) {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if hit {
		t.Fatal("a refused page was requested")
	}
	if _, err := New(Config{BaseURL: "not a url"}); err == nil {
		t.Fatal("bad address")
	}
}
