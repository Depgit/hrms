package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/api"
	"github.com/saltstayz/hrms/internal/store"
)

// Employee import: codes are handed out and checked in memory, and the rows written in batches, with the same
// results as row-by-row INSERTs — each row's checks see the rows written before it — and a full 5,000-row import fits
// well inside one transaction.
func TestEmployeeImportBatch(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	c.signIn("EMP-1002")
	e := apiDoc(st, "employee", bson.M{"_id": "EMP-1020"})
	name := func(coll string, id any) string { return fmt.Sprint(apiDoc(st, coll, bson.M{"_id": id})["name"]) }
	loc, dept, desig := name("location", e["location_id"]), name("department", e["department_id"]), name("designation", e["designation_id"])
	pan := func(i int) string {
		b := []byte("AAAAA")
		for k, n := 4, i; k >= 0; k, n = k-1, n/26 {
			b[k] = byte('A' + n%26)
		}
		return fmt.Sprintf("%s%04dZ", b, i%10000)
	}
	row := func(i int, extra map[string]any) map[string]any {
		r := map[string]any{"name": fmt.Sprintf("Batch Person %d", i), "type": "Permanent", "doj": "2026-01-05", "location": loc, "department": dept,
			"designation": desig, "grade": e["grade"], "shift": e["shift_code"], "gross": 17000, "mobile": fmt.Sprintf("98765%05d", i%100000),
			"pan": pan(i), "accountNo": fmt.Sprintf("1234567%05d", i%100000), "ifsc": "HDFC0001234", "bankName": "HDFC"}
		for k, v := range extra {
			r[k] = v
		}
		return r
	}
	imp := func(apply bool, rows ...map[string]any) (int, map[string]any) {
		return c.do("POST", "/api/employees/import", "EMP-1002", "HR Admin", map[string]any{"rows": rows, "apply": apply})
	}
	results := func(r map[string]any) []map[string]any {
		var out []map[string]any
		l, _ := r["results"].([]any)
		if l == nil { // a 400 carries them in details
			l, _ = r["details"].([]any)
		}
		for _, x := range l {
			out = append(out, x.(map[string]any))
		}
		return out
	}
	errsOf := func(x map[string]any) string { return fmt.Sprint(x["errors"]) }

	series := apiDoc(st, "employee_code_series", bson.M{"key": "All"})
	prefix, pad, next := fmt.Sprint(series["prefix"]), int(apiFloat(series["pad"])), int(apiFloat(series["next_no"]))
	code := func(n int) string { return fmt.Sprintf("%s%0*d", prefix, pad, n) }
	before := apiCount(st, "employee", bson.M{})

	// Row 0 claims the code after next in lower case; rows 1-3 take auto codes around it; row 3 reports to row 0;
	// row 4 names a manager that only a later row creates, which the row-by-row checks did not see yet.
	claimed := strings.ToLower(code(next + 1))
	rows := []map[string]any{
		row(1, map[string]any{"code": claimed}),
		row(2, nil),
		row(3, nil),
		row(4, map[string]any{"managerId": claimed}),
		row(5, map[string]any{"code": "BATCH-A", "managerId": "BATCH-B"}),
		row(6, map[string]any{"code": "BATCH-B"}),
	}
	s, r := imp(true, rows...)
	if s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "1 row(s) have errors") {
		t.Fatalf("apply with a bad row: %d %v", s, r)
	}
	if res := results(r); len(res) != 6 || !strings.Contains(errsOf(res[4]), "Reporting manager BATCH-B not found") {
		t.Fatalf("row errors: %v", res)
	}
	if n := apiCount(st, "employee", bson.M{}); n != before {
		t.Fatalf("nothing is imported when a row fails: %d → %d", before, n)
	}
	if got := int(apiFloat(apiDoc(st, "employee_code_series", bson.M{"key": "All"})["next_no"])); got != next {
		t.Fatalf("the series is untouched when the import fails: %d, want %d", got, next)
	}

	rows[4] = row(5, map[string]any{"code": "BATCH-A"})
	s, r = imp(true, rows...)
	if s != 200 {
		t.Fatalf("apply: %d %v", s, r)
	}
	var codes []string
	for _, x := range results(r) {
		codes = append(codes, fmt.Sprint(x["code"]))
	}
	if want := []string{claimed, code(next), code(next + 2), code(next + 3), "BATCH-A", "BATCH-B"}; fmt.Sprint(codes) != fmt.Sprint(want) {
		t.Fatalf("codes %v, want %v", codes, want)
	}
	if got := int(apiFloat(apiDoc(st, "employee_code_series", bson.M{"key": "All"})["next_no"])); got != next+4 {
		t.Fatalf("next_no %d, want %d", got, next+4)
	}
	if m := apiDoc(st, "employee", bson.M{"_id": code(next + 3)}); m["manager_id"] != claimed || m["status"] != "Active" || m["verified"] != true {
		t.Fatalf("imported employee: %v", m)
	}
	for _, coll := range []string{"employee_personal", "employee_gov_id", "gross_history", "employee_bank"} {
		if n := apiCount(st, coll, bson.M{"employee_id": bson.M{"$in": codes}}); n != 6 {
			t.Fatalf("%s: %d rows for the 6 imported employees", coll, n)
		}
	}
	if dflt, _ := st.DB.C("salary_structure").FindOne(ctx, bson.M{"is_default": true}); dflt != nil {
		if n := apiCount(st, "structure_map", bson.M{"employee_id": bson.M{"$in": codes}}); n != 6 {
			t.Fatalf("structure_map: %d", n)
		}
	}

	// Codes and PANs already on file, in another case; and, within one batch, a code an earlier row was just given.
	s, r = imp(false, row(7, map[string]any{"code": strings.ToLower(code(next))}), row(8, map[string]any{"pan": pan(2)}))
	if res := results(r); s != 200 || !strings.Contains(errsOf(res[0]), "already exists") || !strings.Contains(errsOf(res[1]), "already used") {
		t.Fatalf("on-file checks: %d %v", s, r)
	}
	auto, explicit := row(9, nil), row(10, map[string]any{"code": strings.ToLower(code(next + 4))})
	if s, r = imp(false, auto, explicit); s != 200 || r["failed"] != float64(0) {
		t.Fatalf("preview writes nothing, so the explicit code is still free: %d %v", s, r)
	}
	s, r = imp(true, auto, explicit)
	if res := results(r); s != 400 || len(res) != 2 || !strings.Contains(errsOf(res[1]), "Code "+strings.ToLower(code(next+4))+" already exists") {
		t.Fatalf("the code given to an earlier row of the same import is taken: %d %v", s, r)
	}

	// A full import of 5,000 rows, as many as one request allows.
	before = apiCount(st, "employee", bson.M{})
	big := make([]map[string]any, 5000)
	for i := range big {
		big[i] = row(1000+i, nil)
	}
	start := time.Now()
	s, r = imp(true, big...)
	took := time.Since(start)
	if s != 200 || r["ok"] != float64(5000) {
		t.Fatalf("5,000 rows: %d %v", s, r["error"])
	}
	if n := apiCount(st, "employee", bson.M{}); n != before+5000 {
		t.Fatalf("employees %d, want %d", n, before+5000)
	}
	if took > 30*time.Second {
		t.Fatalf("5,000 rows took %v: too close to the 60s transaction limit", took)
	}
	t.Logf("5,000 rows imported in %v", took)
}

// A store error inside the transaction (here: one already aborted by a failed write) is passed on as it is, not
// reported as a missing code series.
func TestNextEmployeeCodeStoreError(t *testing.T) {
	_, _, st := setupFull(t)
	ctx := context.Background()
	var codeErr error
	_ = st.Tx(ctx, func(tx store.Q) error {
		codeErr = nil
		dup := apiDoc(st, "employee", bson.M{"_id": "EMP-1020"})
		delete(dup, "id")
		dup["_id"] = "EMP-1020"
		_, _ = tx.C("employee").Insert(ctx, dup) // duplicate key: aborts the transaction
		_, codeErr = api.NextEmployeeCode(ctx, tx, "Permanent")
		return codeErr
	})
	if codeErr == nil || strings.Contains(codeErr.Error(), "No employee code series") {
		t.Fatalf("want the store's error, got %v", codeErr)
	}
	if code, _ := store.ErrCode(codeErr); code != "23505" {
		t.Fatalf("want the aborting error (23505), got %q: %v", code, codeErr)
	}
}

// Employee codes by type from one series (owner, 25 Nov 2026): with one series for everyone, a Permanent gets
// EMP-<next> and a Retainer RET-<next> from the SAME numbers and width, so their digits never repeat (the portal code
// is the ID's number); a number one prefix already has is skipped for the other; Contractual keeps the EMP- prefix.
func TestEmployeeCodesRetEmpOneSeries(t *testing.T) {
	_, _, st := setupFull(t)
	ctx := context.Background()
	if _, err := st.DB.C("employee_code_settings").Update(ctx, bson.M{"_id": 1}, bson.M{"$set": bson.M{"by_type": false}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.C("employee_code_series").Update(ctx, bson.M{"key": "All"}, bson.M{"$set": bson.M{"prefix": "EMP-", "pad": 6, "next_no": 7}}); err != nil {
		t.Fatal(err)
	}
	dup := apiDoc(st, "employee", bson.M{"_id": "EMP-1020"}) // RET-000009 already exists: 9 is used for EMP- too
	delete(dup, "id")
	dup["_id"] = "RET-000009"
	if _, err := st.DB.C("employee").Insert(ctx, dup); err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := st.Tx(ctx, func(tx store.Q) error {
		for _, typ := range []string{"Permanent", "Retainer", "Permanent", "Contractual"} {
			c, err := api.NextEmployeeCode(ctx, tx, typ)
			if err != nil {
				return err
			}
			got = append(got, c)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := "EMP-000007 RET-000008 EMP-000010 EMP-000011"; strings.Join(got, " ") != want {
		t.Fatalf("codes %v, want %s", got, want)
	}
	if s, ok := apiDoc(st, "employee_code_series", bson.M{"key": "All"})["next_no"]; !ok || fmt.Sprint(s) != "12" {
		t.Fatalf("next_no %v", s)
	}
}
