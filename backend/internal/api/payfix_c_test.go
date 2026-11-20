package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// Payroll test fixes, full and final and exits (EXT-06, EXT-42, EXT-46, EXT-48, EXT-55, EXT-57, EXT-59, EXT-20, ACC-09).

func pfcLines(pv map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	ls, _ := pv["lines"].([]any)
	for _, x := range ls {
		m := x.(map[string]any)
		out[fmt.Sprint(m["key"])] = m
	}
	return out
}

// pfcPermanent is an active Permanent employee who joined before 2025, other than those listed.
func pfcPermanent(t *testing.T, st *store.Store, not ...string) string {
	t.Helper()
	x, _ := st.DB.C("employee").FindOne(context.Background(), bson.M{"status": "Active", "type": "Permanent", "doj": bson.M{"$lt": "2025-01-01"},
		"_id": bson.M{"$nin": append(not, "EMP-1001", "EMP-1002", "EMP-1004", "EMP-1011", "EMP-1012")}}, options.FindOne().SetSort(bson.D{{Key: "_id", Value: -1}}))
	if x == nil {
		t.Fatal("no permanent employee in the sample")
	}
	return fmt.Sprint(x["id"])
}

func TestPayfixCFnfLines(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1011", "EMP-1012")
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open, _ := state["openYm"].(string)
	if open == "" {
		t.Fatalf("state %v", state)
	}
	id := pfcPermanent(t, st)
	prev := ymAdd(open, -1)

	// EXT-06: the leave plan's Privilege leave "Encashment on exit" pays the balance at the LWD.
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"lwd": prev + "-15", "exit_status": "On notice"}}); err != nil {
		t.Fatal(err)
	}
	_, pv := c.do("GET", "/api/fnf/"+id, "EMP-1011", "Payroll", nil)
	if enc := pfcLines(pv)["enc"]; enc == nil || apiFloat(enc["amount"]) <= 0 {
		t.Fatalf("EXT-06 leave encashment: %v", pv["lines"])
	}

	// EXT-57 and EXT-48: the open month was paid (published, with a full row) after the LWD month. Its recovery
	// instalment was in that row and is not brought forward again; the next month's still is. The 30 days paid
	// after the LWD are recovered.
	if _, err := st.DB.C("payroll_month").Insert(ctx, bson.M{"ym": open, "status": "Published"}); err != nil {
		t.Fatal(err)
	}
	row := map[string]any{"employeeId": id, "days": 30, "paid": 30, "gross": 30000, "earned": 30000, "net": 27000, "status": "Active", "doj": "2020-01-01"}
	if _, err := st.DB.C("payroll_row").Insert(ctx, bson.M{"ym": open, "employee_id": id, "net": 27000.0, "data": row}); err != nil {
		t.Fatal(err)
	}
	for i, ym := range []string{open, ymAdd(open, 1)} {
		if _, err := st.DB.C("ledger").Insert(ctx, bson.M{"employee_id": id, "ym": ym, "kind": "ded", "label": fmt.Sprintf("Advance recovery (%d of 2)", i+1),
			"amount": 1000.0, "source_id": "ADV-9" + id[4:], "reason": "test"}); err != nil {
			t.Fatal(err)
		}
	}
	want := 0.0
	later, _ := st.DB.C("ledger").Find(ctx, bson.M{"employee_id": id, "void": false, "kind": "ded", "ym": bson.M{"$gt": open}, "source_id": bson.M{"$regex": "^(ADV|DED|UNI|AST)-"}})
	for _, x := range later {
		want += apiFloat(x["amount"])
	}
	_, pv = c.do("GET", "/api/fnf/"+id, "EMP-1011", "Payroll", nil)
	L := pfcLines(pv)
	if rec := L["rec"]; rec == nil || apiFloat(rec["amount"]) != want {
		t.Fatalf("EXT-57 brought forward %v, want %v (the paid month's line left out): %v", rec, want, pv["lines"])
	}
	if ex := L["excess"]; ex == nil || apiFloat(ex["amount"]) != 30000 || !strings.Contains(fmt.Sprint(ex["label"]), "Salary paid after the last working day") {
		t.Fatalf("EXT-48 excess salary: %v", pv["lines"])
	}
}

func TestPayfixCFnfOwed(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1011", "EMP-1012")
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open, _ := state["openYm"].(string)
	id := pfcPermanent(t, st)
	_, _ = st.DB.C("employee").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"lwd": ymAdd(open, -1) + "-15", "exit_status": "On notice"}})
	_, _ = st.DB.C("asset").UpdateMany(ctx, bson.M{"employee_id": id, "status": "Issued"}, bson.M{"$set": bson.M{"status": "Available", "employee_id": nil}})
	if _, err := st.DB.C("ledger").Insert(ctx, bson.M{"employee_id": id, "ym": ymAdd(open, 2), "kind": "ded", "label": "Advance recovery (1 of 1)",
		"amount": 9000000.0, "source_id": "ADV-8" + id[4:], "reason": "test"}); err != nil {
		t.Fatal(err)
	}
	s, f := c.do("POST", "/api/fnf/"+id, "EMP-1011", "Payroll", nil)
	fid, _ := f["id"].(string)
	if s != 200 || fid == "" || apiFloat(f["net"]) >= 0 {
		t.Fatalf("prepare: %d %v", s, f)
	}
	c.approveAll(t, fid)
	// EXT-42: nothing goes through payroll; the amount is owed by the employee.
	if n := apiCount(st, "ledger", bson.M{"source_id": fid}); n != 0 {
		t.Fatalf("a settlement below zero writes no pay lines: %d", n)
	}
	if n := apiCount(st, "ledger", bson.M{"employee_id": id, "source_id": "ADV-8" + id[4:], "void": false}); n != 0 {
		t.Fatal("the brought-forward recovery is closed into the settlement")
	}
	d := apiDoc(st, "fnf", bson.M{"_id": fid})
	if fmt.Sprint(d["receivable"].(map[string]any)["status"]) != "Open" {
		t.Fatalf("receivable: %v", d["receivable"])
	}
	if s, r := c.do("GET", "/api/fnf/"+id+"/no-dues", "EMP-1011", "Payroll", nil); s != 400 || !strings.Contains(fmt.Sprint(r), "owed") {
		t.Fatalf("no dues while owed: %d %v", s, r)
	}
	if _, lt := c.do("GET", "/api/fnf/"+id+"/letter", "EMP-1011", "Payroll", nil); !strings.Contains(fmt.Sprint(lt["html"]), "Recoverable from you") {
		t.Fatalf("letter: %v", lt["html"])
	}
	_, l := c.do("GET", "/api/fnf", "EMP-1011", "Payroll", nil)
	if !strings.Contains(fmt.Sprint(l["rows"]), "Open") {
		t.Fatal("the list shows the amount owed")
	}
	if s, r := c.do("POST", "/api/fnf/"+id+"/receivable", "EMP-1011", "Payroll", map[string]any{"action": "waived", "note": "Hardship"}); s != 403 {
		t.Fatalf("the preparer does not waive: %d %v", s, r)
	}
	if s, _ := c.do("POST", "/api/fnf/"+id+"/receivable", "EMP-1011", "Payroll", map[string]any{"action": "recovered"}); s != 400 {
		t.Fatal("a note is needed")
	}
	if s, r := c.do("POST", "/api/fnf/"+id+"/receivable", "EMP-1011", "Payroll", map[string]any{"action": "recovered", "note": "NEFT UTR 1234"}); s != 200 || r["status"] != "Recovered" {
		t.Fatal(r)
	}
	if apiDoc(st, "audit_log", bson.M{"action": "Full and final: amount owed recovered"}) == nil {
		t.Fatal("audited")
	}
	if s, r := c.do("GET", "/api/fnf/"+id+"/no-dues", "EMP-1011", "Payroll", nil); s != 200 {
		t.Fatalf("no dues once recovered: %v", r)
	}
}

func TestPayfixCSepAccess(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	id := pfcPermanent(t, st)
	e := apiDoc(st, "employee", bson.M{"_id": id})
	mgr := fmt.Sprint(e["manager_id"])
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1011", mgr, id)
	_, r := c.do("POST", "/api/separations", "EMP-1002", "HR Admin", map[string]any{"employeeId": id, "type": "Contract end", "lwd": time.Now().AddDate(0, 0, 3).Format("2006-01-02")})
	sep, _ := r["id"].(string)
	if sep == "" {
		t.Fatal(r)
	}
	lines := []map[string]any{{"key": "grat", "kind": "earn", "label": "Gratuity, 5 years", "amount": 72115.0, "note": "Basic 25,000 × 15 ÷ 26 × 5"}}
	if _, err := st.DB.C("fnf").Insert(ctx, bson.M{"_id": "FNF-T1", "sep_id": sep, "employee_id": id, "lwd": "2026-11-20", "pay_ym": "2026-12", "lines": lines,
		"net": 72115.0, "status": "Approved", "by_name": "test"}); err != nil {
		t.Fatal(err)
	}
	amt := func(emp, role string) (int, any, any) {
		s, d := c.do("GET", "/api/separations/"+sep, emp, role, nil)
		f, _ := d["fnf"].(map[string]any)
		if f == nil {
			return s, nil, nil
		}
		ls, _ := f["lines"].([]any)
		var a any
		if len(ls) > 0 {
			a = ls[0].(map[string]any)["amount"]
		}
		return s, f["net"], a
	}
	if s, net, a := amt("EMP-1002", "HR Admin"); s != 200 || apiFloat(net) != 72115 || apiFloat(a) != 72115 {
		t.Fatalf("HR Admin sees salary in full: %d %v %v", s, net, a)
	}
	if s, net, a := amt(mgr, "Line Manager"); s == 200 && (net != nil || a != nil) {
		t.Fatalf("ACC-09 a line manager sees no amounts: %v %v", net, a)
	}
	if s, net, a := amt("EMP-1004", "HRBP"); s == 200 {
		if _, isNum := net.(float64); isNum {
			t.Fatalf("ACC-09 HRBP sees masked amounts: %v %v", net, a)
		}
		if _, isNum := a.(float64); isNum {
			t.Fatalf("ACC-09 HRBP line: %v", a)
		}
	}
	if s, net, _ := amt(id, "Employee"); s == 200 && apiFloat(net) != 72115 {
		t.Fatalf("the employee sees their own: %v", net)
	}
	if s, _ := c.do("GET", "/api/fnf/FNF-T1/letter", "EMP-1004", "HRBP", nil); s != 403 {
		t.Fatal("ACC-09 the settlement letter is for Payroll")
	}
}

func TestPayfixCTermination(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004")
	_, r := c.do("POST", "/api/separations", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1026", "type": "Termination", "reason": "Performance", "noticeOption": "serve", "note": "Did not meet the improvement plan"})
	sep, req := fmt.Sprint(r["id"]), fmt.Sprint(r["requestId"])
	sd := apiDoc(st, "separation", bson.M{"_id": sep})
	days := int(apiFloat(sd["notice_days"]))
	if days == 0 {
		if n, ok := sd["notice_days"].(int32); ok {
			days = int(n)
		}
	}
	// The case waited 10 days for approval: the notice still runs from the approval (EXT-59).
	early := time.Now().AddDate(0, 0, days-10).Format("2006-01-02")
	_, _ = st.DB.C("separation").Update(ctx, bson.M{"_id": sep}, bson.M{"$set": bson.M{"lwd": early}})
	_, _ = st.DB.C("approval_request").Update(ctx, bson.M{"_id": req}, bson.M{"$set": bson.M{"payload.lwd": early}})
	c.approveAll(t, req)
	want := time.Now().AddDate(0, 0, days).Format("2006-01-02")
	if e := apiDoc(st, "employee", bson.M{"_id": "EMP-1026"}); days > 0 && fmt.Sprint(e["lwd"]) != want {
		t.Fatalf("EXT-59 LWD %v, want approval + %d days = %s", e["lwd"], days, want)
	}
	// EXT-55: the letter carries the notice sentence.
	if s, r := c.do("POST", "/api/separations/"+sep+"/letter", "EMP-1004", "HRBP", map[string]any{"kind": "Termination letter"}); s != 200 {
		t.Fatal(r)
	}
	l := apiDoc(st, "sep_letter", bson.M{"sep_id": sep, "kind": "Termination letter"})
	if !strings.Contains(fmt.Sprint(l["body"]), "You are required to serve the notice period.") {
		t.Fatalf("EXT-55 letter: %v", l["body"])
	}
}

func TestPayfixCAbscondLWD(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1004")
	// Last at work 10 days ago (a punch), nothing since: the case's LWD is that day (EXT-20).
	day := time.Now().AddDate(0, 0, -10)
	for day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
		day = day.AddDate(0, 0, -1)
	}
	d := day.Format("2006-01-02")
	from := time.Now().AddDate(0, 0, -31).Format("2006-01-02")
	_, _ = st.DB.C("attendance_day").DeleteMany(ctx, bson.M{"employee_id": "EMP-1022", "date": bson.M{"$gte": from}})
	_, _ = st.DB.C("approval_request").DeleteMany(ctx, bson.M{"employee_id": "EMP-1022", "wf": bson.M{"$in": bson.A{"leave", "regularize"}}})
	if _, err := st.DB.C("attendance_day").Insert(ctx, bson.M{"employee_id": "EMP-1022", "date": d, "in_time": "09:00", "out_time": "18:00", "source": "MIS upload", "by_name": "test"}); err != nil {
		t.Fatal(err)
	}
	_, a := c.do("POST", "/api/separations", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1022", "type": "Absconding", "note": "Not reachable on phone since last week"})
	sep, _ := a["id"].(string)
	if sep == "" {
		t.Fatal(a)
	}
	if x := apiDoc(st, "separation", bson.M{"_id": sep}); fmt.Sprint(x["lwd"]) != d {
		t.Fatalf("EXT-20 LWD %v, want the last day at work %s (absent days %v)", x["lwd"], d, x["absent_days"])
	}
}

func TestPayfixCRehire(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	hub, _ := hubPlace(st)
	var old string
	if x, _ := st.DB.C("employee").FindOne(ctx, bson.D{{Key: "location_id", Value: hub}, {Key: "manager_id", Value: "EMP-1009"}},
		options.FindOne().SetSort(bson.D{{Key: "_id", Value: 1}})); x != nil {
		old = fmt.Sprint(x["id"])
	}
	day, _ := time.Parse("2006-01-02", store.Today())
	if _, err := st.DB.C("employee").Update(ctx, bson.D{{Key: "_id", Value: old}}, bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "Inactive"},
		{Key: "lwd", Value: day.AddDate(0, 0, -10).Format("2006-01-02")}, {Key: "exit_reason", Value: "Resigned"}, {Key: "rehire", Value: "Not eligible"}}}}); err != nil {
		t.Fatal(err)
	}
	_, pre := c.do("GET", "/api/candidates/rejoin/"+old, "EMP-1004", "HRBP", nil)
	data, _ := pre["data"].(map[string]any)
	if data == nil {
		t.Fatalf("prefill %v", pre)
	}
	data["codeMode"], data["ctc"], data["dob"], data["gender"] = "same", 250000, "1995-01-01", "Male"
	if data["phone"] == nil {
		data["phone"] = "9876504001"
	}
	if data["email"] == nil {
		data["email"] = "rejoin@example.com"
	}
	body := map[string]any{"data": data, "experience": pre["experience"]}
	_, chk := c.do("POST", "/api/candidates/check", "EMP-1004", "HRBP", body)
	if !strings.Contains(fmt.Sprint(chk), "not eligible for rehire") {
		t.Fatalf("EXT-46 check: %v", chk)
	}
	if s, _ := c.do("POST", "/api/candidates", "EMP-1004", "HRBP", body); s != 400 {
		t.Fatal("EXT-46 a leaver not eligible for rehire is not shortlisted")
	}
}
