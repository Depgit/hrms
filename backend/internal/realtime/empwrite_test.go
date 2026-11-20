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

// Create, update and mark left against the made-up portal: the preview posts nothing, the send posts the previewed
// form once (with the page's current view state, only Save pressed), and the portal's own message comes back.
func TestEmployeeWrites(t *testing.T) {
	fp := useFake(t)
	ctx := context.Background()
	c, _ := realtime.New(fp.Config())
	vals := map[string]string{realtime.FieldCode: "2301", realtime.FieldCard: "00002301", realtime.FieldName: "Asha Verma", realtime.FieldFather: "Ram Verma",
		realtime.FieldDOB: "1995-04-21", realtime.FieldDOJ: "2026-11-16", realtime.FieldGender: "Female", realtime.FieldPhone: "9876500001",
		realtime.FieldEmail: "asha@example.com", realtime.FieldDept: "Front Office", realtime.FieldDesig: "GSA", realtime.FieldBranch: "SaltStayz Saket", realtime.FieldShift: "GEN"}
	pl, err := c.Preview(ctx, realtime.Write{Action: "create", Code: "2301", Values: vals})
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Problems) > 0 || fp.PostCount() != 0 || pl.Button != "Save" {
		t.Fatalf("preview: %v posts %d button %q", pl.Problems, fp.PostCount(), pl.Button)
	}
	for role, want := range map[string]string{realtime.FieldCode: "2301", realtime.FieldName: "Asha Verma", realtime.FieldDOB: "21/04/1995",
		realtime.FieldDOJ: "16/11/2026", realtime.FieldGender: "F", realtime.FieldDept: "1", realtime.FieldBranch: "22", realtime.FieldShift: "GEN",
		realtime.FieldFather: "Ram Verma", realtime.FieldCard: "00002301"} {
		if got := row(pl, role); got.Value != want {
			t.Fatalf("%s: %+v, want %s", role, got, want)
		}
	}
	for _, r := range pl.Rows { // the look-alikes stay as the form has them
		if strings.Contains(r.Name, "txtProxCard") || strings.Contains(r.Name, "txtShiftStart") {
			if r.Role != "" {
				t.Fatalf("look-alike taken: %+v", r)
			}
		}
	}
	// A send with another hash is refused before anything is posted.
	if _, err := c.CreateEmployee(ctx, "2301", vals, "nope"); !errors.Is(err, realtime.ErrChanged) || fp.PostCount() != 0 {
		t.Fatalf("hash: %v %d", err, fp.PostCount())
	}
	res, err := c.CreateEmployee(ctx, "2301", vals, pl.Hash)
	if err != nil || !res.OK || res.Message != "Record Saved Successfully" || fp.PostCount() != 1 {
		t.Fatalf("create: %+v %v %d", res, err, fp.PostCount())
	}
	if e := fp.Emps["2301"]; e == nil || e["ctl00$MainContent$txtEmpName"] != "Asha Verma" || e["ctl00$MainContent$ddlDepartment"] != "1" {
		t.Fatalf("on the portal: %v", e)
	}

	// The portal's own refusal comes back as it said it; nothing is retried.
	pl2, _ := c.Preview(ctx, realtime.Write{Action: "create", Code: "2301", Values: vals})
	res, err = c.CreateEmployee(ctx, "2301", vals, pl2.Hash)
	if err != nil || res.OK || res.Unclear || res.Message != "Employee Code already exists" || fp.PostCount() != 2 {
		t.Fatalf("duplicate: %+v %v %d", res, err, fp.PostCount())
	}

	// Update: the employee's form, the new designation.
	vals[realtime.FieldDesig] = "Commi I"
	pl, err = c.Preview(ctx, realtime.Write{Action: "update", Code: "2301", Values: vals})
	if err != nil || len(pl.Problems) > 0 || row(pl, realtime.FieldDesig).Value != "12" {
		t.Fatalf("update preview: %v %+v", err, pl)
	}
	if res, err = c.UpdateEmployee(ctx, "2301", vals, pl.Hash); err != nil || !res.OK {
		t.Fatalf("update: %+v %v", res, err)
	}
	if fp.Emps["2301"]["ctl00$MainContent$ddlDesignation"] != "12" {
		t.Fatal("designation not changed")
	}

	// Mark left: the leaving date and the Resigned status, nothing else changed.
	pl, err = c.Preview(ctx, realtime.Write{Action: "left", Code: "2301", Values: map[string]string{realtime.FieldLeft: "2026-11-30"}})
	if err != nil || len(pl.Problems) > 0 || row(pl, realtime.FieldLeft).Value != "30/11/2026" || row(pl, realtime.FieldStatus).Value != "R" {
		t.Fatalf("left preview: %v %+v", err, pl)
	}
	if res, err = c.MarkLeft(ctx, "2301", "2026-11-30", pl.Hash); err != nil || !res.OK {
		t.Fatalf("left: %+v %v", res, err)
	}
	if e := fp.Emps["2301"]; e["ctl00$MainContent$ddlStatus"] != "R" || e["ctl00$MainContent$txtEmpName"] != "Asha Verma" || e["ctl00$MainContent$ddlDesignation"] != "12" {
		t.Fatalf("after left: %v", e)
	}

	// A value the portal's list does not have, and a required control left empty, block the send.
	bad := map[string]string{realtime.FieldCode: "2302", realtime.FieldName: "Ravi", realtime.FieldDept: "Spa"}
	pl, _ = c.Preview(ctx, realtime.Write{Action: "create", Code: "2302", Values: bad})
	if len(pl.Problems) != 2 || !strings.Contains(pl.Problems[0]+pl.Problems[1], `no "Spa"`) || !strings.Contains(pl.Problems[0]+pl.Problems[1], "Date Of Join") {
		t.Fatalf("problems: %v", pl.Problems)
	}
	n := fp.PostCount()
	if _, err := c.CreateEmployee(ctx, "2302", bad, pl.Hash); err == nil || fp.PostCount() != n {
		t.Fatalf("blocked send posted: %v", err)
	}
}
