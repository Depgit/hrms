package realtime

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

// The employee list page as the portal shows it (made-up data): a device list box, the Active / All / Resigned
// filter and seven buttons, of which only Export Excel may be pressed.
const empListPage = `<html><form method="post" action="./EmployeeList.aspx"><input type="hidden" name="__VIEWSTATE" value="e1" />
<select size="4" name="ctl00$MainContent$lstBoxTest" multiple="multiple"><option value="RSS1">1-RSS1 / Lobby - Online</option></select>
<input id="MainContent_RadioButton3" type="radio" name="ctl00$MainContent$dd" value="RadioButton3" onclick="x()" /><label>All</label>
<input id="MainContent_RadioButton1" type="radio" name="ctl00$MainContent$dd" value="RadioButton1" checked="checked" /><label>Active</label>
<input id="MainContent_RadioButton2" type="radio" name="ctl00$MainContent$dd" value="RadioButton2" /><label>Resigned Staff</label>
<input type="hidden" name="ctl00$MainContent$HiddenField1" value="" />
<input type="submit" name="ctl00$MainContent$BtnAdd" value="Add" /><input type="submit" name="ctl00$MainContent$BtnDel" value="Delete" onclick="javascript:return DeleteConfirm();" />
<input type="submit" name="ctl00$MainContent$Btn_uploadName" value="Upload Name in Device" /><input type="submit" name="ctl00$MainContent$BtnImportFromExcel" value="Import from Excel" />
<input type="submit" name="ctl00$MainContent$Button2" value="Export Excel" /><input type="submit" name="ctl00$MainContent$btnassinebatchshift" value="Assign Batch Shift" />
<input type="submit" name="ctl00$MainContent$Button1" value="Close" />
<table id='grid2'><tr><th>EmpName</th><th>Cardno</th><th>EmpCode</th><th>pwd</th></tr><tr><td>Asha Verma</td><td>00001050</td><td>1050</td><td>1234</td></tr></table>
</form></html>`

// The export (made-up people): a heading with a leading space, a name that is the card number, a quoted name with
// a comma, a phone with a country code, a placeholder date and trailing empty columns.
const empCSV = " EmpName,FatherName,EmpCode,CardNo,ProximityCardNo,EmailID,PhoneNo,Department,Designation,BranchName,OfficeTimePolicy,DateOfBirth,DateOfJoin,ShiftStartDate,ShiftCode,WeeklyOff,CompanyName,,\n" +
	"Asha Verma,Ram Verma,1050,00001050,,asha@example.com,9876500001,Front Office,GSA,SaltStayz Amara,GEN,21/04/1995,13/11/2024,13/11/2024,GEN,,Acme,,\n" +
	"00000007,,00000007,00000007,,,,Centre,Assistant Manager,Office,GEN,,01/01/1900,01/01/1900,GEN,,Acme,,\n" +
	"\"Kumar, Ravi\",,2201,00002201,,,+91 98765 00002,Kitchen,Commi I,Saltstayz Sage,GEN,03/02/1990,05/06/2023,05/06/2023,GEN,Sunday,Acme,,\n" +
	",,,,,,,,,,,,,,,,,,\n"

func TestParseEmployees(t *testing.T) {
	l, err := ParseEmployees([]byte("\xef\xbb\xbf" + empCSV))
	if err != nil {
		t.Fatal(err)
	}
	if len(l) != 3 {
		t.Fatalf("rows: %d %+v", len(l), l)
	}
	a := l[0]
	if a.Name != "Asha Verma" || a.Code != "1050" || a.Card != "00001050" || a.Phone != "9876500001" || a.DOB != "1995-04-21" || a.DOJ != "2024-11-13" ||
		a.Dept != "Front Office" || a.Desig != "GSA" || a.Branch != "SaltStayz Amara" || a.Father != "Ram Verma" {
		t.Fatalf("first: %+v", a)
	}
	if l[1].DOJ != "" || l[1].Phone != "" { // 01/01/1900 is no date
		t.Fatalf("placeholders: %+v", l[1])
	}
	if l[2].Name != "Kumar, Ravi" || l[2].Phone != "9876500002" {
		t.Fatalf("quoted: %+v", l[2])
	}
	if _, err := ParseEmployees([]byte("Name,Code\nA,1\n")); err == nil {
		t.Fatal("columns missing")
	}
}

func TestClientEmployeeList(t *testing.T) {
	fp := &fakePortal{t: t, password: "secret", empCSV: []byte(empCSV)}
	srv := httptest.NewServer(fp)
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, User: "hr", Pass: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.EmployeeList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if l, err := ParseEmployees(b); err != nil || len(l) != 3 {
		t.Fatal(err, len(l))
	}
	want := "GET /loginnew.aspx,POST /loginnew.aspx,GET /Welcome.aspx,GET /EmployeeList.aspx,POST /EmployeeList.aspx"
	if got := strings.Join(fp.paths, ","); got != want {
		t.Fatalf("requests:\n%s\nwant\n%s", got, want)
	}
	// A page without the Export Excel button is not pressed at all.
	if _, err := exportForm(strings.Replace(empListPage, `value="Export Excel"`, `value="Export"`, 1)); err == nil {
		t.Fatal("no export button")
	}
}
