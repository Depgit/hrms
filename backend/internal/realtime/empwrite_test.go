package realtime_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/saltstayz/hrms/internal/realtime"
	"github.com/saltstayz/hrms/internal/realtime/rtfake"
)

func useFake(t *testing.T) *rtfake.Portal {
	old := realtime.Spec
	realtime.Spec = rtfake.Spec
	t.Cleanup(func() { realtime.Spec = old })
	return rtfake.New(t)
}

func row(pl *realtime.Plan, role string) realtime.PlanRow {
	for _, r := range pl.Rows {
		if r.Role == role {
			return r
		}
	}
	return realtime.PlanRow{}
}

const mc = "ctl00$MainContent$"

// Create, update and mark left against a fake of the live employee form (Employee.aspx?RowId=…, 25 Nov 2026): the
// preview posts nothing, the send posts the previewed form once (the page's current view state, only Save pressed),
// the employee's form is found on the employee list by code, and the portal's message comes back.
func TestEmployeeWrites(t *testing.T) {
	fp := useFake(t)
	ctx := context.Background()
	c, _ := realtime.New(fp.Config())
	fp.Add("2290", "Old Hand", nil)
	vals := map[string]string{realtime.FieldCode: "002301", realtime.FieldCard: "00002301", realtime.FieldName: "Asha Verma", realtime.FieldFather: "Ram Verma",
		realtime.FieldDOB: "1995-04-21", realtime.FieldDOJ: "2026-11-16", realtime.FieldGender: "Female", realtime.FieldPhone: "9876500001",
		realtime.FieldEmail: "asha@example.com", realtime.FieldDept: "Front Office", realtime.FieldDesig: "GSA", realtime.FieldBranch: "Saltstayz Basil",
		realtime.FieldShift: "GEN", realtime.FieldPolicy: "OPR", realtime.FieldWeeklyOff: "Tuesday"}
	pl, err := c.Preview(ctx, realtime.Write{Action: "create", Code: "002301", Values: vals})
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Problems) > 0 || fp.PostCount() != 0 || pl.Button != "Save" || pl.Page != "/Employee.aspx?RowId=0" {
		t.Fatalf("preview: %v posts %d button %q page %s", pl.Problems, fp.PostCount(), pl.Button, pl.Page)
	}
	for role, want := range map[string]string{realtime.FieldCode: "002301", realtime.FieldCard: "00002301", realtime.FieldName: "Asha Verma",
		realtime.FieldDOB: "21/04/1995", realtime.FieldDOJ: "16/11/2026", realtime.FieldShiftStart: "16/11/2026", realtime.FieldGender: "Female",
		realtime.FieldDept: "4", realtime.FieldDesig: "1041", realtime.FieldBranch: "8", realtime.FieldShift: "2", realtime.FieldPolicy: "3",
		realtime.FieldWeeklyOff: "3", realtime.FieldFather: "Ram Verma", realtime.FieldPhone: "9876500001", realtime.FieldEmail: "asha@example.com"} {
		if got := row(pl, role); got.Value != want {
			t.Fatalf("%s: %+v, want %s", role, got, want)
		}
	}
	for _, r := range pl.Rows { // the master page's boxes, the GPS tick and the look-alikes take nothing
		if r.Role != "" && !strings.HasPrefix(r.Name, mc) || strings.Contains(r.Name, "chkActiveFlag") && r.Role != "" || strings.Contains(r.Name, "txtpcardno") && r.Role != "" {
			t.Fatalf("wrong control taken: %+v", r)
		}
	}
	if row(pl, realtime.FieldCompany).Role != "" || !strings.Contains(rowByName(pl, mc+"cbocompnayname").Shown, "Saltstayz") {
		t.Fatalf("company: %+v", rowByName(pl, mc+"cbocompnayname"))
	}
	// A send with another hash is refused before anything is posted.
	if _, err := c.CreateEmployee(ctx, "002301", vals, "nope"); !errors.Is(err, realtime.ErrChanged) || fp.PostCount() != 0 {
		t.Fatalf("hash: %v %d", err, fp.PostCount())
	}
	res, err := c.CreateEmployee(ctx, "002301", vals, pl.Hash)
	if err != nil || !res.OK || res.Unclear || fp.PostCount() != 1 {
		t.Fatalf("create: %+v %v %d", res, err, fp.PostCount())
	}
	if e := fp.Emps["002301"]; e == nil || e[mc+"Txtempname"] != "Asha Verma" || e[mc+"cbodeptname"] != "4" || e[mc+"cboofficetimepolicy"] != "3" ||
		e[mc+"cbocompnayname"] != "1" || e[mc+"chkActiveFlag"] != "" || e[mc+"txtreginnationdate"] != "" {
		t.Fatalf("on the portal: %v", e)
	}

	// The code is on the portal's list now: a second create is stopped before the post (leading zeros ignored).
	pl2, err := c.Preview(ctx, realtime.Write{Action: "create", Code: "2301", Values: vals})
	if err != nil || len(pl2.Problems) != 1 || !strings.Contains(pl2.Problems[0], "already has employee code") {
		t.Fatalf("duplicate preview: %v %+v", err, pl2)
	}
	if _, err := c.Post(ctx, realtime.Write{Action: "create", Code: "2301", Values: vals}); err == nil || fp.PostCount() != 1 {
		t.Fatalf("duplicate posted: %v %d", err, fp.PostCount())
	}

	// Update: the employee's form found by code on the list, the new designation; the code and card stay.
	upd := map[string]string{}
	for k, v := range vals {
		upd[k] = v
	}
	delete(upd, realtime.FieldCode)
	delete(upd, realtime.FieldCard)
	upd[realtime.FieldDesig] = "Commi I"
	pl, err = c.Preview(ctx, realtime.Write{Action: "update", Code: "002301", Values: upd})
	if err != nil || len(pl.Problems) > 0 || row(pl, realtime.FieldDesig).Value != "1042" || !strings.HasPrefix(pl.Page, "/Employee.aspx?RowId=1673") {
		t.Fatalf("update preview: %v %+v", err, pl)
	}
	fp.Silent = true // no message: the HRMS looks at the employee's form again
	if res, err = c.UpdateEmployee(ctx, "002301", upd, pl.Hash); err != nil || !res.OK || !strings.Contains(res.Message, "shows what was sent") {
		t.Fatalf("update: %+v %v", res, err)
	}
	if e := fp.Emps["002301"]; e[mc+"cbodesignation"] != "1042" || e[mc+"Txtpaycode"] != "002301" || e[mc+"Txtcardno"] != "00002301" {
		t.Fatalf("after update: %v", e)
	}
	// A save that says nothing and kept nothing is unclear (HR looks), never "saved".
	upd[realtime.FieldDesig] = "Manager"
	fp.Drop = true
	if res, err = c.Post(ctx, realtime.Write{Action: "update", Code: "002301", Values: upd}); err != nil || res.OK || !res.Unclear {
		t.Fatalf("dropped update: %+v %v", res, err)
	}
	fp.Silent = false

	// Mark left: only the resignation date changes.
	pl, err = c.Preview(ctx, realtime.Write{Action: "left", Code: "002301", Values: map[string]string{realtime.FieldLeft: "2026-11-30"}})
	if err != nil || len(pl.Problems) > 0 || row(pl, realtime.FieldLeft).Value != "30/11/2026" || row(pl, realtime.FieldStatus).Name != "" {
		t.Fatalf("left preview: %v %+v", err, pl)
	}
	if res, err = c.MarkLeft(ctx, "002301", "2026-11-30", pl.Hash); err != nil || !res.OK {
		t.Fatalf("left: %+v %v", res, err)
	}
	if e := fp.Emps["002301"]; e[mc+"txtreginnationdate"] != "30/11/2026" || e[mc+"Txtempname"] != "Asha Verma" || e[mc+"cbodesignation"] != "1042" {
		t.Fatalf("after left: %v", e)
	}
	// Off the active list now: another update cannot find them.
	if _, err := c.Preview(ctx, realtime.Write{Action: "update", Code: "002301", Values: upd}); err == nil || !strings.Contains(err.Error(), "not on the portal's employee list") {
		t.Fatalf("left employee found: %v", err)
	}

	// The portal's own refusal comes back in its words; nothing is retried.
	n := fp.PostCount()
	fp.Refuse = "Card No. Already Exist"
	v3 := map[string]string{realtime.FieldCode: "002302", realtime.FieldCard: "00002302", realtime.FieldName: "Ravi", realtime.FieldDept: "Kitchen",
		realtime.FieldDesig: "GSA", realtime.FieldPolicy: "GEN", realtime.FieldShift: "GEN"}
	if res, err = c.Post(ctx, realtime.Write{Action: "create", Code: "002302", Values: v3}); err != nil || res.OK || res.Unclear || res.Message != "Card No. Already Exist" || fp.PostCount() != n+1 {
		t.Fatalf("refused: %+v %v", res, err)
	}

	// A value the portal's list does not have, the required department left on its blank choice, and a card the
	// portal cannot take block the send.
	bad := map[string]string{realtime.FieldCode: "2303", realtime.FieldCard: "EMP-2303", realtime.FieldName: "Ravi", realtime.FieldDesig: "Spa Therapist"}
	pl, _ = c.Preview(ctx, realtime.Write{Action: "create", Code: "2303", Values: bad})
	all := strings.Join(pl.Problems, " | ")
	if len(pl.Problems) != 3 || !strings.Contains(all, `no "Spa Therapist"`) || !strings.Contains(all, "Dept. Name") || !strings.Contains(all, "9 digits") {
		t.Fatalf("problems: %v", pl.Problems)
	}
	n = fp.PostCount()
	if _, err := c.Post(ctx, realtime.Write{Action: "create", Code: "2303", Values: bad}); err == nil || fp.PostCount() != n {
		t.Fatalf("blocked send posted: %v", err)
	}
	// An email the portal's form refuses is left out (not required there), and said so.
	v4 := map[string]string{realtime.FieldCode: "002304", realtime.FieldCard: "00002304", realtime.FieldName: "Meena", realtime.FieldDept: "Kitchen",
		realtime.FieldDesig: "GSA", realtime.FieldEmail: "meena@saltstayz.travel"}
	pl, _ = c.Preview(ctx, realtime.Write{Action: "create", Code: "002304", Values: v4})
	if len(pl.Problems) != 0 || row(pl, realtime.FieldEmail).Role != "" || len(pl.Unused) != 1 || !strings.Contains(pl.Unused[0], "Email") {
		t.Fatalf("email: %v %v", pl.Problems, pl.Unused)
	}
}

func rowByName(pl *realtime.Plan, name string) realtime.PlanRow {
	for _, r := range pl.Rows {
		if r.Name == name {
			return r
		}
	}
	return realtime.PlanRow{}
}

// Sending is on in the shipped Spec (owner, 25 Nov 2026). With it switched off, a preview works and nothing is
// posted.
func TestSendOff(t *testing.T) {
	fp := rtfake.New(t)
	if !realtime.Spec.Send || !realtime.Confirmed() || !realtime.FormKnown() {
		t.Fatal("the shipped Spec must have sending on and the form set")
	}
	old := realtime.Spec
	realtime.Spec.Send = false
	t.Cleanup(func() { realtime.Spec = old })
	if realtime.Confirmed() {
		t.Fatal("sending off but confirmed")
	}
	c, _ := realtime.New(fp.Config())
	ctx := context.Background()
	w := realtime.Write{Action: "create", Code: "002401", Values: map[string]string{realtime.FieldCode: "002401", realtime.FieldCard: "00002401", realtime.FieldName: "Off", realtime.FieldDept: "Kitchen"}}
	if _, err := c.Preview(ctx, w); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Post(ctx, w); !errors.Is(err, realtime.ErrSendOff) || fp.PostCount() != 0 {
		t.Fatalf("sent while off: %v %d", err, fp.PostCount())
	}
}

// The employee list's grid: rows with their RowId, code and card; the device password column is not read.
func TestListRows(t *testing.T) {
	fp := useFake(t)
	r := fp.Add("3078", "Ashwini Kumar Malik", rtfake.Emp{mc + "cbodeptname": "2"})
	c, _ := realtime.New(fp.Config())
	ctx := context.Background()
	pl, err := c.Preview(ctx, realtime.Write{Action: "update", Code: "00003078", Values: map[string]string{realtime.FieldName: "Ashwini Kumar Malik"}})
	if err != nil || pl.Page != "/Employee.aspx?RowId="+r || len(pl.Problems) > 0 {
		t.Fatalf("by code: %v %+v", err, pl)
	}
	rows := realtime.ListRows(`<table id='grid2'><thead><tr><th></th><th>Photo</th><th>EmpName</th><th>Cardno</th><th>EmpCode</th><th>Dept_Name</th><th>Desig_Name</th><th>pwd</th><th>Branch</th></tr></thead>
<tbody><tr><td></td><td></td><td><a href=Employee.aspx?RowId=167165> Ashwini</a></td><td><a href=Employee.aspx?RowId=167165> 00003078</a></td><td><a href=Employee.aspx?RowId=167165> 3078</a></td><td><a> Kitchen</a></td><td><a> GSA</a></td><td><a> 9999</a></td><td><a> Saltstayz Gulmohar Marg</a></td></tr></tbody></table>`)
	if len(rows) != 1 || rows[0] != (realtime.ListRow{RowID: "167165", Name: "Ashwini", Card: "00003078", Code: "3078", Dept: "Kitchen", Desig: "GSA", Branch: "Saltstayz Gulmohar Marg"}) {
		t.Fatalf("rows: %+v", rows)
	}
	if realtime.FindCode(rows, "003078") == nil || realtime.FindCard(rows, "3078") == nil || realtime.FindCode(rows, "3079") != nil {
		t.Fatal("find")
	}
}

// A create picks the punch machine the HRMS planned in the form's "Select Devices" list by its serial number; a
// machine the list does not have leaves the form's choice with a note (never a problem); an update or a mark left
// never touches the device; no device button is pressed (the fake fails the test if one is).
func TestDeviceOnCreate(t *testing.T) {
	fp := useFake(t)
	ctx := context.Background()
	c, _ := realtime.New(fp.Config())
	vals := map[string]string{realtime.FieldCode: "002501", realtime.FieldCard: "00002501", realtime.FieldName: "Dev One", realtime.FieldDept: "Kitchen",
		realtime.FieldDesig: "GSA", realtime.FieldDevice: "RSS20230761581"}
	pl, err := c.Preview(ctx, realtime.Write{Action: "create", Code: "002501", Values: vals})
	if d := row(pl, realtime.FieldDevice); err != nil || len(pl.Problems) > 0 || len(pl.Notes) > 0 || d.Value != "RSS20230761581" || d.Shown != "RSS20230761581 / Gulmohar Marg" {
		t.Fatalf("device: %v %+v", err, pl)
	}
	res, err := c.Post(ctx, realtime.Write{Action: "create", Code: "002501", Values: vals})
	if err != nil || !res.OK || fp.Emps["002501"][mc+"Device_List"] != "RSS20230761581" {
		t.Fatalf("create: %+v %v %v", res, err, fp.Emps["002501"])
	}
	// Not in the list: sent without a device (the form's own value), with a note.
	v2 := map[string]string{}
	for k, v := range vals {
		v2[k] = v
	}
	v2[realtime.FieldCode], v2[realtime.FieldCard], v2[realtime.FieldDevice] = "002502", "00002502", "RSS0000000"
	w := realtime.Write{Action: "create", Code: "002502", Values: v2, Notes: nil}
	pl, err = c.Preview(ctx, w)
	if err != nil || len(pl.Problems) > 0 || len(pl.Notes) != 1 || !strings.Contains(pl.Notes[0], "No punch machine chosen: machine RSS0000000") ||
		row(pl, realtime.FieldDevice).Name != "" || rowByName(pl, mc+"Device_List").Value == "RSS0000000" {
		t.Fatalf("unknown device: %v %+v", err, pl)
	}
	if res, err = c.Post(ctx, w); err != nil || !res.OK || len(res.Notes) != 1 || fp.Emps["002502"][mc+"Device_List"] == "RSS0000000" {
		t.Fatalf("create without device: %+v %v", res, err)
	}
	// A note given by the caller (no machine planned) is kept.
	pl, _ = c.Preview(ctx, realtime.Write{Action: "create", Code: "002503", Values: map[string]string{realtime.FieldCode: "002503", realtime.FieldCard: "00002503",
		realtime.FieldName: "Dev Three", realtime.FieldDept: "Kitchen"}, Notes: []string{realtime.DeviceNote("cluster X is full")}})
	if len(pl.Notes) != 1 || len(pl.Problems) > 0 || !strings.Contains(pl.Notes[0], "cluster X is full; HR adds them") {
		t.Fatalf("caller's note: %+v", pl)
	}
	// Update: the device is never set.
	pl, err = c.Preview(ctx, realtime.Write{Action: "update", Code: "002502", Values: map[string]string{realtime.FieldName: "Dev Two", realtime.FieldDevice: "RSS20230761581"}})
	if err != nil || len(pl.Problems) > 0 || len(pl.Notes) > 0 || row(pl, realtime.FieldDevice).Name != "" {
		t.Fatalf("update device: %v %+v", err, pl)
	}
}
