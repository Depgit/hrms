package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Round 3 of the Approvals, Shift and roster and Bulk update pages: the improvement plan's flow,
// the Employee view of "Check a flow", health items, overlaps, the info tab, changing or withdrawing
// an accepted resignation, HR's reasons, the roster change behind a regularization, the prototype's
// result sentences, the discipline report by role, the PIP approver's name, and the Work Policy
// column of the employee master update.

// ui3ApproveAll approves a request stage by stage as HR Admin (override, with a comment).
func ui3ApproveAll(t *testing.T, c *client, id string) map[string]any {
	t.Helper()
	var a map[string]any
	for i := 0; i < 10; i++ {
		var s int
		s, a = c.do("POST", "/api/approvals/"+id+"/approve", "EMP-1002", "HR Admin", map[string]string{"note": "Agreed"})
		if s != 200 {
			t.Fatalf("approve %s: %v", id, a)
		}
		if a["status"] == "Approved" {
			return a
		}
	}
	t.Fatalf("%s still pending: %v", id, a)
	return nil
}

// ui3Applied is the result line an approved request wrote to its history.
func ui3Applied(t *testing.T, c *client, id string) string {
	t.Helper()
	_, d := c.do("GET", "/api/approvals/"+id, "EMP-1002", "HR Admin", nil)
	for _, h := range d["history"].([]any) {
		if m := h.(map[string]any); m["action"] == "Applied" {
			return fmt.Sprint(m["note"])
		}
	}
	return ""
}

func TestUI3ApprPipFlow(t *testing.T) {
	c := setup(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009", "EMP-1020")

	t.Run("the improvement plan has an approval flow", func(t *testing.T) {
		_, mx := c.do("GET", "/api/approvals/flows", "EMP-1002", "HR Admin", nil)
		flows := map[string]map[string]any{}
		for _, x := range mx["list"].([]any) {
			if m := x.(map[string]any); m["wf"] == "p_pip" {
				flows[m["flow"].(string)] = m
			}
		}
		approvers := func(m map[string]any) string {
			rules := m["rules"].([]any)
			return fmt.Sprint(rules[len(rules)-1].(map[string]any)["approvers"])
		}
		if len(flows) != 2 || approvers(flows["Operations"]) != "[HRBP]" || approvers(flows["Corporate"]) != "[HRBP Corporate]" {
			t.Fatalf("p_pip flows: %v", flows)
		}
		m := flows["Operations"]
		if s, r := c.do("PUT", "/api/approvals/flows/p_pip/Operations", "EMP-1002", "HR Admin", map[string]any{"rules": m["rules"], "noResponse": "escalate"}); s != 200 {
			t.Fatalf("saving the p_pip flow: %v", r)
		}
	})

	var hrbp string
	t.Run("simulate accepts p_pip and names the approver", func(t *testing.T) {
		s, sim := c.do("POST", "/api/approvals/simulate", "EMP-1004", "HRBP", map[string]any{"wf": "p_pip", "employeeId": "EMP-1020", "payload": map[string]any{}})
		if s != 200 || fmt.Sprint(sim["chain"]) != "[HRBP]" {
			t.Fatalf("%d %v", s, sim)
		}
		hrbp = sim["people"].([]any)[0].(map[string]any)["name"].(string)
		if hrbp == "" || hrbp == "Unassigned" {
			t.Fatal(sim["people"])
		}
		// A PIP is still raised from consequence management, never as an approval request.
		if s, _ := c.do("POST", "/api/approvals", "EMP-1004", "HRBP", map[string]any{"wf": "p_pip", "employeeId": "EMP-1020", "payload": map[string]any{}}); s != 400 {
			t.Fatalf("raised a PIP as a request: %d", s)
		}
	})

	t.Run("the approver's name is known before the PIP is raised, and listed for every PIP", func(t *testing.T) {
		s, a := c.do("GET", "/api/consequences/approver?employeeId=EMP-1020", "EMP-1009", "Line Manager", nil)
		if s != 200 || a["role"] != "HRBP" || a["name"] != hrbp || a["named"] != true {
			t.Fatalf("%d %v (simulate said %s)", s, a, hrbp)
		}
		if s, _ := c.do("GET", "/api/consequences/approver?employeeId=EMP-1020", "EMP-1020", "Employee", nil); s != 403 {
			t.Fatalf("an employee asked who approves plans: %d", s)
		}
		if s, _ := c.do("GET", "/api/consequences/approver?employeeId=EMP-1002", "EMP-1009", "Line Manager", nil); s != 403 {
			t.Fatalf("outside the line manager's team: %d", s)
		}
		_, r := c.do("POST", "/api/consequences", "EMP-1009", "Line Manager", map[string]any{"employeeId": "EMP-1020", "kind": "PIP", "days": 7, "reason": "Performance", "details": "Missed three handovers in October."})
		id := fmt.Sprint(r["id"])
		if s, r := c.do("POST", "/api/consequences/"+id+"/approve", "EMP-1004", "HRBP", map[string]string{"note": "Agreed"}); s != 200 {
			t.Fatal(r)
		}
		_, l := c.do("GET", "/api/consequences", "EMP-1009", "Line Manager", nil)
		for _, x := range l["rows"].([]any) {
			if m := x.(map[string]any); fmt.Sprint(m["id"]) == id {
				if m["status"] != "Active" || m["approver_role"] != "HRBP" || m["approver_name"] != hrbp {
					t.Fatalf("an active plan keeps the approver's name: %v %v %v", m["status"], m["approver_role"], m["approver_name"])
				}
				return
			}
		}
		t.Fatal("plan not listed")
	})

	t.Run("a PIP follows its approval flow", func(t *testing.T) {
		rules := []any{map[string]any{"id": "pip1", "label": "Otherwise", "on": true, "when": []any{}, "approvers": []string{"Cluster Manager"}, "notify": []string{}}}
		if s, r := c.do("PUT", "/api/approvals/flows/p_pip/Operations", "EMP-1002", "HR Admin", map[string]any{"rules": rules}); s != 200 {
			t.Fatal(r)
		}
		if _, a := c.do("GET", "/api/consequences/approver?employeeId=EMP-1014", "EMP-1009", "Line Manager", nil); a["role"] != "Cluster Manager" {
			t.Fatalf("approver after the change: %v", a)
		}
		_, r := c.do("POST", "/api/consequences", "EMP-1009", "Line Manager", map[string]any{"employeeId": "EMP-1014", "kind": "PIP", "days": 15, "reason": "Performance", "details": "Missed three handovers in October."})
		_, l := c.do("GET", "/api/consequences", "EMP-1009", "Line Manager", nil)
		for _, x := range l["rows"].([]any) {
			if m := x.(map[string]any); fmt.Sprint(m["id"]) == fmt.Sprint(r["id"]) && m["approver_role"] != "Cluster Manager" {
				t.Fatalf("the plan waits for %v", m["approver_role"])
			}
		}
	})
}

func TestUI3ApprCheckFlowReads(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1020")

	health := func(emp, role string) map[string]map[string]any {
		s, h := c.do("GET", "/api/approvals/health", emp, role, nil)
		if s != 200 {
			t.Fatalf("health as %s: %d %v", role, s, h)
		}
		out := map[string]map[string]any{}
		for _, x := range h["list"].([]any) {
			m := x.(map[string]any)
			out[m["title"].(string)] = m
		}
		return out
	}

	t.Run("the Employee view reads the health check and the flows, and changes nothing", func(t *testing.T) {
		if len(health("EMP-1020", "Employee")) != 6 {
			t.Fatal("six checks")
		}
		if s, mx := c.do("GET", "/api/approvals/flows", "EMP-1020", "Employee", nil); s != 200 || len(mx["list"].([]any)) < 10 {
			t.Fatalf("flows: %d", s)
		}
		if s, _ := c.do("PUT", "/api/approvals/flows/leaveC/Operations", "EMP-1020", "Employee", map[string]any{"rules": []any{map[string]any{"label": "Otherwise", "approvers": []string{"Line Manager"}}}}); s != 403 {
			t.Fatalf("an employee changed a flow: %d", s)
		}
		if s, _ := c.do("GET", "/api/approvals/approvers", "EMP-1020", "Employee", nil); s != 403 {
			t.Fatalf("the approver names tab stays with the approvals view: %d", s)
		}
		if s, _ := c.do("POST", "/api/approvals/flows/leaveC/Operations/test", "EMP-1020", "Employee", map[string]any{"facts": map[string]any{"roll": 2}}); s != 403 {
			t.Fatalf("an employee tested a flow in the editor: %d", s)
		}
		if s, _ := c.do("POST", "/api/approvals/simulate", "EMP-1020", "Employee", map[string]any{"wf": "p_pip", "payload": map[string]any{}}); s != 200 {
			t.Fatalf("own route for any request type: %d", s)
		}
	})

	t.Run("each check has its items, the count and the prototype's sentence", func(t *testing.T) {
		// Without the north HRBP, every north property misses its HRBP.
		if _, err := st.DB.C("user_role_map").UpdateMany(ctx, bson.M{"employee_id": "EMP-1004", "role_name": "HRBP"}, bson.M{"$set": bson.M{"active": false}}); err != nil {
			t.Fatal(err)
		}
		h := health("EMP-1002", "HR Admin")
		for title, m := range h {
			if int(num(m["count"])) != len(m["items"].([]any)) || (m["ok"] == true) != (num(m["count"]) == 0) {
				t.Fatalf("%s: count %v items %v ok %v", title, m["count"], m["items"], m["ok"])
			}
		}
		g := h["Every approver in use has a name"]
		items := g["items"].([]any)
		n := len(items)
		if n < 2 || g["fix"] != "names" || g["detail"] != fmt.Sprintf("%d missing, for example %s and %s", n, items[0], items[1]) || !strings.Contains(fmt.Sprint(items), "HRBP for Delhi") {
			t.Fatalf("names check: %v", g)
		}
		// The Employee view gets the count and only the examples the sentence names.
		e := health("EMP-1020", "Employee")["Every approver in use has a name"]
		if num(e["count"]) != float64(n) || len(e["items"].([]any)) != 2 || e["detail"] != g["detail"] {
			t.Fatalf("employee view: %v", e)
		}
		if l := h["Every named approver can sign in"]; l["ok"] == false && !strings.Contains(fmt.Sprint(l["detail"]), "cannot sign in, for example") {
			t.Fatalf("sign-in check: %v", l)
		}
	})

	t.Run("two people mapped to the same role and place are an overlap", func(t *testing.T) {
		if _, err := st.DB.C("user_role_map").UpdateMany(ctx, bson.M{"employee_id": "EMP-1004", "role_name": "HRBP"}, bson.M{"$set": bson.M{"active": true}}); err != nil {
			t.Fatal(err)
		}
		// EMP-1014 gets the same HRBP places as EMP-1004.
		maps, err := st.DB.C("user_role_map").Find(ctx, bson.M{"employee_id": "EMP-1004", "role_name": "HRBP"})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range maps {
			if _, err := st.DB.C("user_role_map").Insert(ctx, bson.M{"employee_id": "EMP-1014", "role_name": "HRBP", "access": "locs", "location_ids": m["location_ids"], "created_by": "test"}); err != nil {
				t.Fatal(err)
			}
		}
		var meera, other string
		if e := apiDoc(st, "employee", bson.M{"_id": "EMP-1004"}); e != nil {
			meera, _ = e["name"].(string)
		}
		if e := apiDoc(st, "employee", bson.M{"_id": "EMP-1014"}); e != nil {
			other, _ = e["name"].(string)
		}
		_, d := c.do("GET", "/api/approvals/approvers", "EMP-1002", "HR Admin", nil)
		ov := d["overlaps"].([]any)
		want := fmt.Sprintf(": %s and %s are both mapped. %s is used.", meera, other, meera)
		if len(ov) == 0 || !strings.HasPrefix(fmt.Sprint(ov[0]), "HRBP for ") || !strings.HasSuffix(fmt.Sprint(ov[0]), want) {
			t.Fatalf("overlaps: %v", ov)
		}
	})
}

func TestUI3ApprInfoTab(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1004", "EMP-1020")
	from := apprNextMonday(25).Format("2006-01-02")
	_, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "leave", "payload": map[string]any{"ltype": "Casual leave", "from": from, "to": from}})
	id, _ := r["id"].(string)
	if id == "" {
		t.Fatal(r)
	}
	// The HRBP is informed of this request, and it is already decided.
	if _, err := st.DB.C("approval_request").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"notify": `[{"role":"HRBP","person":"Meera"}]`, "status": "Approved"}}); err != nil {
		t.Fatal(err)
	}
	if apprRow(c, t, "EMP-1004", "HRBP", "info", "", id) == nil {
		t.Fatal("a completed request the role is informed of is in For information")
	}
	if _, cnt := c.do("GET", "/api/approvals/counts", "EMP-1004", "HRBP", nil); num(cnt["info"]) < 1 {
		t.Fatalf("counts: %v", cnt)
	}
	if apprRow(c, t, "EMP-1020", "Employee", "info", "", id) != nil {
		t.Fatal("only the informed role")
	}
}

func TestUI3ApprResignChangeWithdraw(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1020")
	sim := func(emp, role string, body map[string]any) map[string]any {
		t.Helper()
		s, r := c.do("POST", "/api/approvals/simulate", emp, role, body)
		if s != 200 {
			t.Fatalf("simulate: %d %v", s, r)
		}
		return r
	}

	t.Run("nothing to change or withdraw before a resignation is accepted", func(t *testing.T) {
		p := sim("EMP-1020", "Employee", map[string]any{"wf": "resign", "payload": map[string]any{"rkind": "withdraw"}})
		if !strings.Contains(fmt.Sprint(p["problems"]), "There is no accepted resignation to withdraw") {
			t.Fatal(p["problems"])
		}
		if s, _ := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "resign", "payload": map[string]any{"rkind": "withdraw"}}); s != 400 {
			t.Fatalf("withdrew a resignation that was never given: %d", s)
		}
	})

	_, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "resign", "payload": map[string]any{"reason": "Relocation"}})
	req, sep := fmt.Sprint(r["id"]), fmt.Sprint(r["case"])
	ui3ApproveAll(t, c, req)
	var lwd, notice string
	if e := apiDoc(st, "employee", bson.M{"_id": "EMP-1020", "exit_status": "On notice"}); e != nil {
		lwd, _ = e["lwd"].(string)
	}
	if r := apiDoc(st, "approval_request", bson.M{"_id": req}); r != nil {
		notice, _ = r["payload"].(map[string]any)["noticeLwd"].(string)
	}
	if lwd == "" || notice == "" {
		t.Fatal("the employee is on notice")
	}

	t.Run("HR changes the last working day, routed for approval", func(t *testing.T) {
		later := mustDate(lwd).AddDate(0, 0, 10).Format("2006-01-02")
		if s, _ := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "resign", "payload": map[string]any{"rkind": "change", "lwd": later}}); s != 403 {
			t.Fatalf("the employee view only withdraws: %d", s)
		}
		early := mustDate(notice).AddDate(0, 0, -3).Format("2006-01-02")
		s, e := c.do("POST", "/api/approvals", "EMP-1004", "HRBP", map[string]any{"wf": "resign", "employeeId": "EMP-1020", "payload": map[string]any{"rkind": "change", "lwd": early}})
		if s != 400 || !strings.HasPrefix(fmt.Sprint(e["error"]), "Policy notice is ") {
			t.Fatalf("an earlier date without a waiver: %d %v", s, e)
		}
		if p := sim("EMP-1004", "HRBP", map[string]any{"wf": "resign", "employeeId": "EMP-1020", "payload": map[string]any{"rkind": "change", "lwd": early, "waive": true}}); len(p["problems"].([]any)) != 0 || len(p["chain"].([]any)) == 0 {
			t.Fatalf("with a waiver: %v", p)
		}
		s, ch := c.do("POST", "/api/approvals", "EMP-1004", "HRBP", map[string]any{"wf": "resign", "employeeId": "EMP-1020", "payload": map[string]any{"rkind": "change", "lwd": later, "reason": "Death or permanent disability"}})
		if s != 200 {
			t.Fatal(ch)
		}
		cid := fmt.Sprint(ch["id"])
		_, d := c.do("GET", "/api/approvals/"+cid, "EMP-1004", "HRBP", nil)
		p := d["payload"].(map[string]any)
		if d["caseId"] != sep || p["rkind"] != "change" || p["lwd"] != later || p["fromLwd"] != lwd || p["caseId"] != sep {
			t.Fatalf("change request: %v %v", d["caseId"], p)
		}
		var still string
		if e := apiDoc(st, "employee", bson.M{"_id": "EMP-1020"}); e != nil {
			still, _ = e["lwd"].(string)
		}
		if still != lwd {
			t.Fatal("nothing moves before approval")
		}
		ui3ApproveAll(t, c, cid)
		var now, caseLwd string
		if s := apiDoc(st, "separation", bson.M{"_id": sep}); s != nil {
			if e := apiDoc(st, "employee", bson.M{"_id": s["employee_id"]}); e != nil {
				now, _ = e["lwd"].(string)
				caseLwd, _ = s["lwd"].(string)
			}
		}
		if now != later || caseLwd != later {
			t.Fatalf("last working day %s, case %s, want %s", now, caseLwd, later)
		}
		if note := ui3Applied(t, c, cid); !strings.Contains(note, "'s last working day changed from ") {
			t.Fatal(note)
		}
		lwd = later
	})

	t.Run("the employee withdraws, routed for approval, and the case is withdrawn", func(t *testing.T) {
		p := sim("EMP-1020", "Employee", map[string]any{"wf": "resign", "payload": map[string]any{"rkind": "withdraw"}})
		if len(p["problems"].([]any)) != 0 {
			t.Fatal(p["problems"])
		}
		s, w := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "resign", "payload": map[string]any{"rkind": "withdraw", "reason": "Relocation"}})
		if s != 200 {
			t.Fatal(w)
		}
		wid := fmt.Sprint(w["id"])
		if s, _ := c.do("POST", "/api/approvals", "EMP-1004", "HRBP", map[string]any{"wf": "resign", "employeeId": "EMP-1020", "payload": map[string]any{"rkind": "withdraw"}}); s != 400 {
			t.Fatal("one request at a time")
		}
		ui3ApproveAll(t, c, wid)
		var exit *string
		var status, note string
		if s := apiDoc(st, "separation", bson.M{"_id": sep}); s != nil {
			if e := apiDoc(st, "employee", bson.M{"_id": s["employee_id"]}); e != nil {
				if x, ok := e["exit_status"].(string); ok {
					exit = &x
				}
				status, _ = s["status"].(string)
				note, _ = s["close_note"].(string)
			}
		}
		if exit != nil || status != "Cancelled" || !strings.HasPrefix(note, "Withdrawn by ") {
			t.Fatalf("exit %v status %s note %s", exit, status, note)
		}
		_, d := c.do("GET", "/api/separations/"+sep, "EMP-1004", "HRBP", nil)
		if d["stage"] != "Withdrawn" {
			t.Fatal(d["stage"])
		}
		if note := ui3Applied(t, c, wid); !strings.HasSuffix(note, "'s resignation is withdrawn. Employment continues.") {
			t.Fatal(note)
		}
	})

	t.Run("a resignation is withdrawn only within the policy window", func(t *testing.T) {
		_, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "resign", "payload": map[string]any{"reason": "Relocation"}})
		ui3ApproveAll(t, c, fmt.Sprint(r["id"]))
		old := time.Now().AddDate(0, 0, -20).Format("2006-01-02")
		if _, err := st.DB.C("separation").Update(ctx, bson.M{"_id": fmt.Sprint(r["case"])}, bson.M{"$set": bson.M{"given_on": old}}); err != nil {
			t.Fatal(err)
		}
		s, e := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "resign", "payload": map[string]any{"rkind": "withdraw"}})
		if s != 400 || !strings.HasPrefix(fmt.Sprint(e["error"]), "Resignation can be withdrawn within ") {
			t.Fatalf("%d %v", s, e)
		}
	})
}

func TestUI3ApprExitReasons(t *testing.T) {
	c := setup(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1020")
	const death = "Death or permanent disability"
	_, mine := c.do("GET", "/api/me/separation", "EMP-1020", "Employee", nil)
	if containsAny(mine["reasons"].([]any), death) {
		t.Fatal("the employee's list has no death reason")
	}
	_, hr := c.do("GET", "/api/me/separation?employeeId=EMP-1020", "EMP-1004", "HRBP", nil)
	if !containsAny(hr["reasons"].([]any), death) {
		t.Fatal("HR's list has it")
	}
	if s, _ := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "resign", "payload": map[string]any{"reason": death}}); s != 400 {
		t.Fatal("the employee view cannot choose it")
	}
	if s, r := c.do("POST", "/api/approvals", "EMP-1004", "HRBP", map[string]any{"wf": "resign", "employeeId": "EMP-1020", "payload": map[string]any{"reason": death}}); s != 200 {
		t.Fatalf("HR records it: %v", r)
	}
}

func TestUI3ApprRosterChangeAndWording(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1011", "EMP-1012", "EMP-1020")
	day := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	var loc, dept string
	if e := apiDoc(st, "employee", bson.M{"_id": "EMP-1020"}); e != nil {
		if d := apiDoc(st, "department", bson.M{"_id": e["department_id"]}); d != nil {
			loc, _ = e["location_id"].(string)
			dept, _ = d["name"].(string)
		}
	}
	if _, err := st.DB.C("roster_entry").Upsert(ctx, bson.M{"employee_id": "EMP-1020", "date": day},
		bson.M{"code": "N", "why": "Staff absent or sick", "late": true, "by_name": "Rakesh Bajaj (Property Manager)"}, bson.M{"from_code": "G"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.C("roster_log").Insert(ctx, bson.M{"employee_id": "EMP-1020", "date": day, "from_code": "G", "to_code": "N", "why": "Staff absent or sick", "late": true,
		"location_id": loc, "department": dept, "by_name": "Rakesh Bajaj (Property Manager)"}); err != nil {
		t.Fatal(err)
	}

	t.Run("a regularization for a changed roster shows the change", func(t *testing.T) {
		s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "regularize", "payload": map[string]any{"date": day, "rtype": "All Other", "reason": "Roster changed", "in": "22:00", "out": "23:30"}})
		if s != 200 {
			t.Fatal(r)
		}
		id := fmt.Sprint(r["id"])
		_, d := c.do("GET", "/api/approvals/"+id, "EMP-1004", "HRBP", nil)
		rc, _ := d["rosterChange"].(map[string]any)
		if rc == nil || rc["late"] != true || rc["by"] != "Property Manager" || rc["byName"] != "Rakesh Bajaj" || rc["why"] != "Staff absent or sick" {
			t.Fatalf("roster change: %v", d["rosterChange"])
		}
		ui3ApproveAll(t, c, id)
		if note := ui3Applied(t, c, id); !strings.HasSuffix(note, ". Hours will count in payroll.") {
			t.Fatal(note)
		}
	})

	t.Run("the discipline report groups changes by role", func(t *testing.T) {
		_, d := c.do("GET", "/api/roster/discipline?ym="+day[:7], "EMP-1002", "HR Admin", nil)
		keys := []string{}
		for _, x := range d["byWho"].([]any) {
			keys = append(keys, fmt.Sprint(x.(map[string]any)["key"]))
		}
		if !containsAny(toAny(keys), "Property Manager") || strings.Contains(strings.Join(keys, "|"), "(") {
			t.Fatalf("by who: %v", keys)
		}
	})

	t.Run("a verified joiner is active for payroll", func(t *testing.T) {
		c.signIn("EMP-1009")
		si := selectCandidate(t, c, shortlistBody(st, "Tara", "Mehta", "9876501299"))
		offer := approvedOffer(t, c, st, si, "Tara", "Mehta", "9876501299")
		c.do("POST", "/api/offers/"+offer+"/release", "EMP-1004", "HRBP", nil)
		c.do("POST", "/api/offers/"+offer+"/accept", "EMP-1004", "HRBP", nil)
		if s, r := c.do("POST", "/api/candidates/"+si+"/joined", "EMP-1004", "HRBP", map[string]any{"date": today()}); s != 200 {
			t.Fatal(r)
		}
		join := map[string]any{"gender": "Female", "dob": "1998-05-04", "aadhaar": "123412341299", "pan": "abcde1299f", "uanAvail": "No", "pfChoice": "Yes",
			"bankName": "HDFC Bank", "acct": "50100123456799", "ifsc": "HDFC0001234", "shift": "G", "attMode": "Biometric", "l1": "EMP-1009"}
		if s, r := c.do("POST", "/api/offers/"+offer+"/joining", "EMP-1004", "HRBP", map[string]any{"data": join}); s != 200 {
			t.Fatal(r)
		}
		for _, k := range []string{"pan", "photo", "bankProof", "police"} {
			if s, r := c.upload("EMP-1004", "HRBP", "candidate", si, k); s != 200 {
				t.Fatal(r)
			}
		}
		s, v := c.do("POST", "/api/offers/"+offer+"/send", "EMP-1004", "HRBP", nil)
		if s != 200 {
			t.Fatal(v)
		}
		vid := v["id"].(string)
		c.do("POST", "/api/approvals/"+vid+"/approve", "EMP-1011", "Payroll", nil)
		if _, a := c.do("POST", "/api/approvals/"+vid+"/approve", "EMP-1012", "Payroll Manager", nil); a["status"] != "Approved" {
			t.Fatal(a)
		}
		// The background check holds the Employee ID by default (hiring.go).
		if note := ui3Applied(t, c, vid); note != "Tara Mehta is verified by Payroll and is now active for payroll. The Employee ID waits for a clear background check." {
			t.Fatal(note)
		}
	})
}

func TestUI3BulkWorkPolicy(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002")
	pol := func(id string) string {
		_, f := c.do("GET", "/api/employees/"+id, "EMP-1002", "HR Admin", nil)
		p, _ := f["policy"].(map[string]any)
		return fmt.Sprint(p["name"])
	}
	// A policy neither EMP-1016 nor EMP-1019 has now.
	cur, cur19 := pol("EMP-1016"), pol("EMP-1019")
	var other string
	if p := apiDoc(st, "work_policy", bson.M{"status": "Active", "name": bson.M{"$nin": []string{cur, cur19}}},
		options.FindOne().SetSort(bson.D{{Key: "sort", Value: 1}, {Key: "name", Value: 1}})); p != nil {
		other, _ = p["name"].(string)
	}
	if cur == "" || other == "" {
		t.Fatalf("policies: %q %q", cur, other)
	}
	head := "Employee ID,Work City,Reporting Location,Department,Designation,Grade,Shift,Reporting Manager L1,Functional Manager L2,Work Policy,Attendance Mode,Reason for change,Effective From\n"
	later := time.Now().AddDate(0, 0, 12).Format("2006-01-02")

	t.Run("the template has the column with a drop-down list", func(t *testing.T) {
		_, l := c.do("GET", "/api/bulk", "EMP-1002", "HR Admin", nil)
		for _, x := range l["list"].([]any) {
			if m := x.(map[string]any); m["key"] == "upd" && !strings.Contains(fmt.Sprint(m["cols"]), "Functional Manager L2 Work Policy Attendance Mode") {
				t.Fatal(m["cols"])
			}
		}
		old := strings.Replace(head, "Work Policy,", "", 1) + "EMP-1016,,,,,,,,,,Policy change," + today() + "\n"
		if s, r := c.sendFile("/api/bulk/upd/check", "EMP-1002", "HR Admin", "upd.csv", []byte(old)); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "Work Policy") {
			t.Fatalf("a file without the column: %d %v", s, r)
		}
	})

	t.Run("checked against the policy master, with the date and reason on the row", func(t *testing.T) {
		csv := head + "EMP-1016,,,,,,,,," + other + ",,policy change," + today() + "\n" +
			"EMP-1017,,,,,,,,,No Such Policy,,Policy change," + today() + "\n" +
			"EMP-1018,,,,,,,,," + cur + ",,," + "\n" +
			"EMP-1019,,,,,,E,,," + other + ",,Transfer," + later + "\n"
		_, chk := c.sendFile("/api/bulk/upd/check", "EMP-1002", "HR Admin", "upd.csv", []byte(csv))
		rs := chk["results"].([]any)
		if len(rs) != 4 {
			t.Fatal(chk)
		}
		a, b, cc := rs[0].(map[string]any), rs[1].(map[string]any), rs[3].(map[string]any)
		ch := a["changes"].([]any)
		if len(a["errors"].([]any)) != 0 || len(ch) != 1 || ch[0].(map[string]any)["field"] != "Work Policy" || ch[0].(map[string]any)["to"] != other || a["eff"] != today() || a["reason"] != "Policy change" {
			t.Fatalf("policy row: %v", a)
		}
		if e := fmt.Sprint(b["errors"]); !strings.Contains(e, "Work Policy “No Such Policy” not found") {
			t.Fatal(e)
		}
		if len(cc["changes"].([]any)) != 2 || cc["eff"] != later || cc["reason"] != "Transfer" {
			t.Fatalf("shift and policy, scheduled: %v", cc)
		}
		if n := apiCount(st, "employee_change", bson.M{"field": "Work Policy"}); n != 0 {
			t.Fatal("a check changes nothing")
		}
	})

	t.Run("applied now, or scheduled for its date", func(t *testing.T) {
		csv := head + "EMP-1016,,,,,,,,," + other + ",,Policy change," + today() + "\n" + "EMP-1019,,,,,,,,," + other + ",,Policy change," + later + "\n"
		if _, ap := c.sendFile("/api/bulk/upd/apply", "EMP-1002", "HR Admin", "upd.csv", []byte(csv)); num(ap["ok"]) != 2 {
			t.Fatal(ap)
		}
		_, f := c.do("GET", "/api/employees/EMP-1016", "EMP-1002", "HR Admin", nil)
		if p := f["policy"].(map[string]any); p["name"] != other || p["via"] != "Employee" {
			t.Fatalf("policy now: %v", p)
		}
		_, f = c.do("GET", "/api/employees/EMP-1019", "EMP-1002", "HR Admin", nil)
		if p := f["policy"].(map[string]any); p["name"] == other && p["via"] == "Employee" {
			t.Fatal("a future change waits for its date")
		}
		var state string
		if x := apiDoc(st, "employee_change", bson.M{"employee_id": "EMP-1019", "field": "Work Policy"}); x != nil {
			state, _ = x["state"].(string)
		}
		if state != "Scheduled" {
			t.Fatal(state)
		}
	})
}
