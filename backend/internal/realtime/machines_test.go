package realtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// machineListPage imitates the portal's MachineList.aspx grid (made-up serials and names).
const machineListPage = `<html><form method="post"><input type="hidden" name="__VIEWSTATE" value="m1" />
<table id="menu"><tr><td><a href="Welcome.aspx">Home</a></td></tr></table>
<div><table cellspacing="0" rules="all" class="table" border="1" id="MainContent_Grid1">
<tr><th scope="col">Select</th><th scope="col">Machine No</th><th scope="col">Machine Model</th><th scope="col">Machine Serial No</th><th scope="col">Device Name</th><th scope="col">Device In_Out Mode</th><th scope="col">Branch Name</th></tr>
<tr><td class="gItmTxt"><input id="c0" type="checkbox" name="ctl00$MainContent$Grid1$ctl02$cbRows" /></td><td><a href="MachineManagement.aspx?RowId=1">1</a></td><td>T304F Mini</td><td>RSSTEST0001</td><td>Front &amp; Back</td><td>In/Out</td><td>SaltStayz Alpha</td></tr>
<tr><td><input type="checkbox" name="x" /></td><td><a href="MachineManagement.aspx?RowId=2">2</a></td><td>RS 9W</td><td> RSSTEST0002 </td><td>Gate</td><td>In</td><td>Corporate Office</td></tr>
<tr><td colspan="7"></td></tr>
</table></div>
<input type="submit" name="ctl00$MainContent$Btnadd" value="Add" /><input type="submit" name="ctl00$MainContent$BtnDel" value="Delete" /></form></html>`

func TestParseMachines(t *testing.T) {
	l, err := ParseMachines([]byte(machineListPage))
	if err != nil {
		t.Fatal(err)
	}
	if len(l) != 2 {
		t.Fatalf("machines: %+v", l)
	}
	if m := l[0]; m.No != "1" || m.Model != "T304F Mini" || m.Serial != "RSSTEST0001" || m.Name != "Front & Back" || m.Mode != "In/Out" || m.Branch != "SaltStayz Alpha" || m.Online != nil {
		t.Fatalf("row 1: %+v", m)
	}
	if l[1].Serial != "RSSTEST0002" || l[1].Branch != "Corporate Office" {
		t.Fatalf("row 2: %+v", l[1])
	}
	// A status column, when the page has one, gives online/offline.
	withStatus := strings.Replace(strings.Replace(machineListPage, `<th scope="col">Branch Name</th>`, `<th scope="col">Branch Name</th><th>Status</th>`, 1),
		`<td>SaltStayz Alpha</td>`, `<td>SaltStayz Alpha</td><td>Online</td>`, 1)
	withStatus = strings.Replace(withStatus, `<td>Corporate Office</td>`, `<td>Corporate Office</td><td><img src="img/offline.png" /></td>`, 1)
	l, err = ParseMachines([]byte(withStatus))
	if err != nil || len(l) != 2 || l[0].Online == nil || !*l[0].Online || l[1].Online == nil || *l[1].Online {
		t.Fatalf("status: %v %+v", err, l)
	}
	if _, err := ParseMachines([]byte(`<table><tr><th>Name</th></tr><tr><td>x</td></tr></table>`)); err == nil {
		t.Fatal("no serial column")
	}
}

// The client signs in and opens MachineList.aspx with a GET; it never posts the page (no button is pressed).
func TestClientMachineList(t *testing.T) {
	fp := &fakePortal{t: t, password: "secret"}
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.URL.Path, "/MachineList.aspx") {
			paths = append(paths, r.Method+" "+r.URL.Path)
			if r.Method != http.MethodGet {
				t.Errorf("the machine list was posted")
			}
			if c, _ := r.Cookie("Application_SessionId"); c == nil {
				http.Redirect(w, r, "/Default.aspx", http.StatusFound)
				return
			}
			fmt.Fprint(w, machineListPage)
			return
		}
		fp.ServeHTTP(w, r)
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, User: "hr", Pass: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.MachineList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if l, err := ParseMachines(b); err != nil || len(l) != 2 {
		t.Fatal(err, l)
	}
	if strings.Join(fp.paths, ",") != "GET /loginnew.aspx,POST /loginnew.aspx,GET /Welcome.aspx" || strings.Join(paths, ",") != "GET /MachineList.aspx" {
		t.Fatalf("requests: %v %v", fp.paths, paths)
	}
	// The machine management page behind each row is not on the list.
	if _, _, err := c.do(context.Background(), http.MethodGet, "/MachineManagement.aspx?RowId=1", nil, nil); !errors.Is(err, ErrNotAllowed) {
		t.Fatal(err)
	}
}
