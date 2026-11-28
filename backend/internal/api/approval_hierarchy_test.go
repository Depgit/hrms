package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The "Approval hierarchy required" tick of a flow (meaning of 21 Nov 2026): cleared, a new request goes to nobody
// and is approved at once (history line, the usual next steps). A request raised before and still waiting keeps its
// approvers, and HR may skip it with a reason; skipping is refused without a reason, for a non-HR role, for an HR
// role that neither raised it nor sets up approvals, and on a ticked flow.
func TestApprovalHierarchyOff(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1014", "EMP-1011", "EMP-1004")
	hr := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		return c.do(method, path, "EMP-1002", "HR Admin", body)
	}
	claim := func() map[string]any {
		t.Helper()
		_, doc := c.upload("EMP-1014", "Employee", "employee", "EMP-1014", "Bill")
		s, r := c.do("POST", "/api/approvals", "EMP-1014", "Employee", map[string]any{"wf": "p_claim", "payload": map[string]any{"amount": 700, "claimType": "Travel", "billDate": today(), "doc": doc["id"]}})
		if s != 200 || r["id"] == nil {
			t.Fatalf("claim: %d %v", s, r)
		}
		_, d := hr("GET", "/api/approvals/"+r["id"].(string), nil)
		return d
	}
	rulesOf := func(flow string) string {
		t.Helper()
		_, l := hr("GET", "/api/approvals/flows", nil)
		for _, x := range l["list"].([]any) {
			if m := x.(map[string]any); m["wf"] == "p_claim" && m["flow"] == flow {
				b, _ := json.Marshal(m["rules"])
				return string(b)
			}
		}
		t.Fatalf("no p_claim/%s", flow)
		return ""
	}
	skip := func(id, emp, role, reason string) (int, map[string]any) {
		t.Helper()
		return c.do("POST", "/api/approvals/"+id+"/skip", emp, role, map[string]any{"reason": reason})
	}

	first := claim()
	flow := fmt.Sprint(first["flow"])
	if first["status"] != "Pending" || len(first["chain"].([]any)) == 0 || first["canSkip"] != false {
		t.Fatalf("ticked: %v %v %v", first["status"], first["chain"], first["canSkip"])
	}
	if s, _ := skip(first["id"].(string), "EMP-1002", "HR Admin", "Urgent"); s != 400 {
		t.Fatalf("skipped on a ticked flow: %d", s)
	}
	path := "/api/approvals/flows/p_claim/" + flow + "/hierarchy"
	before := rulesOf(flow)
	if s, _ := c.do("PUT", path, "EMP-1014", "Employee", map[string]any{"on": false}); s != 403 {
		t.Fatalf("an employee cleared the tick: %d", s)
	}

	s, r := hr("PUT", path, map[string]any{"on": false})
	if s != 200 || r["waiting"] != float64(1) || r["hierarchy"] != false {
		t.Fatalf("clear: %d %v", s, r)
	}
	if got := rulesOf(flow); got != before {
		t.Fatalf("the rules changed:\n%s\n%s", before, got)
	}
	_, tr := hr("POST", "/api/approvals/flows/p_claim/"+flow+"/test", map[string]any{"facts": map[string]any{"amt": 700}})
	if tr["hierarchyOff"] != true || len(tr["chain"].([]any)) != 0 {
		t.Fatalf("test a request: %v", tr)
	}

	// Unticked: a new request is approved at once, nobody asked, its next step done (the claim's ledger line).
	auto := claim()
	if auto["status"] != "Approved" || len(auto["chain"].([]any)) != 0 || auto["canSkip"] != false {
		t.Fatalf("hierarchy off: %v %v %v", auto["status"], auto["chain"], auto["canSkip"])
	}
	note := false
	for _, h := range auto["history"].([]any) {
		note = note || h.(map[string]any)["note"] == "Approved automatically: approval hierarchy is off"
	}
	if !note {
		t.Fatalf("auto history: %v", auto["history"])
	}
	if n, _ := st.DB.C("ledger").Count(ctx, bson.M{"source_id": auto["id"]}); n != 1 {
		t.Fatalf("auto ledger lines: %d", n)
	}
	if _, sc := hr("GET", "/api/approvals/skip-check?wf=p_claim&subject=EMP-1014", nil); sc["skip"] != false {
		t.Fatalf("skip offered on a raise form: %v", sc)
	}

	// The request raised before the tick was cleared still waits for its approvers; HR may skip it.
	id := first["id"].(string)
	if _, w := hr("GET", "/api/approvals/"+id, nil); w["status"] != "Pending" || w["canSkip"] != true {
		t.Fatalf("waiting one: %v %v", w["status"], w["canSkip"])
	}
	if s, _ := skip(id, "EMP-1002", "HR Admin", "  "); s != 400 {
		t.Fatalf("skipped without a reason: %d", s)
	}
	if s, _ := skip(id, "EMP-1011", "Payroll", "Urgent"); s != 403 {
		t.Fatalf("a non-HR role skipped: %d", s)
	}
	if s, _ := skip(id, "EMP-1004", "HRBP", "Urgent"); s != 403 {
		t.Fatalf("an HRBP who did not raise it skipped: %d", s)
	}
	if s, _ := skip(id, "EMP-1014", "Employee", "Urgent"); s != 403 {
		t.Fatalf("the employee skipped: %d", s)
	}
	if s, r := skip(id, "EMP-1002", "HR Admin", "Bill checked by HR"); s != 200 || r["status"] != "Approved" {
		t.Fatalf("skip: %d %v", s, r)
	}
	_, done := hr("GET", "/api/approvals/"+id, nil)
	found := false
	for _, h := range done["history"].([]any) {
		m := h.(map[string]any)
		found = found || (m["action"] == "Approved" && m["note"] == "Approval skipped by Sandeep Rawat: Bill checked by HR")
	}
	if done["status"] != "Approved" || done["canSkip"] != false || !found {
		t.Fatalf("after skip: %v %v %v", done["status"], done["canSkip"], done["history"])
	}
	if n, _ := st.DB.C("audit_log").Count(ctx, bson.M{"action": "Approval skipped", "entity_id": id}); n != 1 {
		t.Fatalf("audit: %d", n)
	}
	if n, _ := st.DB.C("ledger").Count(ctx, bson.M{"source_id": id}); n != 1 {
		t.Fatalf("ledger lines: %d", n)
	}

	if s, r := hr("PUT", path, map[string]any{"on": true}); s != 200 || r["hierarchy"] != true {
		t.Fatalf("tick: %d %v", s, r)
	}
	again := claim()
	if again["status"] != "Pending" || again["canSkip"] != false {
		t.Fatalf("ticked again: %v %v", again["status"], again["canSkip"])
	}
	if s, _ := skip(again["id"].(string), "EMP-1002", "HR Admin", "Urgent"); s != 400 {
		t.Fatalf("skipped after ticking again: %d", s)
	}
	if n, _ := st.DB.C("audit_log").Count(ctx, bson.M{"entity": "approval_flow", "entity_id": "p_claim/" + flow}); n != 2 {
		t.Fatalf("tick audits: %d", n)
	}
}
