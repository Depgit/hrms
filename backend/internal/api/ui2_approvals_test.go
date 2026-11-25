package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// Round 2 approvals: subject details on requests, the employee's own route preview, settings for
// everyone, health titles, approver paths, waiting names, improvement plans and tax proofs in the
// inbox, and pay-item rows for deductions and full and final.

// apprRow finds a request in an inbox tab.
func apprRow(c *client, t *testing.T, emp, role, tab, wf, id string) map[string]any {
	t.Helper()
	s, r := c.do("GET", "/api/approvals?tab="+tab+"&wf="+wf, emp, role, nil)
	if s != 200 {
		t.Fatalf("queue %s: %v", tab, r)
	}
	for _, x := range r["list"].([]any) {
		if m := x.(map[string]any); m["id"] == id {
			return m
		}
	}
	return nil
}

// apprNextMonday is the first Monday at least n days ahead.
func apprNextMonday(n int) time.Time {
	d := time.Now().AddDate(0, 0, n)
	for d.Weekday() != time.Monday {
		d = d.AddDate(0, 0, 1)
	}
	return d
}

func TestUI2ApprSubjectAndPreview(t *testing.T) {
	c := setup(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009", "EMP-1020")
	from := apprNextMonday(20)
	day := func(i int) string { return from.AddDate(0, 0, i).Format("2006-01-02") }

	t.Run("an employee previews the route of their own leave, with the days counted", func(t *testing.T) {
		s, sim := c.do("POST", "/api/approvals/simulate", "EMP-1020", "Employee", map[string]any{"wf": "leave", "payload": map[string]any{"ltype": "Casual leave", "from": day(0), "to": day(6)}})
		if s != 200 {
			t.Fatal(sim)
		}
		days, offs, rng := num(sim["days"]), num(sim["offs"]), num(sim["range"])
		// A holiday in that week is an off the leave may still count (sandwich rule): days lies between 7−offs and 7.
		if rng != 7 || offs < 1 || days <= 0 || days > 7 || days < 7-offs {
			t.Fatalf("days %v offs %v range %v", days, offs, rng)
		}
		if len(sim["chain"].([]any)) == 0 || len(sim["people"].([]any)) == 0 {
			t.Fatalf("no route: %v", sim)
		}
		if !strings.Contains(fmt.Sprint(sim["problems"]), "limited to") {
			t.Fatalf("raise-time problems: %v", sim["problems"])
		}
		if _, ok := sim["team"].(float64); !ok || sim["location"] == "" || sim["department"] == "" {
			t.Fatalf("team cover: %v %v %v", sim["team"], sim["location"], sim["department"])
		}
		// Somebody else's route still needs the approvals view.
		if s, _ := c.do("POST", "/api/approvals/simulate", "EMP-1020", "Employee", map[string]any{"wf": "leave", "employeeId": "EMP-1014", "payload": map[string]any{"ltype": "Casual leave", "from": day(0), "to": day(0)}}); s != 403 {
			t.Fatalf("employee previewed another employee's route: %d", s)
		}
		if s, r := c.do("POST", "/api/approvals/simulate", "EMP-1004", "HRBP", map[string]any{"wf": "leave", "employeeId": "EMP-1020", "payload": map[string]any{"ltype": "Casual leave", "from": day(0), "to": day(0)}}); s != 200 || num(r["days"]) != 1 {
			t.Fatalf("HRBP preview: %d %v", s, r)
		}
	})

	t.Run("the turnaround setting is readable by everyone", func(t *testing.T) {
		if s, r := c.do("GET", "/api/approvals/settings", "EMP-1020", "Employee", nil); s != 200 || num(r["slaHours"]) <= 0 {
			t.Fatalf("%d %v", s, r)
		}
		if s, _ := c.do("PUT", "/api/approvals/settings", "EMP-1020", "Employee", map[string]int{"slaHours": 12}); s != 403 {
			t.Fatal("an employee changed the turnaround")
		}
	})

	var lv string
	t.Run("requests carry city, type, designation and the raiser's name", func(t *testing.T) {
		s, r := c.do("POST", "/api/approvals", "EMP-1009", "Line Manager", map[string]any{"wf": "leave", "employeeId": "EMP-1020", "payload": map[string]any{"ltype": "Casual leave", "from": day(1), "to": day(1)}})
		if s != 200 {
			t.Fatal(r)
		}
		lv = r["id"].(string)
		row := apprRow(c, t, "EMP-1002", "HR Admin", "open", "", lv)
		if row == nil {
			t.Fatal("leave not in All open")
		}
		for _, k := range []string{"city", "type", "designation", "department", "raised_by_name"} {
			if str, _ := row[k].(string); str == "" {
				t.Fatalf("list %s missing: %v", k, row)
			}
		}
		if row["raised_by_name"] != "Anil Kapoor" || row["type"] != "Permanent" {
			t.Fatalf("raiser %v type %v", row["raised_by_name"], row["type"])
		}
		_, d := c.do("GET", "/api/approvals/"+lv, "EMP-1002", "HR Admin", nil)
		if d["city"] != row["city"] || d["designation"] != row["designation"] || d["raised_by_name"] != "Anil Kapoor" || d["location"] == nil {
			t.Fatalf("detail: %v %v %v %v", d["city"], d["designation"], d["raised_by_name"], d["location"])
		}
		if _, ok := d["teamOff"].([]any); !ok {
			t.Fatalf("team cover: %v", d["teamOff"])
		}
	})

	t.Run("my requests name the person a request waits for", func(t *testing.T) {
		_, r := c.do("GET", "/api/me/requests?wf=leave", "EMP-1020", "Employee", nil)
		for _, x := range r["list"].([]any) {
			m := x.(map[string]any)
			if m["id"] == lv {
				if m["waiting_on"] == nil || m["waiting_name"] == nil || m["waiting_name"] == "" {
					t.Fatalf("waiting: %v %v", m["waiting_on"], m["waiting_name"])
				}
				return
			}
		}
		t.Fatal("leave not in my requests")
	})

	t.Run("health titles and approver paths follow the prototype", func(t *testing.T) {
		_, h := c.do("GET", "/api/approvals/health", "EMP-1002", "HR Admin", nil)
		titles := ""
		for _, x := range h["list"].([]any) {
			titles += x.(map[string]any)["title"].(string) + "|"
		}
		if !strings.Contains(titles, "Every named approver can sign in|") || !strings.Contains(titles, "Every approving role has at least one user|") {
			t.Fatal(titles)
		}
		_, d := c.do("GET", "/api/approvals/approvers", "EMP-1002", "HR Admin", nil)
		l := d["locations"].([]any)
		if len(l) == 0 || len(strings.Split(l[0].(map[string]any)["path"].(string), " › ")) != 4 {
			t.Fatalf("path: %v", l[0])
		}
	})
}

func TestUI2ApprPipAndTax(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009", "EMP-1011", "EMP-1014", "EMP-1020")

	_, r := c.do("POST", "/api/consequences", "EMP-1009", "Line Manager", map[string]any{"employeeId": "EMP-1014", "kind": "PIP", "days": 15, "reason": "Performance", "details": "Missed three handovers in October."})
	if r["id"] == nil {
		t.Fatal(r)
	}
	pip := fmt.Sprintf("PIP-%v", r["id"])

	t.Run("a PIP waits in the HRBP's inbox and is decided in consequence management", func(t *testing.T) {
		row := apprRow(c, t, "EMP-1004", "HRBP", "mine", "", pip)
		if row == nil || row["canAct"] != true || row["wf"] != "p_pip" || row["waitingOn"] != "HRBP" || row["sub"] != "PIP · 15 days" {
			t.Fatalf("inbox row: %v", row)
		}
		if apprRow(c, t, "EMP-1004", "HRBP", "open", "p_pip", pip) == nil || apprRow(c, t, "EMP-1004", "HRBP", "open", "leave", pip) != nil {
			t.Fatal("request type filter")
		}
		_, cnt := c.do("GET", "/api/approvals/counts?wf=p_pip", "EMP-1004", "HRBP", nil)
		if num(cnt["mine"]) != 1 || num(cnt["open"]) != 1 || num(cnt["info"]) != 0 {
			t.Fatalf("counts: %v", cnt)
		}
		s, d := c.do("GET", "/api/approvals/"+pip, "EMP-1004", "HRBP", nil)
		p, _ := d["payload"].(map[string]any)
		if s != 200 || p["reason"] != "Performance" || p["issuedBy"] != "Anil Kapoor" || len(d["people"].([]any)) != 1 || d["raised_by_name"] != "Anil Kapoor" {
			t.Fatalf("detail: %d %v", s, d)
		}
		// The person on the plan, and an employee without rights, never see it.
		if row := apprRow(c, t, "EMP-1014", "Employee", "open", "", pip); row != nil {
			t.Fatal("the employee sees the plan in Approvals")
		}
		if s, _ := c.do("GET", "/api/approvals/"+pip, "EMP-1020", "Employee", nil); s != 404 {
			t.Fatalf("another employee opened the plan: %d", s)
		}
		if s, r := c.do("POST", "/api/consequences/"+strings.TrimPrefix(pip, "PIP-")+"/approve", "EMP-1004", "HRBP", map[string]string{"note": "Agreed"}); s != 200 {
			t.Fatal(r)
		}
		done := apprRow(c, t, "EMP-1004", "HRBP", "done", "", pip)
		if done == nil || done["status"] != "Approved" || apprRow(c, t, "EMP-1004", "HRBP", "mine", "", pip) != nil {
			t.Fatalf("after approval: %v", done)
		}
		_, d = c.do("GET", "/api/approvals/"+pip, "EMP-1004", "HRBP", nil)
		h := fmt.Sprint(d["history"])
		if !strings.Contains(h, "Approved") || !strings.Contains(h, "PIP started.") {
			t.Fatal(h)
		}
	})

	t.Run("pending tax proofs wait for Payroll", func(t *testing.T) {
		// Proofs of the financial year of the open month.
		_, pay := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
		ym := pay["openYm"].(string)
		var yi int
		fmt.Sscan(ym[:4], &yi)
		if ym[5:7] < "04" {
			yi--
		}
		fy := fmt.Sprintf("%d-%02d", yi, (yi+1)%100)
		// One uploaded document backs two proofs.
		doc, err := st.DB.C("document").Insert(ctx, bson.M{"owner_kind": "employee", "owner_id": "EMP-1020", "employee_id": "EMP-1020", "doc_key": "tax-80C",
			"file_name": "lic.pdf", "content_type": "application/pdf", "size_bytes": 10, "storage_key": "none", "uploaded_by": "test"})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.DB.C("tax_proof").InsertMany(ctx, []any{
			bson.M{"employee_id": "EMP-1020", "fy": fy, "item": "80C", "doc": doc, "amount": 15000},
			bson.M{"employee_id": "EMP-1020", "fy": fy, "item": "80D", "doc": doc, "amount": 8000},
		}); err != nil {
			t.Fatal(err)
		}
		row := apprRow(c, t, "EMP-1011", "Payroll", "mine", "", "TAX-EMP-1020")
		if row == nil || row["wf"] != "p_tax" || row["sub"] != "2 proofs to review" || row["canAct"] != true {
			t.Fatalf("payroll inbox: %v", row)
		}
		if apprRow(c, t, "EMP-1011", "Payroll", "open", "p_tax", "TAX-EMP-1020") == nil {
			t.Fatal("filter p_tax")
		}
		_, d := c.do("GET", "/api/approvals/TAX-EMP-1020", "EMP-1011", "Payroll", nil)
		if d["title"] != "2 tax proofs" || d["people"] == nil || num(d["payload"].(map[string]any)["proofs"]) != 2 {
			t.Fatalf("detail: %v", d)
		}
		if apprRow(c, t, "EMP-1020", "Employee", "open", "", "TAX-EMP-1020") != nil {
			t.Fatal("the employee sees their own proofs as an approval")
		}
		if s, _ := c.do("GET", "/api/approvals/TAX-EMP-1020", "EMP-1009", "Line Manager", nil); s != 404 {
			t.Fatalf("a line manager opened tax proofs: %d", s)
		}
	})
}

func TestUI2ApprPayItemRows(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1001", "EMP-1002", "EMP-1004", "EMP-1011", "EMP-1020")
	_, pay := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open := pay["openYm"].(string)

	t.Run("a deduction shows its instalment schedule", func(t *testing.T) {
		s, r := c.do("POST", "/api/approvals", "EMP-1011", "Payroll", map[string]any{"wf": "p_ded", "employeeId": "EMP-1020",
			"payload": map[string]any{"amount": 3000, "instalments": 2, "startYm": open, "dedType": "Cash shortage", "heard": "Yes", "note": "Short by 3000 on the night shift"}})
		if s != 200 {
			t.Fatal(r)
		}
		id := r["id"].(string)
		_, d := c.do("GET", "/api/approvals/"+id, "EMP-1001", "CHRO", nil)
		sched, _ := d["schedule"].([]any)
		if len(sched) != 2 || sched[0].(map[string]any)["ym"] != open || num(sched[0].(map[string]any)["amount"])+num(sched[1].(map[string]any)["amount"]) != 3000 {
			t.Fatalf("pending schedule: %v", d["schedule"])
		}
		c.approveAll(t, id)
		_, d = c.do("GET", "/api/approvals/"+id, "EMP-1001", "CHRO", nil)
		if sched, _ = d["schedule"].([]any); len(sched) != 2 {
			t.Fatalf("approved schedule from the ledger: %v", d["schedule"])
		}
	})

	t.Run("a resignation names its separation case", func(t *testing.T) {
		s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "resign", "payload": map[string]any{"reason": "Relocation"}})
		if s != 200 {
			t.Fatal(r)
		}
		_, d := c.do("GET", "/api/approvals/"+r["id"].(string), "EMP-1002", "HR Admin", nil)
		if d["caseId"] != r["case"] || d["caseId"] == nil {
			t.Fatalf("case %v, want %v", d["caseId"], r["case"])
		}
	})

	t.Run("a full and final carries the last working day and its settlement lines", func(t *testing.T) {
		_, r := c.do("POST", "/api/separations", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1026", "type": "Contract end", "lwd": time.Now().AddDate(0, 0, 3).Format("2006-01-02")})
		sep, _ := r["id"].(string)
		if sep == "" {
			t.Fatal(r)
		}
		c.approveAll(t, r["requestId"].(string))
		_, _ = st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1026"}, bson.M{"$set": bson.M{"lwd": store.DateOf(time.Now().AddDate(0, 0, -1))}})
		_, d := c.do("GET", "/api/separations/"+sep, "EMP-1002", "HR Admin", nil)
		for _, x := range d["clearance"].([]any) {
			if m := x.(map[string]any); m["auto"] == nil {
				c.do("POST", fmt.Sprintf("/api/separations/%s/clearance/%v", sep, m["id"]), "EMP-1002", "HR Admin", nil)
			}
		}
		ratings := map[string]any{"Manager": 3, "Work and shifts": 3, "Pay and benefits": 3, "Growth": 3, "Workplace and team": 3}
		c.do("POST", "/api/separations/"+sep+"/interview", "EMP-1002", "HR Admin", map[string]any{"mainReason": "Other", "ratings": ratings, "recommend": "Yes"})
		_, f := c.do("POST", "/api/fnf/EMP-1026", "EMP-1011", "Payroll", nil)
		fid, _ := f["id"].(string)
		if fid == "" {
			t.Fatal(f)
		}
		lwd := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
		row := apprRow(c, t, "EMP-1002", "HR Admin", "open", "p_fnf", fid)
		if row == nil || row["payload"].(map[string]any)["lwd"] != lwd {
			t.Fatalf("list payload: %v", row)
		}
		_, d = c.do("GET", "/api/approvals/"+fid, "EMP-1001", "CHRO", nil)
		fnf, _ := d["fnf"].(map[string]any)
		if fnf == nil || fnf["lwd"] != lwd || fnf["doj"] == nil || fnf["payYm"] == nil {
			t.Fatalf("settlement: %v", d["fnf"])
		}
		if _, ok := fnf["lines"].([]any); !ok {
			t.Fatalf("lines: %v", fnf["lines"])
		}
		if d["payload"].(map[string]any)["lwd"] != lwd {
			t.Fatalf("payload lwd: %v", d["payload"])
		}
	})
}
