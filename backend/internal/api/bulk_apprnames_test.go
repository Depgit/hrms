package api_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Bulk update › Approver names (owner, 26 Nov 2026): the template lists the missing names with an Employee drop-down;
// an upload names the person as Map a role would; the right and an inactive person are refused.
func TestBulkApproverNames(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1020")
	const hr, hrRole = "EMP-1002", "HR Admin"
	ctx := context.Background()

	// An employee may neither download nor upload.
	if s, _ := c.raw("/api/bulk/apprnames/download", "EMP-1020", "Employee"); s != 403 {
		t.Fatalf("employee download: %d", s)
	}
	if s, _ := c.sendFile("/api/bulk/apprnames/check", "EMP-1020", "Employee", "a.csv", []byte("Role\n")); s != 403 {
		t.Fatalf("employee upload: %d", s)
	}

	// The template: the missing names, an Employee drop-down from the hidden list.
	s, raw := c.raw("/api/bulk/apprnames/download", hr, hrRole)
	if s != 200 {
		t.Fatalf("download: %d %s", s, raw)
	}
	f, err := excelize.OpenReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := f.GetRows("Data")
	if len(rows) < 2 || strings.Join(rows[0], ",") != "Role,Level,Place,Place Code,Where,Needed By,Employee" {
		t.Fatalf("template: %v", rows)
	}
	var slot []string
	for _, r := range rows[1:] {
		if len(r) >= 4 && r[1] == "Property" {
			slot = r
			break
		}
	}
	if slot == nil {
		t.Fatalf("no property slot missing: %v", rows)
	}
	dvs, _ := f.GetDataValidations("Data")
	if len(dvs) != 1 || dvs[0].Sqref != "G2:G5000" || !strings.Contains(dvs[0].Formula1, "Lists!") {
		t.Fatalf("employee drop-down: %+v", dvs)
	}
	if v, _ := f.GetSheetVisible("Lists"); v {
		t.Fatal("the lists sheet is hidden")
	}
	list, _ := f.GetCols("Lists")
	pick := ""
	for _, v := range list[0] {
		if strings.HasSuffix(v, "(EMP-1020)") {
			pick = v
		}
	}
	if pick == "" {
		t.Fatalf("EMP-1020 not in the drop-down: %v", list[0])
	}
	role, placeID, place := slot[0], slot[3], slot[2]
	gap := role + " for " + place
	gaps := func() string {
		_, d := c.do("GET", "/api/approvals/approvers", hr, hrRole, nil)
		return strings.Join(apprStrs(d["gaps"]), "|")
	}
	if !strings.Contains(gaps(), gap) {
		t.Fatalf("%s is not a gap: %s", gap, gaps())
	}
	run := func(action, csv string) map[int]map[string]any {
		t.Helper()
		s, r := c.sendFile("/api/bulk/apprnames/"+action, hr, hrRole, "apprnames.csv", []byte(csv))
		if s != 200 {
			t.Fatalf("%s: %d %v", action, s, r)
		}
		return bulkRows(t, r)
	}
	head := "Role,Level,Place,Place Code,Employee\n"
	line := role + ",Property,\"" + place + "\"," + placeID + ","

	// An inactive person and an unknown one are refused; a blank Employee is skipped.
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1021"}, bson.M{"$set": bson.M{"status": "Inactive"}}); err != nil {
		t.Fatal(err)
	}
	rr := run("check", head+line+"Someone (EMP-1021)\n"+role+",Property,x,nowhere,Nobody (EMP-9999)\n")
	if !strings.Contains(rowErr(rr[2]), "is not active") || !strings.Contains(rowErr(rr[3]), "not found") {
		t.Fatalf("refusals: %v | %v", rr[2], rr[3])
	}
	if rr = run("check", head+line+"\n"); rowErr(rr[2]) != "[]" || len(rr[2]["changes"].([]any)) != 0 {
		t.Fatalf("blank Employee: %v", rr[2])
	}

	// The pick is saved as Role mapping saves it: the slot is filled, the red box shrinks, audited.
	before := apiCount(st, "user_role_map", bson.M{"employee_id": "EMP-1020", "role_name": role})
	rr = run("apply", head+line+pick+"\n")
	if rowErr(rr[2]) != "[]" || len(rr[2]["changes"].([]any)) != 1 {
		t.Fatalf("apply: %v", rr[2])
	}
	m := apiDoc(st, "user_role_map", bson.M{"employee_id": "EMP-1020", "role_name": role})
	if m == nil || m["active"] != true || (before == 0 && (m["access"] != "locs" || strings.Join(apprStrs(m["location_ids"]), ",") != placeID)) {
		t.Fatalf("mapping: %v", m)
	}
	if strings.Contains(gaps(), gap) {
		t.Fatalf("%s still missing: %s", gap, gaps())
	}
	if n := apiCount(st, "audit_log", bson.M{"entity": "user_role_map", "action": bson.M{"$regex": "approver names"}}); n != 1 {
		t.Fatalf("audit: %d", n)
	}
	// The same file again: no change.
	rr = run("check", head+line+pick+"\n")
	if rowErr(rr[2]) != "[]" || len(rr[2]["changes"].([]any)) != 0 {
		t.Fatalf("again: %v", rr[2])
	}
}

func apprStrs(v any) []string {
	l, _ := v.([]any)
	return toStrs(l)
}
