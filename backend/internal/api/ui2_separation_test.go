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

// Separation, round 2 of the UI: settings, history, rehire, terms by any current approver, the
// absconding check, the full and final letter and no dues certificate, and /me additions.

func histText(d map[string]any) string {
	out := []string{}
	for _, x := range d["history"].([]any) {
		m := x.(map[string]any)
		out = append(out, fmt.Sprintf("%v · %v · %v", m["t"], m["by"], m["role"]))
	}
	return strings.Join(out, "\n")
}

func sepRow(d map[string]any, id string) map[string]any {
	for _, x := range d["cases"].([]any) {
		if m := x.(map[string]any); m["id"] == id {
			return m
		}
	}
	return nil
}

func TestUI2SepSettings(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004")
	_, g := c.do("GET", "/api/separations/settings", "EMP-1002", "HR Admin", nil)
	if num(g["absDays"]) != 7 || num(g["absLetters"]) != 2 || num(g["absWait"]) != 3 || len(g["exitReq"].([]any)) != 3 || g["canEdit"] != true || len(g["items"].([]any)) != 7 {
		t.Fatalf("defaults equal the old constants: %v", g)
	}
	if _, h := c.do("GET", "/api/separations/settings", "EMP-1004", "HRBP", nil); h["canEdit"] != false {
		t.Fatal("an HRBP reads the settings but does not edit them")
	}
	if s, _ := c.do("PUT", "/api/separations/settings", "EMP-1004", "HRBP", map[string]any{"absDays": 5, "absLetters": 1, "absWait": 0, "exitReq": []string{}}); s != 403 {
		t.Fatal("only HR Admin edits")
	}
	if s, _ := c.do("PUT", "/api/separations/settings", "EMP-1002", "HR Admin", map[string]any{"absDays": 5, "absLetters": 6, "absWait": 0, "exitReq": []string{}}); s != 400 {
		t.Fatal("at most 5 letters")
	}
	if s, r := c.do("PUT", "/api/separations/settings", "EMP-1002", "HR Admin", map[string]any{"absDays": 5, "absLetters": 1, "absWait": 0, "exitReq": []string{"Resignation"}}); s != 200 {
		t.Fatal(r)
	}
	if _, g = c.do("GET", "/api/separations/settings", "EMP-1004", "HRBP", nil); num(g["absDays"]) != 5 || num(g["absLetters"]) != 1 || len(g["exitReq"].([]any)) != 1 {
		t.Fatalf("saved: %v", g)
	}

	// Checklist: add, no duplicates, rename, make inactive (never deleted); an automatic item keeps its rule.
	if s, _ := c.do("POST", "/api/separations/clearance-items", "EMP-1004", "HRBP", map[string]any{"owner": "Finance", "name": "Laptop bag returned"}); s != 403 {
		t.Fatal("only HR Admin adds items")
	}
	_, it := c.do("POST", "/api/separations/clearance-items", "EMP-1002", "HR Admin", map[string]any{"owner": "Finance", "name": "Salary advance closed"})
	if it["id"] == nil || it["status"] != "Active" {
		t.Fatal(it)
	}
	if s, _ := c.do("POST", "/api/separations/clearance-items", "EMP-1002", "HR Admin", map[string]any{"owner": "Finance", "name": "salary ADVANCE closed"}); s != 400 {
		t.Fatal("already in the list")
	}
	if s, _ := c.do("POST", "/api/separations/clearance-items", "EMP-1002", "HR Admin", map[string]any{"owner": "Payroll", "name": "x"}); s != 400 {
		t.Fatal("owner is one of the three")
	}
	newID := fmt.Sprint(it["id"])
	if s, r := c.do("PUT", "/api/separations/clearance-items/"+newID, "EMP-1002", "HR Admin", map[string]any{"name": "Salary advance recovered"}); s != 200 || r["name"] != "Salary advance recovered" {
		t.Fatal(r)
	}
	var autoID int
	if x, _ := st.DB.C("clearance_item").FindOne(ctx, bson.M{"auto": "assets"}); x != nil {
		autoID = int(x["id"].(int32))
	}
	if s, r := c.do("PUT", fmt.Sprintf("/api/separations/clearance-items/%d", autoID), "EMP-1002", "HR Admin", map[string]any{"name": "Assets and uniform back in the store"}); s != 200 || r["auto"] != "assets" {
		t.Fatalf("automatic item keeps its rule: %v", r)
	}
	if s, r := c.do("PUT", "/api/separations/clearance-items/"+newID, "EMP-1002", "HR Admin", map[string]any{"status": "Inactive"}); s != 200 || r["status"] != "Inactive" {
		t.Fatal(r)
	}
	n, _ := st.DB.C("clearance_item").Count(ctx, nil)
	if n != 8 {
		t.Fatalf("items are never deleted: %d", n)
	}

	// The rules follow the settings: one letter and no wait before an absconding case goes for approval,
	// and no exit interview for a contract end.
	_, a := c.do("POST", "/api/separations", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1022", "type": "Absconding", "note": "Not reachable on phone since Monday"})
	abs, _ := a["id"].(string)
	if abs == "" {
		t.Fatal(a)
	}
	_, d := c.do("GET", "/api/separations/"+abs, "EMP-1004", "HRBP", nil)
	if d["next"] != "HRBP sends letter 1 of 1" {
		t.Fatal(d["next"])
	}
	if s, r := c.do("POST", "/api/separations/"+abs+"/show-cause", "EMP-1004", "HRBP", nil); s != 200 {
		t.Fatal(r)
	}
	if s, r := c.do("POST", "/api/separations/"+abs+"/send", "EMP-1004", "HRBP", nil); s != 200 {
		t.Fatalf("sent after one letter and no wait: %v", r)
	}
	_, ce := c.do("POST", "/api/separations", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1026", "type": "Contract end", "lwd": time.Now().AddDate(0, 0, 5).Format("2006-01-02")})
	_, d = c.do("GET", "/api/separations/"+ce["id"].(string), "EMP-1002", "HR Admin", nil)
	if d["exitNeeded"] != false {
		t.Fatal("contract end no longer needs an exit interview")
	}
	for _, x := range d["clearance"].([]any) {
		m := x.(map[string]any)
		if m["id"] == it["id"] {
			t.Fatal("an inactive item is not on the checklist")
		}
		if m["auto"] == "exit" && m["done"] != true {
			t.Fatal("exit interview item is done when not needed")
		}
	}
	// Not required any more, but HR may still hold an optional one (prototype: "Optional for Contract end").
	if s, r := c.do("POST", "/api/separations/"+ce["id"].(string)+"/interview", "EMP-1002", "HR Admin", map[string]any{"mainReason": "Other", "recommend": "Yes",
		"ratings": map[string]int{"Manager": 3, "Work and shifts": 3, "Pay and benefits": 3, "Growth": 3, "Workplace and team": 3}}); s != 200 {
		t.Fatalf("HR may hold an optional interview for a contract end: %v", r)
	}
}

func TestUI2SepCase(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1008", "EMP-1009", "EMP-1010", "EMP-1020")
	_, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "resign", "payload": map[string]any{"reason": "Relocation"}})
	req, _ := r["id"].(string)
	sep, _ := r["case"].(string)
	if req == "" || sep == "" {
		t.Fatal(r)
	}

	t.Run("me: manager's open cases and scope names", func(t *testing.T) {
		for _, role := range []string{"Employee", "Line Manager"} {
			if _, m := c.do("GET", "/api/me", "EMP-1009", role, nil); num(m["ctx"].(map[string]any)["mgrCases"]) != 1 {
				t.Fatalf("%s view: %v", role, m["ctx"])
			}
		}
		_, m := c.do("GET", "/api/me", "EMP-1004", "HRBP", nil)
		x := m["ctx"].(map[string]any)
		if len(x["scopeLocations"].([]any)) != 3 || len(x["scopeDepartments"].([]any)) != 0 || num(x["mgrCases"]) != 0 || x["rights"] == nil {
			t.Fatalf("HRBP north: %v", x)
		}
		_, m = c.do("GET", "/api/me", "EMP-1010", "Function Head", nil)
		x = m["ctx"].(map[string]any)
		if len(x["scopeLocations"].([]any)) != 0 || fmt.Sprint(x["scopeDepartments"]) != "[Sales]" {
			t.Fatalf("Function Head: %v", x)
		}
		_, m = c.do("GET", "/api/me", "EMP-1002", "HR Admin", nil)
		if x = m["ctx"].(map[string]any); len(x["scopeLocations"].([]any)) != 0 {
			t.Fatal("all locations: empty")
		}
		_, n := c.do("GET", "/api/me/separation", "EMP-1020", "Employee", nil)
		if p, _ := n["notice"].(map[string]any)["policy"].(string); p == "" {
			t.Fatalf("notice policy name: %v", n["notice"])
		}
	})

	t.Run("list: designation, manager, next approver, waiting for me", func(t *testing.T) {
		_, l := c.do("GET", "/api/separations", "EMP-1009", "Line Manager", nil)
		row := sepRow(l, sep)
		if row == nil || row["designation"] == "" || row["managerId"] != "EMP-1009" || row["waitingForMe"] != true || row["next"] != "Line Manager: Anil Kapoor" {
			t.Fatalf("line manager's row: %v", row)
		}
		_, l = c.do("GET", "/api/separations", "EMP-1004", "HRBP", nil)
		if row = sepRow(l, sep); row == nil || row["waitingForMe"] != false {
			t.Fatalf("HRBP row: %v", row)
		}
		_, d := c.do("GET", "/api/separations/"+sep, "EMP-1004", "HRBP", nil)
		rq := d["request"].(map[string]any)
		if d["next"] != "Line Manager: Anil Kapoor" || len(rq["people"].([]any)) < 2 || d["hrbp"] == "" || d["hrbp"] == nil || num(rq["stepIndex"]) != 0 {
			t.Fatalf("detail: next %v people %v hrbp %v", d["next"], rq["people"], d["hrbp"])
		}
	})

	t.Run("terms by the current approver without the separation right", func(t *testing.T) {
		if s, r := c.do("POST", "/api/approvals/"+req+"/approve", "EMP-1009", "Line Manager", map[string]any{"note": "ok"}); s != 200 {
			t.Fatal(r)
		}
		_, _ = st.DB.C("role_right").DeleteMany(ctx, bson.M{"role_name": "Property Manager", "right_id": "emp.sep"})
		if s, _ := c.do("GET", "/api/separations/"+sep, "EMP-1008", "Property Manager", nil); s != 403 {
			t.Fatal("the property manager cannot view separations now")
		}
		_, d := c.do("GET", "/api/separations/"+sep, "EMP-1004", "HRBP", nil)
		notice := d["request"].(map[string]any)["noticeLwd"].(string)
		early := mustDate(notice).AddDate(0, 0, -5).Format("2006-01-02")
		if s, _ := c.do("POST", "/api/separations/"+sep+"/terms", "EMP-1009", "Line Manager", map[string]any{"lwd": early, "shortfall": "waive"}); s != 403 {
			t.Fatal("the line manager has approved; the property manager acts now")
		}
		if s, r := c.do("POST", "/api/separations/"+sep+"/terms", "EMP-1008", "Property Manager", map[string]any{"lwd": early, "shortfall": "waive"}); s != 200 || r["termsBy"] != "Rakesh Bajaj" {
			t.Fatalf("%d %v", s, r)
		}
		_, d = c.do("GET", "/api/separations/"+sep, "EMP-1004", "HRBP", nil)
		rq := d["request"].(map[string]any)
		if rq["termsBy"] != "Rakesh Bajaj" || rq["termsRole"] != "Property Manager" || rq["lwd"] != early || d["lwd"] != early {
			t.Fatalf("terms recorded: %v", rq)
		}
		if h := histText(d); !strings.Contains(h, "brought earlier") || !strings.Contains(h, "Rakesh Bajaj (Property Manager)") || !strings.Contains(h, "Line Manager: approved") {
			t.Fatal(h)
		}
		_, a := c.do("GET", "/api/approvals/"+req, "EMP-1002", "HR Admin", nil)
		if a["payload"].(map[string]any)["termsBy"] != "Rakesh Bajaj" || !strings.Contains(fmt.Sprint(a["history"]), "Terms changed") {
			t.Fatalf("request detail: %v", a["payload"])
		}
	})

	t.Run("rehire at any stage, by HR", func(t *testing.T) {
		if s, _ := c.do("POST", "/api/separations/"+sep+"/rehire", "EMP-1009", "Line Manager", map[string]any{"rehire": "Eligible"}); s != 403 {
			t.Fatal("HR decides")
		}
		if s, _ := c.do("POST", "/api/separations/"+sep+"/rehire", "EMP-1004", "HRBP", map[string]any{"rehire": "Maybe"}); s != 400 {
			t.Fatal("unknown value")
		}
		if s, r := c.do("POST", "/api/separations/"+sep+"/rehire", "EMP-1004", "HRBP", map[string]any{"rehire": "Not eligible"}); s != 200 {
			t.Fatal(r)
		}
		_, d := c.do("GET", "/api/separations/"+sep, "EMP-1004", "HRBP", nil)
		if d["rehire"] != "Not eligible" || d["stage"] != "Approval" || !strings.Contains(histText(d), "Eligible for rehire set to No") {
			t.Fatalf("%v %v", d["rehire"], histText(d))
		}
	})

	t.Run("notice: manager's clearance, last working day change, history", func(t *testing.T) {
		c.approveAll(t, req)
		_, l := c.do("GET", "/api/separations", "EMP-1009", "Employee", nil)
		if row := sepRow(l, sep); row == nil || row["stage"] != "Notice" || row["waitingForMe"] != true {
			t.Fatalf("waiting for the manager's clearance: %v", row)
		}
		_, d := c.do("GET", "/api/separations/"+sep, "EMP-1009", "Line Manager", nil)
		for _, x := range d["clearance"].([]any) {
			if m := x.(map[string]any); m["owner"] == "Reporting manager" {
				if s, r := c.do("POST", fmt.Sprintf("/api/separations/%s/clearance/%v", sep, m["id"]), "EMP-1009", "Employee", nil); s != 200 {
					t.Fatal(r)
				}
			}
		}
		_, l = c.do("GET", "/api/separations", "EMP-1009", "Line Manager", nil)
		if row := sepRow(l, sep); row["waitingForMe"] != false {
			t.Fatal("manager's part done")
		}
		_, l = c.do("GET", "/api/separations", "EMP-1004", "HRBP", nil)
		if row := sepRow(l, sep); row["waitingForMe"] != true {
			t.Fatal("the HRBP part is open")
		}
		lwd := time.Now().AddDate(0, 0, 3).Format("2006-01-02")
		if s, r := c.do("POST", "/api/separations/"+sep+"/lwd", "EMP-1004", "HRBP", map[string]any{"lwd": lwd, "note": "Agreed with the manager"}); s != 200 {
			t.Fatal(r)
		}
		_, d = c.do("GET", "/api/separations/"+sep, "EMP-1004", "HRBP", nil)
		h := histText(d)
		for _, want := range []string{"Resignation started, last working day", "Reporting manager: ticked", "Last working day changed from", "Agreed with the manager", "Approved by the last approver"} {
			if !strings.Contains(h, want) {
				t.Fatalf("history lacks %q:\n%s", want, h)
			}
		}
		first := d["history"].([]any)[0].(map[string]any)
		if first["by"] != "Karan Khan" || first["role"] != "Employee" {
			t.Fatalf("oldest first, name and role apart: %v", first)
		}
	})

	t.Run("absent check for the start dialog", func(t *testing.T) {
		_, _ = st.DB.C("attendance_day").Upsert(ctx, bson.M{"employee_id": "EMP-1022", "date": store.DateOf(time.Now().AddDate(0, 0, -12))}, nil,
			bson.M{"in_time": "09:00", "out_time": "18:00", "source": "MIS upload", "by_name": "test"})
		s, a := c.do("GET", "/api/separations/absent?employeeId=EMP-1022", "EMP-1004", "HRBP", nil)
		if s != 200 || a["lastPunch"] != time.Now().AddDate(0, 0, -12).Format("2006-01-02") || num(a["window"]) != 7 || a["absentDays"] == nil {
			t.Fatalf("%d %v", s, a)
		}
		if s, _ := c.do("GET", "/api/separations/absent?employeeId=EMP-1022", "EMP-1020", "Employee", nil); s != 403 {
			t.Fatal("HR only")
		}
	})

	t.Run("cancel: closed by and when", func(t *testing.T) {
		_, x := c.do("POST", "/api/separations", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1026", "type": "Contract end", "lwd": time.Now().AddDate(0, 0, 5).Format("2006-01-02")})
		id := x["id"].(string)
		if s, r := c.do("POST", "/api/separations/"+id+"/cancel", "EMP-1004", "HRBP", map[string]any{"note": "Contract extended"}); s != 200 {
			t.Fatal(r)
		}
		_, d := c.do("GET", "/api/separations/"+id, "EMP-1004", "HRBP", nil)
		h := histText(d)
		if d["closedBy"] != "Meera Kulkarni" || d["closedAt"] == nil || !strings.Contains(h, "Sent for approval as") || !strings.Contains(h, "Cancelled: Contract extended") {
			t.Fatalf("%v %v\n%s", d["closedBy"], d["closedAt"], h)
		}
	})
}

func TestUI2SepFnf(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1011", "EMP-1020")
	_, r := c.do("POST", "/api/separations", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1026", "type": "Contract end", "lwd": time.Now().AddDate(0, 0, 3).Format("2006-01-02")})
	sep := r["id"].(string)
	c.approveAll(t, r["requestId"].(string))
	_, _ = st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1026"}, bson.M{"$set": bson.M{"lwd": store.DateOf(time.Now().AddDate(0, 0, -1))}})

	_, l := c.do("GET", "/api/fnf", "EMP-1011", "Payroll", nil)
	found := false
	for _, x := range l["rows"].([]any) {
		if m := x.(map[string]any); m["id"] == "EMP-1026" {
			found = true
			if _, ok := m["estNet"].(float64); !ok {
				t.Fatalf("estimated net for an unprepared row: %v", m)
			}
		}
	}
	if !found {
		t.Fatal("EMP-1026 is on notice")
	}
	_, pv := c.do("GET", "/api/fnf/EMP-1026", "EMP-1011", "Payroll", nil)
	if _, ok := pv["assets"].([]any); !ok {
		t.Fatalf("assets list: %v", pv)
	}
	if s, _ := c.do("GET", "/api/fnf/EMP-1026/letter", "EMP-1011", "Payroll", nil); s != 400 {
		t.Fatal("the letter follows the approved settlement")
	}
	_, d := c.do("GET", "/api/separations/"+sep, "EMP-1002", "HR Admin", nil)
	for _, x := range d["clearance"].([]any) {
		if m := x.(map[string]any); m["auto"] == nil {
			if s, r := c.do("POST", fmt.Sprintf("/api/separations/%s/clearance/%v", sep, m["id"]), "EMP-1002", "HR Admin", nil); s != 200 {
				t.Fatal(r)
			}
		}
	}
	ratings := map[string]any{"Manager": 3, "Work and shifts": 3, "Pay and benefits": 3, "Growth": 3, "Workplace and team": 3}
	if s, r := c.do("POST", "/api/separations/"+sep+"/interview", "EMP-1002", "HR Admin", map[string]any{"mainReason": "Other", "ratings": ratings, "recommend": "Yes"}); s != 200 {
		t.Fatal(r)
	}
	_, f := c.do("POST", "/api/fnf/EMP-1026", "EMP-1011", "Payroll", nil)
	fid, _ := f["id"].(string)
	if fid == "" {
		t.Fatal(f)
	}
	c.approveAll(t, fid)

	s, lt := c.do("GET", "/api/fnf/EMP-1026/letter", "EMP-1011", "Payroll", nil)
	h, _ := lt["html"].(string)
	if s != 200 || !strings.Contains(h, "<h1>Full and final settlement</h1>") || !strings.Contains(h, "Net payable") || !strings.Contains(h, "Authorised signatory") || lt["file"] != "full-and-final-EMP-1026.html" {
		t.Fatalf("%d %v", s, lt)
	}
	if s, _ := c.do("GET", "/api/fnf/"+fid+"/letter", "EMP-1011", "Payroll", nil); s != 200 {
		t.Fatal("by settlement id too")
	}
	if s, _ := c.do("GET", "/api/fnf/EMP-1026/letter", "EMP-1020", "Employee", nil); s != 403 {
		t.Fatal("not for an employee")
	}
	s, nd := c.do("GET", "/api/fnf/EMP-1026/no-dues", "EMP-1011", "Payroll", nil)
	if b, _ := nd["body"].(string); s != 200 || !strings.Contains(b, "has returned all company assets and has no pending dues") || !strings.Contains(b, "subject to the full and final settlement") {
		t.Fatalf("%d %v", s, nd)
	}
	// HR's issued certificate is the one Payroll reads, and it carries the same words.
	if s, r := c.do("POST", "/api/separations/"+sep+"/letter", "EMP-1004", "HRBP", map[string]any{"kind": "No dues certificate"}); s != 200 {
		t.Fatal(r)
	}
	_, nd = c.do("GET", "/api/fnf/EMP-1026/no-dues", "EMP-1011", "Payroll", nil)
	if nd["by_name"] != "Meera Kulkarni" || !strings.Contains(nd["body"].(string), "subject to the full and final settlement") {
		t.Fatal(nd)
	}
	_, d = c.do("GET", "/api/separations/"+sep, "EMP-1004", "HRBP", nil)
	if hx := histText(d); !strings.Contains(hx, "Full and final "+fid+" sent for approval") || !strings.Contains(hx, "Full and final "+fid+" approved") || !strings.Contains(hx, "No dues certificate issued") ||
		!strings.Contains(hx, "HRBP clearance complete") || !strings.Contains(hx, "Finance clearance complete") {
		t.Fatal(hx)
	}
}
