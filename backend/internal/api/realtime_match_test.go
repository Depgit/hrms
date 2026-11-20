package api_test

import (
	"context"
	"encoding/csv"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/api"
	"github.com/saltstayz/hrms/internal/realtime"
)

// "Match portal codes" (realtime_match.go): fetching the portal's employee list, the proposals, applying them,
// the downloads and the rights. The portal is faked with made-up rows built from the sample employees.
func TestRealtimeMatchPortalCodes(t *testing.T) {
	c, app, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1020")
	ctx := context.Background()

	name := func(id string) string { return fmt.Sprint(apiDoc(st, "employee", bson.M{"_id": id})["name"]) }
	pers := apiDoc(st, "employee_personal", bson.M{"employee_id": "EMP-1020"})
	doj9 := fmt.Sprint(apiDoc(st, "employee", bson.M{"_id": "EMP-1009"})["doj"])
	// EMP-1014 already has a code; its portal row is not proposed again.
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1014"}, bson.M{"$set": bson.M{"attendance_code": "7777"}}); err != nil {
		t.Fatal(err)
	}
	portal := []realtime.Employee{
		{Name: name("EMP-1020"), Code: "1201", Card: "00001201", Phone: fmt.Sprint(pers["mobile"])},                        // Sure: mobile and name
		{Name: name("EMP-1009"), Code: "1209", Card: "00001209", DOJ: doj9},                                                // Sure: name and date of joining
		{Name: name("EMP-1011"), Code: "1211", Card: "00001211"},                                                           // Likely: name only
		{Name: name("EMP-1014"), Code: "7777", Card: "00007777"},                                                           // already given
		{Name: "Made Up Person", Code: "9100", Card: "00009100", Dept: "Kitchen", Desig: "Commi I", Branch: "SaltStayz X"}, // not in the HRMS
	}
	calls := 0
	undo := api.SetRealtimeEmployees(realtime.Config{BaseURL: "http://203.0.113.10:83", User: "u", Pass: "p"}, true,
		func(context.Context, realtime.Config) ([]realtime.Employee, error) { calls++; return portal, nil })
	defer undo()

	// Rights: an employee can neither see nor start nor apply.
	if s, _ := c.do("GET", "/api/attendance/realtime/match", "EMP-1020", "Employee", nil); s != 403 {
		t.Fatalf("employee GET: %d", s)
	}
	if s, _ := c.do("POST", "/api/attendance/realtime/match", "EMP-1020", "Employee", nil); s != 403 {
		t.Fatalf("employee start: %d", s)
	}
	if s, _ := c.do("POST", "/api/attendance/realtime/match/apply", "EMP-1020", "Employee", map[string]any{"pairs": []any{map[string]any{"employeeId": "EMP-1020", "code": "1201"}}}); s != 403 {
		t.Fatalf("employee apply: %d", s)
	}

	// Before any fetch.
	if s, g := c.do("GET", "/api/attendance/realtime/match", "EMP-1002", "HR Admin", nil); s != 200 || g["envSet"] != true || g["fetchedAt"] != nil || g["total"] != 0.0 {
		t.Fatalf("empty: %d %v", s, g)
	}
	// Start (the server under test may run it in the background); then run it at once to be sure of the result.
	if s, g := c.do("POST", "/api/attendance/realtime/match", "EMP-1002", "HR Admin", nil); s != 200 || g["started"] != true {
		t.Fatalf("start: %d %v", s, g)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, g := c.do("GET", "/api/attendance/realtime/match", "EMP-1002", "HR Admin", nil)
		if g["running"] == nil && g["fetchedAt"] != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fetch did not finish: %v", g)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if calls == 0 {
		t.Fatal("the portal was not asked")
	}
	if a := apiDoc(st, "audit_log", bson.M{"action": "Employee list fetched from the Realtime portal to match Attendance codes"}); a == nil {
		t.Fatal("start not audited")
	}

	s, g := c.do("GET", "/api/attendance/realtime/match?level=Sure", "EMP-1002", "HR Admin", nil)
	counts, _ := g["counts"].(map[string]any)
	if s != 200 || g["portal"] != 5.0 || g["used"] != 1.0 || g["leftover"] != 1.0 || counts["Sure"] != 2.0 || counts["Likely"] != 1.0 || g["total"] != 2.0 {
		t.Fatalf("result: %d %v", s, g)
	}
	picks := map[string]string{}
	for _, l := range []string{"Sure", "Likely"} {
		_, g := c.do("GET", "/api/attendance/realtime/match?level="+l, "EMP-1002", "HR Admin", nil)
		for _, x := range g["rows"].([]any) {
			r := x.(map[string]any)
			picks[fmt.Sprint(r["employeeId"])] = l + " " + fmt.Sprint(r["pick"].(map[string]any)["code"])
		}
	}
	if picks["EMP-1020"] != "Sure 1201" || picks["EMP-1009"] != "Sure 1209" || picks["EMP-1011"] != "Likely 1211" {
		t.Fatalf("picks: %v", picks)
	}
	// Search and paging.
	if _, g := c.do("GET", "/api/attendance/realtime/match?level=All&q=emp-1020&size=1", "EMP-1002", "HR Admin", nil); g["total"] != 1.0 || len(g["rows"].([]any)) != 1 {
		t.Fatalf("search: %v", g)
	}

	// Downloads: the proposals in the Employee master update format, and the portal people not in the HRMS.
	s, raw := c.getRaw("/api/attendance/realtime/match/download?format=csv", "EMP-1002", "HR Admin")
	recs, _ := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"))).ReadAll()
	if s != 200 || len(recs) != 4 || recs[0][0] != "Employee ID" || recs[0][len(recs[0])-1] != "Attendance code" {
		t.Fatalf("download: %d %v", s, recs)
	}
	got := map[string]string{}
	for _, r := range recs[1:] {
		got[r[0]] = r[len(r)-1]
	}
	if got["EMP-1020"] != "1201" || got["EMP-1011"] != "1211" {
		t.Fatalf("download rows: %v", got)
	}
	// The file goes through Bulk update as it is.
	if _, r := c.sendFile("/api/bulk/upd/check", "EMP-1002", "HR Admin", "codes.csv", raw); r["ok"] != 3.0 {
		t.Fatalf("bulk check of the download: %v", r)
	}
	if s, raw := c.getRaw("/api/attendance/realtime/match/download?what=portal&format=csv", "EMP-1002", "HR Admin"); s != 200 || !strings.Contains(string(raw), "Made Up Person") || strings.Contains(string(raw), "7777") {
		t.Fatalf("portal download: %d %s", s, raw)
	}

	// Apply: two good, and the refusals.
	pairs := []any{
		map[string]any{"employeeId": "EMP-1020", "code": "00001201"}, // by card number: the EmpCode is stored
		map[string]any{"employeeId": "EMP-1009", "code": "1209"},
		map[string]any{"employeeId": "EMP-1011", "code": "1209"},  // given to EMP-1009 above
		map[string]any{"employeeId": "EMP-1014", "code": "1211"},  // already has a code
		map[string]any{"employeeId": "EMP-1012", "code": "55555"}, // not a portal code
		map[string]any{"employeeId": "EMP-9999", "code": "1211"},  // nobody
	}
	s, ap := c.do("POST", "/api/attendance/realtime/match/apply", "EMP-1002", "HR Admin", map[string]any{"pairs": pairs})
	if s != 200 || ap["applied"] != 2.0 || ap["failed"] != 4.0 {
		t.Fatalf("apply: %d %v", s, ap)
	}
	res := ap["results"].([]any)
	errOf := func(i int) string { return fmt.Sprint(res[i].(map[string]any)["error"]) }
	if res[0].(map[string]any)["code"] != "1201" || !strings.Contains(errOf(2), "two employees") || !strings.Contains(errOf(3), "Already has") ||
		!strings.Contains(errOf(4), "not in the portal") || !strings.Contains(errOf(5), "No such employee") {
		t.Fatalf("results: %v", res)
	}
	if apiDoc(st, "employee", bson.M{"_id": "EMP-1020"})["attendance_code"] != "1201" || apiDoc(st, "employee", bson.M{"_id": "EMP-1009"})["attendance_code"] != "1209" ||
		apiDoc(st, "employee", bson.M{"_id": "EMP-1011"})["attendance_code"] != nil {
		t.Fatal("stored codes")
	}
	if a := apiDoc(st, "audit_log", bson.M{"entity": "employee", "entity_id": "EMP-1020", "action": "Attendance code 1201 given from the Realtime portal match"}); a == nil {
		t.Fatal("apply not audited")
	}
	// Applied people drop out of the proposals.
	if _, g := c.do("GET", "/api/attendance/realtime/match?level=All", "EMP-1002", "HR Admin", nil); g["counts"].(map[string]any)["Sure"] != 0.0 {
		t.Fatalf("after apply: %v", g["counts"])
	}
	// A code given since through Bulk update is no longer proposed.
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1012"}, bson.M{"$set": bson.M{"attendance_code": "1211"}}); err != nil {
		t.Fatal(err)
	}
	if _, g := c.do("GET", "/api/attendance/realtime/match?level=Likely", "EMP-1002", "HR Admin", nil); g["total"] != 0.0 {
		t.Fatalf("stale code still proposed: %v", g)
	}

	// Without the account nothing starts.
	undo2 := api.SetRealtimeEmployees(realtime.Config{}, false, nil)
	if s, _ := c.do("POST", "/api/attendance/realtime/match", "EMP-1002", "HR Admin", nil); s != 400 {
		t.Fatalf("no account: %d", s)
	}
	undo2()
	_ = app
}
