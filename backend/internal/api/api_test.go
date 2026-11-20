package api_test

// Integration tests against a real MongoDB replica set. Set HRMS_TEST_MONGO_URL, e.g.
// mongodb://localhost:27017/?replicaSet=rs0&directConnection=true, and optionally HRMS_TEST_MONGO_DB (default
// hrms_test_go), a database that may be wiped. Skipped otherwise.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/api"
	"github.com/saltstayz/hrms/internal/jobs"
	"github.com/saltstayz/hrms/internal/seed"
	"github.com/saltstayz/hrms/internal/storage"
	"github.com/saltstayz/hrms/internal/store"
)

type client struct {
	t      *testing.T
	base   string
	tokens map[string]string
	st     *store.Store // for seeding records directly, as some tests do (the payroll checklist helper)
}

func (c *client) do(method, path, emp, role string, body any) (int, map[string]any) {
	c.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, c.base+path, &buf)
	if emp != "" {
		req.Header.Set("Authorization", "Bearer "+c.tokens[emp])
	}
	req.Header.Set("X-Role", role)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	var out any
	_ = json.NewDecoder(res.Body).Decode(&out)
	m, _ := out.(map[string]any)
	if m == nil {
		m = map[string]any{"list": out}
	}
	return res.StatusCode, m
}

// signIn activates an account with the dev OTP and sets a password.
func (c *client) signIn(emp string) {
	c.t.Helper()
	_, o := c.do("POST", "/api/auth/otp", "", "", map[string]string{"employeeId": emp, "purpose": "first"})
	otp, _ := o["devOtp"].(string)
	if otp == "" {
		c.t.Fatalf("no OTP for %s: %v", emp, o)
	}
	s, v := c.do("POST", "/api/auth/otp/verify", "", "", map[string]string{"employeeId": emp, "code": otp})
	if s != 200 {
		c.t.Fatalf("verify: %v", v)
	}
	vt, _ := v["verifyToken"].(string)
	s, b := c.do("POST", "/api/auth/password", "", "", map[string]string{"employeeId": emp, "password": "Saltstayz#2026", "verifyToken": vt})
	if s != 200 {
		c.t.Fatalf("password: %v", b)
	}
	c.tokens[emp] = b["token"].(string)
}

func setup(t *testing.T) *client {
	c, _, _ := setupFull(t)
	return c
}

// The test database is rebuilt once per test process (dropped, then every collection and index created); each
// setupFull then only empties the collections (indexes stay), puts back the migrations' reference data and seeds.
var (
	testDBOnce sync.Once
	testDBErr  error
)

// testMongo is the test database's URL and name: HRMS_TEST_MONGO_URL (tests skip when unset), HRMS_TEST_MONGO_DB
// (default hrms_test_go; give each concurrent test run its own).
func testMongo(t *testing.T) (url, name string) {
	url = os.Getenv("HRMS_TEST_MONGO_URL")
	if url == "" {
		t.Skip("HRMS_TEST_MONGO_URL not set")
	}
	name = os.Getenv("HRMS_TEST_MONGO_DB")
	if name == "" {
		name = "hrms_test_go"
	}
	return url, name
}

func setupFull(t *testing.T) (*client, *api.Server, *store.Store) {
	url, name := testMongo(t)
	ctx := context.Background()
	st, err := store.Open(ctx, url, name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	testDBOnce.Do(func() {
		if testDBErr = st.DB.Database().Drop(ctx); testDBErr == nil {
			testDBErr = st.Migrate(ctx, func(string) {})
		}
	})
	if testDBErr != nil {
		t.Fatal(testDBErr)
	}
	if err := st.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if err := seed.Reference(ctx, st); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.SampleOrg(ctx, st); err != nil {
		t.Fatal(err)
	}
	app := api.New(st, api.Options{DevOTP: true, Files: &storage.Memory{}})
	srv := httptest.NewServer(app.Routes())
	t.Cleanup(srv.Close)
	return &client{t: t, base: srv.URL, tokens: map[string]string{}, st: st}, app, st
}

// apiDoc is the first document of coll matching filter, or nil: the tests' direct look at stored data.
func apiDoc(st *store.Store, coll string, filter any, opts ...options.Lister[options.FindOneOptions]) store.Row {
	r, _ := st.DB.C(coll).FindOne(context.Background(), filter, opts...)
	return r
}

// apiCount counts the documents of coll matching filter.
func apiCount(st *store.Store, coll string, filter any) int {
	n, _ := st.DB.C(coll).Count(context.Background(), filter)
	return int(n)
}

// apiFloat reads a stored number (or a number in text, as a JSON payload may hold it) as float64.
func apiFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}

// The maintenance sweep runs on the job queue: a job change due today must be applied.
func TestMaintenanceJob(t *testing.T) {
	_, app, st := setupFull(t)
	ctx := context.Background()
	if _, err := st.DB.C("employee_change").Insert(ctx, bson.M{"employee_id": "EMP-1014", "field": "Shift", "old_value": "G", "new_value": "M", "reason": "Transfer",
		"effective_from": store.Today(), "state": "Scheduled", "apply_column": "shift_code", "apply_value": "M", "changed_by": "test"}); err != nil {
		t.Fatal(err)
	}
	jctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := app.StartJobs(jctx); err != nil { // periodic sweep runs on start
		t.Fatal(err)
	}
	defer app.StopJobs(ctx)
	deadline := time.Now().Add(15 * time.Second)
	for {
		var state, shift string
		if ch := apiDoc(st, "employee_change", bson.M{"changed_by": "test"}); ch != nil {
			if e := apiDoc(st, "employee", bson.M{"_id": ch["employee_id"]}); e != nil {
				state, _ = ch["state"].(string)
				shift, _ = e["shift_code"].(string)
			}
		}
		if state == "Applied" && shift == "M" {
			break
		}
		if time.Now().After(deadline) {
			n, _ := st.DB.Database().Collection(jobs.CollectionJobs).CountDocuments(ctx, bson.M{"kind": "maintenance"})
			t.Fatalf("scheduled change not applied (state %q, shift %q, %d maintenance jobs)", state, shift, n)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Sample org (seed.SampleOrg): EMP-1002 HR Admin, EMP-1004 HRBP (north), EMP-1008 Property Manager
// (Sector 29 Hub), EMP-1009 Line Manager there, EMP-1014+ staff. EMP-1014 reports to EMP-1009.
func TestEndToEnd(t *testing.T) {
	c := setup(t)
	for _, e := range []string{"EMP-1002", "EMP-1004", "EMP-1008", "EMP-1009", "EMP-1014", "EMP-1020"} {
		c.signIn(e)
	}

	t.Run("sign-in rules", func(t *testing.T) {
		if s, _ := c.do("POST", "/api/auth/login", "", "", map[string]string{"employeeId": "1009", "password": "Saltstayz#2026"}); s != 200 {
			t.Fatal("numeric ID should normalise to EMP-1009")
		}
		if s, _ := c.do("POST", "/api/auth/login", "", "", map[string]string{"employeeId": "EMP-1009", "password": "wrong"}); s != 401 {
			t.Fatal("wrong password must fail")
		}
		if s, _ := c.do("GET", "/api/me", "", "", nil); s != 401 {
			t.Fatal("no token must be 401")
		}
	})

	t.Run("scope", func(t *testing.T) {
		_, me := c.do("GET", "/api/me", "EMP-1009", "Line Manager", nil)
		if me["ctx"].(map[string]any)["role"] != "Line Manager" {
			t.Fatal("line manager view derived from reports")
		}
		if s, _ := c.do("GET", "/api/employees", "EMP-1014", "Employee", nil); s != 403 {
			t.Fatal("employee view has no directory")
		}
		if s, _ := c.do("GET", "/api/employees/EMP-1014", "EMP-1014", "Employee", nil); s != 200 {
			t.Fatal("own record is always visible")
		}
		if s, _ := c.do("GET", "/api/employees/EMP-1002", "EMP-1009", "Line Manager", nil); s != 403 {
			t.Fatal("line manager sees only the team")
		}
		_, e := c.do("GET", "/api/employees/EMP-1014", "EMP-1004", "HRBP", nil)
		if acct, _ := e["bank"].(map[string]any)["account_no"].(string); len(acct) == 0 || acct[0] != 0xE2 {
			t.Fatalf("HRBP must see the bank account masked, got %v", e["bank"])
		}
	})

	var leaveID string
	// Leave dates follow today: Tuesday and Wednesday of next week, and the Thursday of the week after, so they are
	// never past the back-dating limit and stay inside one 30-day rolling window.
	tue := time.Now()
	for tue = tue.AddDate(0, 0, 1); tue.Weekday() != time.Monday; tue = tue.AddDate(0, 0, 1) {
	}
	tue = tue.AddDate(0, 0, 1)
	l1, l2, l3 := tue.Format("2006-01-02"), tue.AddDate(0, 0, 1).Format("2006-01-02"), tue.AddDate(0, 0, 9).Format("2006-01-02")
	t.Run("leave chain in column order; own request blocked", func(t *testing.T) {
		// EMP-1020 is permanent (EMP-1014 is contractual, and contract staff have no leave in their plan).
		if s, _ := c.do("POST", "/api/approvals", "EMP-1014", "Employee", map[string]any{"wf": "leave", "payload": map[string]string{"ltype": "Casual leave", "from": l1, "to": l2}}); s != 400 {
			t.Fatal("contractual staff have no casual leave")
		}
		s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "leave", "payload": map[string]string{"ltype": "Casual leave", "from": l1, "to": l2}})
		if s != 200 {
			t.Fatal(r)
		}
		leaveID = r["id"].(string)
		chain := r["plan"].(map[string]any)["chain"].([]any)
		if len(chain) != 2 || chain[0] != "Line Manager" || chain[1] != "Property Manager" {
			t.Fatalf("chain %v", chain)
		}
		if s, _ := c.do("POST", "/api/approvals/"+leaveID+"/approve", "EMP-1020", "Employee", nil); s != 403 {
			t.Fatal("nobody approves their own request")
		}
		if s, _ := c.do("POST", "/api/approvals/"+leaveID+"/approve", "EMP-1008", "Property Manager", nil); s != 403 {
			t.Fatal("PM cannot act before LM")
		}
		if _, r := c.do("POST", "/api/approvals/"+leaveID+"/approve", "EMP-1009", "Line Manager", nil); r["next"] != "Property Manager" {
			t.Fatal(r)
		}
		if s, _ := c.do("POST", "/api/approvals/"+leaveID+"/reject", "EMP-1008", "Property Manager", map[string]string{}); s != 400 {
			t.Fatal("reject needs a comment")
		}
		if _, r := c.do("POST", "/api/approvals/"+leaveID+"/approve", "EMP-1008", "Property Manager", nil); r["status"] != "Approved" {
			t.Fatal(r)
		}
	})

	t.Run("claim lands in the ledger", func(t *testing.T) {
		if s, _ := c.do("POST", "/api/approvals", "EMP-1014", "Employee", map[string]any{"wf": "p_claim", "payload": map[string]any{"amount": 3000, "claimType": "Travel", "billDate": today()}}); s != 400 {
			t.Fatal("a claim needs the bill")
		}
		_, doc := c.upload("EMP-1014", "Employee", "employee", "EMP-1014", "Bill")
		_, r := c.do("POST", "/api/approvals", "EMP-1014", "Employee", map[string]any{"wf": "p_claim", "payload": map[string]any{"amount": 3000, "claimType": "Travel", "billDate": today(), "doc": doc["id"]}})
		id, _ := r["id"].(string)
		if id == "" {
			t.Fatal(r)
		}
		if s, _ := c.do("POST", "/api/approvals/"+id+"/approve", "EMP-1002", "HR Admin", map[string]string{}); s != 400 {
			t.Fatal("HR Admin override needs a note")
		}
		for i := 0; i < 2; i++ {
			c.do("POST", "/api/approvals/"+id+"/approve", "EMP-1002", "HR Admin", map[string]string{"note": "test"})
		}
		_, d := c.do("GET", "/api/approvals/"+id, "EMP-1014", "Employee", nil)
		if d["status"] != "Approved" {
			t.Fatal(d["status"])
		}
	})

	t.Run("departed approver is skipped", func(t *testing.T) {
		if s, r := c.do("POST", "/api/employees/EMP-1008/inactive", "EMP-1002", "HR Admin", map[string]string{"lwd": "2026-10-01", "reason": "Resigned"}); s != 200 {
			t.Fatal(r)
		}
		_, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "leave", "payload": map[string]string{"ltype": "Casual leave", "from": l3, "to": l3}})
		id, _ := r["id"].(string)
		if id == "" {
			t.Fatal(r)
		}
		_, a := c.do("POST", "/api/approvals/"+id+"/approve", "EMP-1009", "Line Manager", nil)
		// With the 2 approved days the week before, the rolling total is 3 days, so the chain is
		// LM, PM, HRBP. The Property Manager has left, so the request moves straight to the HRBP.
		if a["status"] != "Pending" || a["next"] != "HRBP" {
			t.Fatalf("got %v", a)
		}
	})

	t.Run("one holder per place", func(t *testing.T) {
		_, l := c.do("GET", "/api/masters/location?status=Active", "EMP-1002", "HR Admin", nil)
		var hub string
		for _, x := range l["list"].([]any) {
			if m := x.(map[string]any); m["name"] == "Gurugram – Sector 29 Hub" {
				hub = m["id"].(string)
			}
		}
		if s, r := c.do("POST", "/api/role-map", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1006", "role": "HRBP", "access": "locs", "locationIds": []string{hub}}); s != 400 {
			t.Fatalf("second HRBP for the same place must be refused: %v", r)
		}
		if s, _ := c.do("POST", "/api/role-map", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1006", "role": "Line Manager", "access": "all"}); s != 400 {
			t.Fatal("Line Manager is not assignable")
		}
	})

	t.Run("salary preview matches the golden value", func(t *testing.T) {
		_, r := c.do("POST", "/api/salary/preview", "EMP-1002", "HR Admin", map[string]any{"type": "Permanent", "gross": 30000})
		if net := r["breakup"].(map[string]any)["net"].(float64); net != 28200 { // no location: no LWF (Mumbai golden row)
			t.Fatalf("net %v", net)
		}
	})
}

// Approval extras: transfers, inbox counts, global response time, escalate now, and
// moving the reports of a manager who has left.
func TestApprovalExtras(t *testing.T) {
	c, _, st := setupFull(t)
	for _, e := range []string{"EMP-1002", "EMP-1009", "EMP-1014", "EMP-1020"} {
		c.signIn(e)
	}
	golf, _ := apiDoc(st, "location", bson.M{"name": "Gurugram – Golf Course Rd"})["id"].(string)
	eff := time.Now().AddDate(0, 1, 0).Format("2006-01-02")

	t.Run("transfer", func(t *testing.T) {
		tr := map[string]any{"wf": "transfer", "employeeId": "EMP-1014", "payload": map[string]string{"toLocation": golf, "effective": eff}}
		if s, _ := c.do("POST", "/api/approvals", "EMP-1014", "Employee", tr); s != 403 {
			t.Fatal("an employee cannot raise a transfer")
		}
		past := map[string]any{"wf": "transfer", "employeeId": "EMP-1014", "payload": map[string]string{"toLocation": golf, "effective": "2020-01-01"}}
		if s, _ := c.do("POST", "/api/approvals", "EMP-1002", "HR Admin", past); s != 400 {
			t.Fatal("effective date must be in the future")
		}
		s, r := c.do("POST", "/api/approvals", "EMP-1009", "Line Manager", tr)
		if s != 200 {
			t.Fatal(r)
		}
		id := r["id"].(string)
		if s, _ := c.do("POST", "/api/approvals", "EMP-1009", "Line Manager", tr); s != 400 {
			t.Fatal("a second pending transfer must be refused")
		}
		for i := 0; i < 12; i++ {
			_, a := c.do("POST", "/api/approvals/"+id+"/approve", "EMP-1002", "HR Admin", map[string]string{"note": "ok"})
			if a["status"] == "Approved" {
				break
			}
		}
		state, _ := apiDoc(st, "employee_change", bson.M{"employee_id": "EMP-1014", "field": "Location", "reason": "Transfer"})["state"].(string)
		facts, _ := apiDoc(st, "approval_request", bson.M{"_id": id})["facts"].(map[string]any)
		tkind, _ := facts["tkind"].(string)
		if state != "Scheduled" || tkind != "Within city" {
			t.Fatalf("state %q tkind %q", state, tkind)
		}
	})

	t.Run("counts and response time", func(t *testing.T) {
		if _, r := c.do("GET", "/api/approvals/counts", "EMP-1009", "Line Manager", nil); r["mine"] == nil || r["done"] == nil {
			t.Fatal(r)
		}
		if s, _ := c.do("PUT", "/api/approvals/settings", "EMP-1009", "Line Manager", map[string]int{"slaHours": 48}); s != 403 {
			t.Fatal("only approval setup can change the response time")
		}
		if s, _ := c.do("PUT", "/api/approvals/settings", "EMP-1002", "HR Admin", map[string]int{"slaHours": 0}); s != 400 {
			t.Fatal("hours must be at least 1")
		}
		c.do("PUT", "/api/approvals/settings", "EMP-1002", "HR Admin", map[string]int{"slaHours": 48})
		if _, r := c.do("GET", "/api/approvals/settings", "EMP-1002", "HR Admin", nil); r["slaHours"] != float64(48) {
			t.Fatal(r)
		}
	})

	t.Run("escalate now", func(t *testing.T) {
		_, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "leave", "payload": map[string]string{"ltype": "Casual leave", "from": "2026-12-02", "to": "2026-12-02"}})
		id, _ := r["id"].(string)
		if id == "" {
			t.Fatal(r)
		}
		if s, _ := c.do("POST", "/api/approvals/"+id+"/escalate", "EMP-1009", "Line Manager", nil); s != 403 {
			t.Fatal("only HR Admin simulates no response")
		}
		if s, r := c.do("POST", "/api/approvals/"+id+"/escalate", "EMP-1002", "HR Admin", nil); s != 200 || r["result"] == nil {
			t.Fatal(r)
		}
	})

	t.Run("manager left", func(t *testing.T) {
		c.do("POST", "/api/employees/EMP-1009/inactive", "EMP-1002", "HR Admin", map[string]string{"lwd": "2026-10-01", "reason": "Resigned"})
		_, l := c.do("GET", "/api/managers/left", "EMP-1002", "HR Admin", nil)
		groups, _ := l["list"].([]any)
		if len(groups) == 0 || groups[0].(map[string]any)["id"] != "EMP-1009" {
			t.Fatalf("got %v", l)
		}
		if _, r := c.do("POST", "/api/managers/reassign", "EMP-1002", "HR Admin", map[string]string{"from": "EMP-1009", "to": "EMP-1008"}); r["moved"].(float64) < 1 {
			t.Fatal(r)
		}
		mgr, _ := apiDoc(st, "employee", bson.M{"_id": "EMP-1014"})["manager_id"].(string)
		if mgr != "EMP-1008" {
			t.Fatal(mgr)
		}
	})
}

// Employee, access and dashboard extras.
func TestPeopleAndAccessExtras(t *testing.T) {
	c, _, _ := setupFull(t)
	for _, e := range []string{"EMP-1002", "EMP-1004", "EMP-1009"} {
		c.signIn(e)
	}

	t.Run("employee code series only moves up", func(t *testing.T) {
		_, g := c.do("GET", "/api/employee-codes", "EMP-1002", "HR Admin", nil)
		series := g["series"].([]any)
		all := series[0].(map[string]any)
		if all["key"] != "All" || all["max"].(float64) < 1014 {
			t.Fatalf("got %v", all)
		}
		body := map[string]any{"byType": false, "rejoin": "same", "series": []map[string]any{{"key": "All", "prefix": "EMP-", "pad": 4, "next": 5}}}
		if s, _ := c.do("PUT", "/api/employee-codes", "EMP-1002", "HR Admin", body); s != 400 {
			t.Fatal("next number below the highest used must be refused")
		}
		body["series"] = []map[string]any{{"key": "All", "prefix": "EMP-", "pad": 4, "next": 5000}}
		if s, r := c.do("PUT", "/api/employee-codes", "EMP-1002", "HR Admin", body); s != 200 {
			t.Fatal(r)
		}
		if _, r := c.do("GET", "/api/employees/next-code", "EMP-1002", "HR Admin", nil); r["next"] != "EMP-5000" {
			t.Fatal(r)
		}
		if s, _ := c.do("PUT", "/api/employee-codes", "EMP-1004", "HRBP", body); s != 403 {
			t.Fatal("only HR Admin changes code rules")
		}
	})

	t.Run("pay options follow the employment type", func(t *testing.T) {
		if s, r := c.do("PUT", "/api/employees/EMP-1016/pay", "EMP-1002", "HR Admin", map[string]any{"varPct": 5, "varCycle": 3, "meal": 1500, "accommodation": 0, "reason": "Policy change"}); s != 200 {
			t.Fatal(r)
		}
		if s, _ := c.do("PUT", "/api/employees/EMP-1014/pay", "EMP-1002", "HR Admin", map[string]any{"meal": 500, "accommodation": 0, "reason": "Policy change"}); s != 400 {
			t.Fatal("contractual staff have no meal deduction")
		}
		if s, _ := c.do("PUT", "/api/employees/EMP-1016/pay", "EMP-1002", "HR Admin", map[string]any{"varCycle": 4, "meal": 0, "accommodation": 0, "reason": "Policy change"}); s != 400 {
			t.Fatal("payout cycle must be 1, 3, 6 or 12")
		}
		_, e := c.do("GET", "/api/employees/EMP-1016", "EMP-1002", "HR Admin", nil)
		pay := e["salary"].(map[string]any)["pay"].(map[string]any)
		if pay["varPct"] != float64(5) || pay["meal"] != float64(1500) {
			t.Fatal(pay)
		}
	})

	t.Run("export", func(t *testing.T) {
		_, o := c.do("GET", "/api/employees/export/options", "EMP-1004", "HRBP", nil)
		if secs := o["sections"].([]any); len(secs) != 1 {
			t.Fatalf("HRBP downloads job and location only, got %v", secs)
		}
		req, _ := http.NewRequest("GET", c.base+"/api/employees/export?sections=emp,bank,salary", nil)
		req.Header.Set("Authorization", "Bearer "+c.tokens["EMP-1002"])
		req.Header.Set("X-Role", "HR Admin")
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != 200 {
			t.Fatal(err, res.StatusCode)
		}
		defer res.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(res.Body)
		lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
		if len(lines) < 10 || !bytes.Contains(lines[0], []byte("Account number")) || !bytes.Contains(lines[0], []byte("CTC a month")) {
			t.Fatalf("%d lines, header %s", len(lines), lines[0])
		}
	})

	t.Run("roles", func(t *testing.T) {
		if s, r := c.do("POST", "/api/roles", "EMP-1002", "HR Admin", map[string]string{"name": "Night Auditor", "copyFrom": "Line Manager"}); s != 200 {
			t.Fatal(r)
		}
		if s, _ := c.do("POST", "/api/roles", "EMP-1002", "HR Admin", map[string]string{"name": "night auditor", "copyFrom": "Line Manager"}); s != 400 {
			t.Fatal("duplicate role names are refused")
		}
		if s, _ := c.do("PATCH", "/api/roles/HRBP", "EMP-1002", "HR Admin", map[string]bool{"active": false}); s != 400 {
			t.Fatal("built-in roles stay on")
		}
		if s, _ := c.do("PATCH", "/api/roles/Night Auditor", "EMP-1002", "HR Admin", map[string]bool{"active": false}); s != 200 {
			t.Fatal("custom roles can be switched off")
		}
		_, p := c.do("GET", "/api/access/person/EMP-1004?role=HRBP", "EMP-1002", "HR Admin", nil)
		if p["role"] != "HRBP" || p["visible"].(float64) < 1 || len(p["approves"].([]any)) == 0 {
			t.Fatal(p)
		}
	})

	t.Run("dashboard tabs", func(t *testing.T) {
		if _, r := c.do("GET", "/api/dashboard", "EMP-1002", "HR Admin", nil); r["avgTenure"] == nil || r["alerts"] == nil {
			t.Fatal(r)
		}
		if _, r := c.do("GET", "/api/dashboard?tab=wf", "EMP-1002", "HR Admin", nil); len(r["trend"].([]any)) != 6 {
			t.Fatal(r)
		}
		if _, r := c.do("GET", "/api/dashboard?tab=ap", "EMP-1002", "HR Admin", nil); r["rate"] == nil {
			t.Fatal(r)
		}
		if _, r := c.do("GET", "/api/dashboard?tab=cmp", "EMP-1002", "HR Admin", nil); r["pan"] == nil {
			t.Fatal(r)
		}
	})
}

// upload sends one small PDF as a document of an owner.
func (c *client) upload(emp, role, kind, owner, key string) (int, map[string]any) {
	c.t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("ownerKind", kind)
	_ = w.WriteField("ownerId", owner)
	_ = w.WriteField("docKey", key)
	f, _ := w.CreateFormFile("file", key+".pdf")
	_, _ = f.Write([]byte("%PDF-1.4\n1 0 obj << >> endobj\ntrailer << >>\n%%EOF\n"))
	_ = w.Close()
	req, _ := http.NewRequest("POST", c.base+"/api/files", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.tokens[emp])
	req.Header.Set("X-Role", role)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

// A selected candidate's offer → approval (no Employee ID yet) → release → accept → joined → joining details and
// documents → Employee ID → Payroll → active. The stages before the offer are in onboarding_stages_test.go.
func TestOnboarding(t *testing.T) {
	c, _, st := setupFull(t)
	for _, e := range []string{"EMP-1002", "EMP-1004", "EMP-1009", "EMP-1011", "EMP-1012", "EMP-1014"} {
		c.signIn(e)
	}
	data := offerData(st, "Riya", "Sharma", "9876501234")
	si := selectCandidate(t, c, shortlistBody(st, "Riya", "Sharma", "9876501234"))

	if s, _ := c.do("POST", "/api/offers/check", "EMP-1014", "Employee", map[string]any{"data": data, "candidate": si}); s != 403 {
		t.Fatal("only onboarding roles raise offers")
	}
	_, chk := c.do("POST", "/api/offers/check", "EMP-1004", "HRBP", map[string]any{"data": data, "candidate": si})
	if errs := chk["errors"].([]any); len(errs) != 1 || errs[0] != "Aadhaar card must be uploaded before the offer is released" {
		t.Fatalf("errors %v", errs)
	}
	if chk["breakup"].(map[string]any)["gross"] != float64(26000) || chk["plan"] == nil {
		t.Fatal(chk)
	}
	if s, r := c.upload("EMP-1004", "HRBP", "candidate", si, "aadhaar"); s != 200 {
		t.Fatal(r)
	}
	s, r := c.do("POST", "/api/offers", "EMP-1004", "HRBP", map[string]any{"data": data, "candidate": si})
	if s != 200 {
		t.Fatal(r)
	}
	offer := r["id"].(string)
	reqID := r["request"].(map[string]any)["id"].(string)
	chain := r["request"].(map[string]any)["plan"].(map[string]any)["chain"].([]any)
	if containsAny(chain, "HRBP") {
		t.Fatalf("the HRBP raised it, so the HRBP column is skipped: %v", chain)
	}
	for i := 0; i < 6; i++ {
		if _, a := c.do("POST", "/api/approvals/"+reqID+"/approve", "EMP-1002", "HR Admin", map[string]string{"note": "ok"}); a["status"] == "Approved" {
			break
		}
	}
	_, o := c.do("GET", "/api/offers/"+offer, "EMP-1004", "HRBP", nil)
	if o["status"] != "Approved" || o["employee_id"] != nil {
		t.Fatalf("status %v employee %v", o["status"], o["employee_id"])
	}
	if s, _ := c.do("POST", "/api/offers/"+offer+"/accept", "EMP-1004", "HRBP", nil); s != 400 {
		t.Fatal("release comes before accept")
	}
	c.do("POST", "/api/offers/"+offer+"/release", "EMP-1004", "HRBP", nil)
	c.do("POST", "/api/offers/"+offer+"/accept", "EMP-1004", "HRBP", nil)
	if s, r := c.do("POST", "/api/candidates/"+si+"/joined", "EMP-1004", "HRBP", map[string]any{"date": today()}); s != 200 {
		t.Fatal(r)
	}
	if s, _ := c.do("POST", "/api/offers/"+offer+"/send", "EMP-1004", "HRBP", nil); s != 400 {
		t.Fatal("mandatory joining details come first")
	}
	if s, _ := c.do("POST", "/api/offers/"+offer+"/joining", "EMP-1004", "HRBP", map[string]any{"data": map[string]any{"aadhaar": "123"}}); s != 400 {
		t.Fatal("aadhaar format is checked")
	}
	join := map[string]any{"gender": "Female", "dob": "1998-05-04", "aadhaar": "123412341234", "pan": "abcde1234f", "uanAvail": "No", "pfChoice": "Yes",
		"bankName": "HDFC Bank", "acct": "50100123456789", "ifsc": "HDFC0001234", "shift": "G", "attMode": "Biometric", "l1": "EMP-1009"}
	if s, r := c.do("POST", "/api/offers/"+offer+"/joining", "EMP-1004", "HRBP", map[string]any{"data": join}); s != 200 {
		t.Fatal(r)
	}
	for _, k := range []string{"pan", "photo", "bankProof", "police"} {
		if s, r := c.upload("EMP-1004", "HRBP", "candidate", si, k); s != 200 {
			t.Fatal(r)
		}
	}
	_, x := c.do("GET", "/api/candidates/"+si, "EMP-1004", "HRBP", nil)
	emp, _ := x["employee_id"].(string)
	// A normal employee on the Joining ID from the joining day (hiring.go).
	status, _ := apiDoc(st, "employee", bson.M{"_id": emp})["status"].(string)
	if status != "Active" || emp != "JI-0001" {
		t.Fatalf("%s: %s", emp, status)
	}
	s, v := c.do("POST", "/api/offers/"+offer+"/send", "EMP-1004", "HRBP", nil)
	if s != 200 {
		t.Fatal(v)
	}
	vid := v["id"].(string)
	if _, a := c.do("POST", "/api/approvals/"+vid+"/approve", "EMP-1011", "Payroll", nil); a["next"] != "Payroll Manager" {
		t.Fatal(a)
	}
	if _, a := c.do("POST", "/api/approvals/"+vid+"/approve", "EMP-1012", "Payroll Manager", nil); a["status"] != "Approved" {
		t.Fatal(a)
	}
	var verified bool
	var pan, mgr string
	if e, g := apiDoc(st, "employee", bson.M{"_id": emp}), apiDoc(st, "employee_gov_id", bson.M{"employee_id": emp}); e != nil && g != nil {
		status, _ = e["status"].(string)
		verified, _ = e["verified"].(bool)
		pan, _ = g["pan"].(string)
		mgr, _ = e["manager_id"].(string)
	}
	if status != "Active" || !verified || pan != "ABCDE1234F" || mgr != "EMP-1009" {
		t.Fatalf("status %s verified %v pan %s manager %s", status, verified, pan, mgr)
	}
	_, o = c.do("GET", "/api/offers/"+offer, "EMP-1004", "HRBP", nil)
	if o["status"] != "Verified" || !strings.Contains(o["letter"].(string), "Riya Sharma") {
		t.Fatal(o["status"])
	}
	// The new joiner can activate sign-in with an OTP to the mobile from the offer.
	if s, r := c.do("POST", "/api/auth/otp", "", "", map[string]string{"employeeId": emp, "purpose": "first"}); s != 200 {
		t.Fatal(r)
	}
}

func containsAny(l []any, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

func today() string { return time.Now().Format("2006-01-02") }

// sendFile posts a file to a multipart endpoint.
func (c *client) sendFile(path, emp, role, name string, content []byte) (int, map[string]any) {
	c.t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	f, _ := w.CreateFormFile("file", name)
	_, _ = f.Write(content)
	_ = w.Close()
	req, _ := http.NewRequest("POST", c.base+path, &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.tokens[emp])
	req.Header.Set("X-Role", role)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestBulkUpdate(t *testing.T) {
	c, _, st := setupFull(t)
	c.signIn("EMP-1002")
	c.signIn("EMP-1014")

	req, _ := http.NewRequest("GET", c.base+"/api/bulk/upd/download", nil)
	req.Header.Set("Authorization", "Bearer "+c.tokens["EMP-1002"])
	req.Header.Set("X-Role", "HR Admin")
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatal(err, res.StatusCode)
	}
	var xb bytes.Buffer
	_, _ = xb.ReadFrom(res.Body)
	res.Body.Close()
	if !bytes.HasPrefix(xb.Bytes(), []byte("PK")) {
		t.Fatal("template must be an xlsx file")
	}

	// With no payroll run yet the open payroll month is last month, so three months ago is two paid months back.
	lastMonth := time.Now().AddDate(0, -3, 0).Format("2006-01") + "-01"
	// The prototype's TPL.upd has a Work Policy column (round 3); every template column is required.
	upd := "Employee ID,Work City,Reporting Location,Department,Designation,Grade,Shift,Reporting Manager L1,Functional Manager L2,Work Policy,Attendance Mode,Reason for change,Effective From\n" +
		"EMP-1016,,,,,,,EMP-1009,,,,Reporting line change," + today() + "\n" +
		"EMP-1017,,,,,G9,,,,,,Promotion," + today() + "\n" +
		"EMP-9999,,,,,,G,,,,,Transfer," + today() + "\n"
	_, chk := c.sendFile("/api/bulk/upd/check", "EMP-1002", "HR Admin", "upd.csv", []byte(upd))
	rs := chk["results"].([]any)
	if len(rs) != 3 || chk["ok"].(float64) != 1 {
		t.Fatalf("check %v", chk)
	}
	if e := rs[1].(map[string]any)["errors"].([]any); len(e) == 0 {
		t.Fatal("an unknown grade must be refused")
	}
	mgr, _ := apiDoc(st, "employee", bson.M{"_id": "EMP-1016"})["manager_id"].(string)
	if mgr == "EMP-1009" {
		t.Fatal("a check must not change anything")
	}
	if _, ap := c.sendFile("/api/bulk/upd/apply", "EMP-1002", "HR Admin", "upd.csv", []byte(upd)); ap["ok"].(float64) != 1 {
		t.Fatal(ap)
	}
	mgr, _ = apiDoc(st, "employee", bson.M{"_id": "EMP-1016"})["manager_id"].(string)
	if mgr != "EMP-1009" {
		t.Fatal(mgr)
	}

	before := apiFloat(apiDoc(st, "gross_history", bson.M{"employee_id": "EMP-1016"}, options.FindOne().SetSort(bson.D{{Key: "from_month", Value: -1}}))["gross"])
	gross := fmt.Sprintf("Employee ID,New Gross,Effective From\nEMP-1016,%v,%s\n", before+1000, lastMonth)
	if _, ap := c.sendFile("/api/bulk/gross/apply", "EMP-1002", "HR Admin", "gross.csv", []byte(gross)); ap["ok"].(float64) != 1 {
		t.Fatal(ap)
	}
	payload, _ := apiDoc(st, "approval_request", bson.M{"wf": "p_arrear", "employee_id": "EMP-1016"})["payload"].(map[string]any)
	arrear := apiFloat(payload["amount"])
	if arrear != 2000 {
		t.Fatalf("two months back at ₹1,000 more is a ₹2,000 arrear, got %v", arrear)
	}

	band := "City,Grade,Minimum Gross,Maximum Gross\nGurugram,G1,15000,9000\nGurugram,G1,15000,22000\n"
	_, b := c.sendFile("/api/bulk/band/apply", "EMP-1002", "HR Admin", "band.csv", []byte(band))
	if b["ok"].(float64) != 0 {
		t.Fatal("a maximum below the minimum is refused, and the repeat row is flagged", b)
	}
	if s, _ := c.sendFile("/api/bulk/band/check", "EMP-1014", "Employee", "band.csv", []byte(band)); s != 403 {
		t.Fatal("employees cannot bulk update")
	}
}

func TestNominations(t *testing.T) {
	c, _, _ := setupFull(t)
	for _, e := range []string{"EMP-1011", "EMP-1016"} {
		c.signIn(e)
	}
	_, mine := c.do("GET", "/api/me/nominations", "EMP-1016", "Employee", nil)
	list := mine["list"].([]any)
	if len(list) != 4 {
		t.Fatalf("a permanent employee has 4 nomination schemes, got %d", len(list))
	}
	pf := list[0].(map[string]any)["scheme"].(map[string]any)
	id := fmt.Sprint(pf["id"])
	people := []map[string]any{{"name": "Asha Verma", "rel": "Spouse", "dob": "1995-02-01", "share": 60}, {"name": "Kabir Verma", "rel": "Son", "dob": "2020-03-01", "share": 30}}
	s, r := c.do("POST", "/api/me/nominations/"+id, "EMP-1016", "Employee", map[string]any{"people": people})
	if s != 400 {
		t.Fatal("shares must total 100, minors need a guardian, documents are required")
	}
	details := fmt.Sprint(r["details"])
	for _, want := range []string{"add up to 100", "under 18", "Nominee Aadhaar copy", "Signed nomination form"} {
		if !strings.Contains(details, want) {
			t.Fatalf("missing %q in %s", want, details)
		}
	}
	people[0]["share"], people[1]["guardian"] = 70, "Asha Verma"
	owner := "EMP-1016/" + id
	for _, k := range []string{"n0:aadhaar", "n1:aadhaar", "e:form"} {
		if s, r := c.upload("EMP-1016", "Employee", "nomination", owner, k); s != 200 {
			t.Fatal(r)
		}
	}
	if s, r := c.do("POST", "/api/me/nominations/"+id, "EMP-1016", "Employee", map[string]any{"people": people}); s != 200 {
		t.Fatal(r)
	}
	if s, _ := c.do("POST", "/api/me/nominations/"+id, "EMP-1016", "Employee", map[string]any{"people": people}); s != 400 {
		t.Fatal("one submission waits for Payroll at a time")
	}
	_, q := c.do("GET", "/api/nominations", "EMP-1011", "Payroll", nil)
	subs := q["submitted"].([]any)
	if len(subs) != 1 {
		t.Fatal(q)
	}
	nid := fmt.Sprint(subs[0].(map[string]any)["id"])
	if s, _ := c.do("POST", "/api/nominations/"+nid+"/return", "EMP-1011", "Payroll", map[string]string{}); s != 400 {
		t.Fatal("returning needs a note")
	}
	if s, r := c.do("POST", "/api/nominations/"+nid+"/verify", "EMP-1011", "Payroll", nil); s != 200 {
		t.Fatal(r)
	}
	_, mine = c.do("GET", "/api/me/nominations", "EMP-1016", "Employee", nil)
	if st := mine["list"].([]any)[0].(map[string]any)["state"]; st != "Verified" {
		t.Fatal(st)
	}
}

// Sprint 4: policies, roster, attendance, deviations, regularization, exceptions, leave, consequences.
// The sample org's EMP-1020 is permanent, F&B at Sector 29 Hub, shift M (06:00-14:00), reports to EMP-1009.
func TestTimeAndAttendance(t *testing.T) {
	c, _, st := setupFull(t)
	for _, e := range []string{"EMP-1002", "EMP-1004", "EMP-1009", "EMP-1012", "EMP-1020"} {
		c.signIn(e)
	}
	hub, _ := apiDoc(st, "location", bson.M{"name": "Gurugram – Sector 29 Hub"})["id"].(string)
	day := func(n int) string { return time.Now().AddDate(0, 0, n).Format("2006-01-02") }

	t.Run("policy resolution", func(t *testing.T) {
		_, r := c.do("GET", "/api/policies/coverage", "EMP-1002", "HR Admin", nil)
		got := map[string]string{}
		for _, x := range r["rows"].([]any) {
			m := x.(map[string]any)
			got[m["id"].(string)] = m["policy"].(string) + " via " + m["via"].(string)
		}
		for id, want := range map[string]string{"EMP-1016": "Operations – 24x7 rotational via Department", "EMP-1015": "Contract and retainer staff via Employment type",
			"EMP-1020": "Operations – 6 day via Default", "EMP-1010": "Corporate office – 5.5 day via Department"} {
			if got[id] != want {
				t.Errorf("%s: %s, want %s", id, got[id], want)
			}
		}
	})

	t.Run("roster rules", func(t *testing.T) {
		if s, _ := c.do("POST", "/api/roster/cell", "EMP-1009", "Line Manager", map[string]string{"employeeId": "EMP-1020", "date": day(1), "code": "E"}); s != 400 {
			t.Fatal("a change inside the lead days needs a reason")
		}
		if s, r := c.do("POST", "/api/roster/cell", "EMP-1009", "Line Manager", map[string]string{"employeeId": "EMP-1020", "date": day(1), "code": "E", "why": "Staff absent or sick"}); s != 200 || r["late"] != true {
			t.Fatal(r)
		}
		if s, _ := c.do("POST", "/api/roster/cell", "EMP-1009", "Line Manager", map[string]string{"employeeId": "EMP-1020", "date": day(-3), "code": "E", "why": "x"}); s != 400 {
			t.Fatal("past dates are locked")
		}
		// N the day before an M day leaves no rest. The next day must really be M on the roster (not a weekly off),
		// so pick the first such day from 9 days ahead: which weekday is off depends on today's date.
		cellOn := func(date string) string {
			_, g := c.do("GET", "/api/roster?location="+hub+"&department=F%26B&week="+date, "EMP-1009", "Line Manager", nil)
			for _, x := range g["rows"].([]any) {
				if row := x.(map[string]any); row["id"] == "EMP-1020" {
					for i, d := range g["days"].([]any) {
						if d == date {
							return fmt.Sprint(row["cells"].([]any)[i].(map[string]any)["code"])
						}
					}
				}
			}
			return ""
		}
		n := 9
		for n < 16 && cellOn(day(n+1)) != "M" {
			n++
		}
		if s, r := c.do("POST", "/api/roster/cell", "EMP-1009", "Line Manager", map[string]string{"employeeId": "EMP-1020", "date": day(n), "code": "N"}); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "rest") {
			t.Fatal("N before an M day leaves no rest", day(n), r)
		}
		_, g := c.do("GET", "/api/roster?location="+hub+"&department=F%26B&week="+day(1), "EMP-1009", "Line Manager", nil)
		found := false
		for _, x := range g["rows"].([]any) {
			row := x.(map[string]any)
			if row["id"] == "EMP-1020" {
				for i, d := range g["days"].([]any) {
					if d == day(1) {
						cell := row["cells"].([]any)[i].(map[string]any)
						found = cell["code"] == "E" && cell["late"] == true
					}
				}
			}
		}
		if !found {
			t.Fatal("the changed cell shows on the roster", g["rows"])
		}
		if s, _ := c.do("POST", "/api/roster/publish", "EMP-1009", "Line Manager", map[string]string{"location": hub, "department": "F&B", "week": day(1)}); s != 403 {
			t.Fatal("line managers build, they do not publish")
		}
	})

	d1, d2, d3, d4 := day(-5), day(-4), day(-3), day(-2)
	t.Run("attendance upload and statuses", func(t *testing.T) {
		csv := "Employee ID,Date,In time,Out time\n" +
			"EMP-1020," + d1 + ",06:00,14:00\n" +
			"EMP-1020," + d2 + ",06:00,\n" +
			"EMP-1020," + d3 + ",08:10,16:00\n" +
			"EMP-1020," + day(1) + ",06:00,14:00\n"
		_, r := c.sendFile("/api/bulk/att/apply", "EMP-1002", "HR Admin", "att.csv", []byte(csv))
		if r["ok"].(float64) != 3 {
			t.Fatal("a future date is refused", r)
		}
		_, a := c.do("GET", "/api/attendance?employee=EMP-1020&ym="+d1[:7], "EMP-1009", "Line Manager", nil)
		st := map[string]map[string]any{}
		for _, x := range a["days"].([]any) {
			m := x.(map[string]any)
			st[m["date"].(string)] = m
		}
		if d1[:7] != d4[:7] {
			t.Skip("the test days straddle a month end")
		}
		for d, want := range map[string]string{d1: "Present", d2: "Missing out punch", d3: "Half day"} {
			if p := st[d]["planned"]; p == "WO" || p == "PH" {
				continue // a weekly off or holiday in the window
			}
			if st[d]["status"] != want {
				t.Errorf("%s: %v, want %s", d, st[d]["status"], want)
			}
		}
		if st[d3]["planned"] == "M" && st[d3]["dev"] == nil {
			t.Error("08:10 on an M day is a deviation")
		}
	})

	t.Run("deviation OK regularizes the roster", func(t *testing.T) {
		_, l := c.do("GET", "/api/deviations?from="+d3+"&to="+d3, "EMP-1009", "Line Manager", nil)
		if len(l["rows"].([]any)) != 1 {
			t.Skip("no deviation that day (an off day in the window)")
		}
		if s, _ := c.do("POST", "/api/deviations/decide", "EMP-1009", "Line Manager", map[string]any{"items": []map[string]string{{"employeeId": "EMP-1020", "date": d3}}, "status": "OK"}); s != 403 {
			t.Fatal("deciding needs the publish right")
		}
		if _, r := c.do("POST", "/api/deviations/decide", "EMP-1002", "HR Admin", map[string]any{"items": []map[string]string{{"employeeId": "EMP-1020", "date": d3}}, "status": "OK", "note": "covering breakfast"}); r["done"].(float64) != 1 {
			t.Fatal(r)
		}
		code, _ := apiDoc(st, "roster_entry", bson.M{"employee_id": "EMP-1020", "date": d3, "by_dev": true})["code"].(string)
		if code != "G" {
			t.Fatalf("the roster follows the shift worked, got %q", code)
		}
	})

	t.Run("regularization", func(t *testing.T) {
		if s, _ := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "regularize", "payload": map[string]string{"date": d2, "rtype": "No Punch", "in": "06:00", "out": "14:00", "reason": "Roster changed"}}); s != 400 {
			t.Fatal("Roster changed needs a roster change that day")
		}
		// d2 can fall in a month whose payroll cycle has already closed (early in a month); Payroll then opens an
		// exception, as a real regularization that late would need. The window rule itself is tested in policies.
		if s, r := c.do("POST", "/api/exceptions", "EMP-1002", "HR Admin", map[string]string{"employeeId": "EMP-1020", "kind": "regularize", "from": d2, "to": d2, "until": day(3), "reason": "Late regularization in the test"}); s != 200 {
			t.Fatal(r)
		}
		s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "regularize", "payload": map[string]string{"date": d2, "rtype": "No Punch", "out": "14:00", "reason": "Forgot to punch"}})
		if s != 200 {
			t.Fatal(r)
		}
		id := r["id"].(string)
		for i := 0; i < 4; i++ {
			if _, a := c.do("POST", "/api/approvals/"+id+"/approve", "EMP-1002", "HR Admin", map[string]string{"note": "ok"}); a["status"] == "Approved" {
				break
			}
		}
		a := apiDoc(st, "attendance_day", bson.M{"employee_id": "EMP-1020", "date": d2})
		in, _ := a["in_time"].(string)
		out, _ := a["out_time"].(string)
		src, _ := a["source"].(string)
		if in != "06:00" || out != "14:00" || src != "Regularized" {
			t.Fatal(in, out, src)
		}
		old := day(-40)
		if s, _ := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "regularize", "payload": map[string]string{"date": old, "rtype": "No Punch", "in": "06:00", "out": "14:00", "reason": "Forgot to punch"}}); s != 400 {
			t.Fatal("outside the window")
		}
		if s, r := c.do("POST", "/api/exceptions", "EMP-1012", "Payroll Manager", map[string]string{"employeeId": "EMP-1020", "kind": "regularize", "from": old, "to": old, "until": day(3), "reason": "Device fault"}); s != 200 {
			t.Fatal(r)
		}
		if s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "regularize", "payload": map[string]string{"date": old, "rtype": "No Punch", "in": "06:00", "out": "14:00", "reason": "Biometric or device issue"}}); s != 200 {
			t.Fatal("an exception opens the window", r)
		}
	})

	t.Run("month register and leave balance", func(t *testing.T) {
		_, m := c.do("GET", "/api/attendance/month/me?ym="+d1[:7], "EMP-1020", "Employee", nil)
		if m["month"].(map[string]any)["daysN"].(float64) < 28 {
			t.Fatal(m)
		}
		_, b := c.do("GET", "/api/leave/balances/me", "EMP-1020", "Employee", nil)
		names := []string{}
		for _, x := range b["types"].([]any) {
			names = append(names, x.(map[string]any)["type"].(string))
		}
		if !containsAny(toAny(names), "Casual leave") || containsAny(toAny(names), "Maternity leave") {
			t.Fatalf("types for a man on the standard plan: %v", names)
		}
	})

	t.Run("consequences", func(t *testing.T) {
		s, r := c.do("POST", "/api/consequences", "EMP-1009", "Line Manager", map[string]any{"employeeId": "EMP-1020", "kind": "PIP", "days": 15, "reason": "Attendance and punctuality", "details": "Late on five days this month."})
		if s != 200 {
			t.Fatal(r)
		}
		id := fmt.Sprint(r["id"])
		if s, _ := c.do("POST", "/api/consequences/"+id+"/approve", "EMP-1009", "Line Manager", nil); s != 403 {
			t.Fatal("the HRBP approves a plan")
		}
		if s, r := c.do("POST", "/api/consequences/"+id+"/approve", "EMP-1004", "HRBP", map[string]string{"note": "ok"}); s != 200 {
			t.Fatal(r)
		}
		_, l := c.do("GET", "/api/consequences/"+id+"/letter", "EMP-1020", "Employee", nil)
		if !strings.Contains(fmt.Sprint(l["text"]), "15 days") {
			t.Fatal(l)
		}
	})
}

func toAny(l []string) []any {
	out := make([]any, len(l))
	for i, x := range l {
		out[i] = x
	}
	return out
}

// Payroll cycle: pay items through approvals, month close, dry run with an edit, verify,
// approve, publish, payslips, holds and the locked month.
func TestPayroll(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	for _, e := range []string{"EMP-1002", "EMP-1011", "EMP-1012", "EMP-1014", "EMP-1020"} {
		c.signIn(e)
	}
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open, _ := state["openYm"].(string)
	if len(open) != 7 {
		t.Fatal(state)
	}
	next := func(ym string, n int) string {
		tm, _ := time.Parse("2006-01", ym)
		return tm.AddDate(0, n, 0).Format("2006-01")
	}
	approve := func(id string) {
		t.Helper()
		for i := 0; i < 8; i++ {
			_, d := c.do("GET", "/api/approvals/"+id, "EMP-1002", "HR Admin", nil)
			if d["status"] != "Pending" {
				if d["status"] != "Approved" {
					t.Fatalf("%s %v", id, d["status"])
				}
				return
			}
			c.do("POST", "/api/approvals/"+id+"/approve", "EMP-1002", "HR Admin", map[string]string{"note": "payroll test"})
		}
		t.Fatalf("%s never approved", id)
	}
	lines := func(src string) []map[string]any {
		rows, _ := st.DB.C("ledger").Find(ctx, bson.M{"source_id": src, "void": false}, options.Find().SetSort(bson.D{{Key: "ym", Value: 1}, {Key: "_id", Value: 1}}))
		out := []map[string]any{}
		for _, x := range rows {
			out = append(out, map[string]any{"ym": x["ym"], "kind": x["kind"], "amount": apiFloat(x["amount"])})
		}
		return out
	}
	var adv string

	t.Run("pay items", func(t *testing.T) {
		// An employee asks for their own advance (advances_test.go), never for someone else.
		if s, _ := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "p_adv", "employeeId": "EMP-1016", "payload": map[string]any{"amount": 5000, "instalments": 2, "note": "for a colleague"}}); s != 403 {
			t.Fatal("an employee cannot raise an advance for someone else")
		}
		if s, _ := c.do("POST", "/api/approvals", "EMP-1011", "Payroll", map[string]any{"wf": "p_adv", "employeeId": "EMP-1014", "payload": map[string]any{"amount": 5000, "instalments": 2, "note": "test"}}); s != 400 {
			t.Fatal("advances are for permanent staff")
		}
		_, r := c.do("POST", "/api/approvals", "EMP-1011", "Payroll", map[string]any{"wf": "p_adv", "employeeId": "EMP-1020",
			"payload": map[string]any{"amount": 12000, "instalments": 4, "payMonth": open, "startYm": next(open, 1), "note": "Medical emergency"}})
		adv, _ = r["id"].(string)
		if adv == "" {
			t.Fatal(r)
		}
		approve(adv)
		l := lines(adv)
		if len(l) != 5 || l[0]["kind"] != "earn" || l[0]["amount"] != 12000.0 || l[1]["ym"] != next(open, 1) || l[4]["amount"] != 3000.0 {
			t.Fatalf("advance lines %v", l)
		}
		if s, _ := c.do("POST", "/api/approvals", "EMP-1011", "Payroll", map[string]any{"wf": "p_ded", "employeeId": "EMP-1020",
			"payload": map[string]any{"amount": 6000, "instalments": 3, "dedType": "Cash shortage", "note": "Shortage at the till on 3 Oct"}}); s != 400 {
			t.Fatal("the employee must have been heard")
		}
		if s, _ := c.do("POST", "/api/approvals", "EMP-1011", "Payroll", map[string]any{"wf": "p_ded", "employeeId": "EMP-1014",
			"payload": map[string]any{"amount": 600, "instalments": 1, "dedType": "Cash shortage", "note": "Shortage at the till", "heard": "Yes"}}); s != 400 {
			t.Fatal("contractual staff have no deductions")
		}
		_, r = c.do("POST", "/api/approvals", "EMP-1011", "Payroll", map[string]any{"wf": "p_ded", "employeeId": "EMP-1020",
			"payload": map[string]any{"amount": 6000, "instalments": 3, "dedType": "Cash shortage", "note": "Shortage at the till on 3 Oct", "heard": "Yes"}})
		ded, _ := r["id"].(string)
		approve(ded)
		if l := lines(ded); len(l) != 3 || l[0]["ym"] != open || l[2]["amount"] != 2000.0 {
			t.Fatalf("deduction lines %v", l)
		}
		_, r = c.do("POST", "/api/approvals", "EMP-1011", "Payroll", map[string]any{"wf": "p_arrear", "employeeId": "EMP-1020",
			"payload": map[string]any{"amount": 3500, "forYm": next(open, -1), "arrearType": "Missed payment", "slipText": "Shift allowance not paid"}})
		arr, _ := r["id"].(string)
		approve(arr)
		if l := lines(arr); len(l) != 1 || l[0]["ym"] != open || l[0]["kind"] != "earn" {
			t.Fatalf("arrear lines %v", l)
		}
		_, doc := c.upload("EMP-1020", "Employee", "employee", "EMP-1020", "Bill")
		_, r = c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "p_claim", "payload": map[string]any{"amount": 800, "claimType": "Mobile and internet", "billDate": today(), "doc": doc["id"]}})
		clm, _ := r["id"].(string)
		approve(clm)
		if l := lines(clm); len(l) != 1 || l[0]["amount"] != 800.0 {
			t.Fatalf("claim lines %v", l)
		}
	})

	t.Run("month close and dry run", func(t *testing.T) {
		if s, _ := c.do("POST", "/api/payroll/"+open+"/dry-run", "EMP-1020", "Employee", nil); s != 403 {
			t.Fatal("employees cannot run payroll")
		}
		if s, _ := c.do("POST", "/api/payroll/"+open+"/dry-run", "EMP-1011", "Payroll", nil); s != 400 {
			t.Fatal("the dry run needs the checklist closed")
		}
		c.pcCloseAll(t, open) // the run checklist, in place of Month close
		if s, r := c.do("POST", "/api/payroll/"+open+"/dry-run", "EMP-1011", "Payroll", nil); s != 200 || r["status"] != "Dry run" {
			t.Fatal(r)
		}
		_, rows := c.do("GET", "/api/payroll/"+open+"/rows?q=EMP-1020", "EMP-1011", "Payroll", nil)
		list, _ := rows["rows"].([]any)
		if len(list) != 1 {
			t.Fatal(rows)
		}
		row := list[0].(map[string]any)
		flags := fmt.Sprint(row["flags"])
		if !strings.Contains(flags, "extra") || !strings.Contains(flags, "recov") {
			t.Fatalf("flags %v", flags)
		}
		_, all := c.do("GET", "/api/payroll/"+open+"/rows?size=500", "EMP-1011", "Payroll", nil)
		for _, x := range all["rows"].([]any) {
			if x.(map[string]any)["employeeId"] == "EMP-1014" && x.(map[string]any)["type"] != "Contractual" {
				t.Fatal("type")
			}
		}
		if s, _ := c.do("PATCH", "/api/payroll/"+open+"/rows/EMP-1020", "EMP-1011", "Payroll", map[string]any{"paidDays": 20}); s != 400 {
			t.Fatal("an edit needs a reason")
		}
		if s, r := c.do("PATCH", "/api/payroll/"+open+"/rows/EMP-1020", "EMP-1011", "Payroll", map[string]any{"paidDays": 20, "otherEarn": 500, "reason": "Late joining on site"}); s != 200 {
			t.Fatal(r)
		}
		_, one := c.do("GET", "/api/payroll/"+open+"/rows/EMP-1020", "EMP-1011", "Payroll", nil)
		rr := one["row"].(map[string]any)
		if rr["paid"] != 20.0 || rr["edited"] != true || rr["oe"] != 500.0 {
			t.Fatalf("edited row %v %v %v", rr["paid"], rr["edited"], rr["oe"])
		}
		if s, r := c.do("POST", "/api/payroll/"+open+"/closures/attendance/reopen", "EMP-1012", "Payroll Manager", map[string]any{"reason": "Late punches to check"}); s != 200 {
			t.Fatal(r)
		}
		if s, _ := c.do("POST", "/api/payroll/"+open+"/verify", "EMP-1011", "Payroll", nil); s != 400 {
			t.Fatal("verify needs the checklist closed")
		}
		if s, r := c.do("POST", "/api/payroll/"+open+"/closures/attendance/close", "EMP-1011", "Payroll", nil); s != 200 {
			t.Fatal(r)
		}
		_, checks := c.do("GET", "/api/payroll/"+open+"/checks", "EMP-1011", "Payroll", nil)
		if l, _ := checks["list"].([]any); len(l) != 7 {
			t.Fatal(checks)
		}
	})

	t.Run("verify approve publish", func(t *testing.T) {
		if s, r := c.do("POST", "/api/payroll/"+open+"/verify", "EMP-1011", "Payroll", nil); s != 200 || r["status"] != "Verified" {
			t.Fatal(r)
		}
		if s, _ := c.do("POST", "/api/payroll/"+open+"/approve", "EMP-1011", "Payroll", nil); s != 403 {
			t.Fatal("approve needs the Payroll Manager")
		}
		if s, r := c.do("POST", "/api/payroll/"+open+"/approve", "EMP-1012", "Payroll Manager", nil); s != 200 || r["status"] != "Approved" {
			t.Fatal(r)
		}
		_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
		if state["openYm"] != next(open, 1) {
			t.Fatalf("open month after approval %v", state["openYm"])
		}
		if s, r := c.do("POST", "/api/payroll/"+open+"/publish", "EMP-1012", "Payroll Manager", nil); s != 200 || r["status"] != "Published" {
			t.Fatal(r)
		}
		_, slips := c.do("GET", "/api/payslips", "EMP-1020", "Employee", nil)
		if l, _ := slips["slips"].([]any); len(l) != 1 {
			t.Fatal(slips)
		}
		_, slip := c.do("GET", "/api/payslips/"+open+"/EMP-1020", "EMP-1020", "Employee", nil)
		snap, _ := slip["snap"].(map[string]any)
		net := apiFloat(apiDoc(st, "payroll_row", bson.M{"ym": open, "employee_id": "EMP-1020"})["net"])
		if snap == nil || snap["net"] != net || !strings.HasPrefix(fmt.Sprint(snap["words"]), "Rupees ") {
			t.Fatalf("payslip %v net %v", snap["net"], net)
		}
		if s, _ := c.do("GET", "/api/payslips/"+open+"/EMP-1014", "EMP-1014", "Employee", nil); s != 404 {
			t.Fatal("contractual staff get no payslip document")
		}
		if s, _ := c.do("GET", "/api/payslips/"+open+"/EMP-1020", "EMP-1014", "Employee", nil); s != 403 {
			t.Fatal("another person's payslip")
		}
		if s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "regularize",
			"payload": map[string]any{"date": open + "-15", "rtype": "No Punch", "in": "09:00", "out": "18:00", "reason": "Forgot to punch"}}); s != 400 || !strings.Contains(fmt.Sprint(r), "already paid") {
			t.Fatalf("locked month regularization %d %v", s, r)
		}
		req, _ := http.NewRequest("GET", c.base+"/api/payroll/"+open+"/bank-advice.xlsx", nil)
		req.Header.Set("Authorization", "Bearer "+c.tokens["EMP-1012"])
		req.Header.Set("X-Role", "Payroll Manager")
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Type"), "spreadsheet") {
			t.Fatalf("bank advice %v %v", err, res.StatusCode)
		}
		res.Body.Close()
	})

	t.Run("advance schedule, hold and file", func(t *testing.T) {
		if s, r := c.do("POST", "/api/advances/"+adv+"/skip", "EMP-1011", "Payroll", nil); s != 200 {
			t.Fatal(r)
		}
		l := lines(adv)
		if len(l) != 5 || l[len(l)-1]["ym"] != next(open, 5) {
			t.Fatalf("after skip %v", l)
		}
		if s, _ := c.do("POST", "/api/holds", "EMP-1011", "Payroll", map[string]any{"employeeId": "EMP-1020", "reason": "Bank or KYC issue", "note": "x"}); s != 400 {
			t.Fatal("a hold needs a note")
		}
		_, h := c.do("POST", "/api/holds", "EMP-1011", "Payroll", map[string]any{"employeeId": "EMP-1020", "reason": "Bank or KYC issue", "note": "Account closed by the bank"})
		hid, _ := h["id"].(string)
		if hid == "" {
			t.Fatal(h)
		}
		_, f := c.do("GET", "/api/employees/EMP-1020/file", "EMP-1011", "Payroll", nil)
		if ov, _ := f["overall"].([]any); len(ov) == 0 || ov[0] != "On hold" {
			t.Fatalf("file %v", f["overall"])
		}
		if s, r := c.do("POST", "/api/holds/"+hid+"/release", "EMP-1011", "Payroll", map[string]string{"note": "New account verified"}); s != 200 {
			t.Fatal(r)
		}
		_, up := c.do("GET", "/api/payroll/upcoming", "EMP-1011", "Payroll", nil)
		if l, _ := up["lines"].([]any); len(l) < 4 {
			t.Fatalf("upcoming %v", up["lines"])
		}
	})
}

// Tax: windows, declaration and proof, review, finalise, monthly TDS in the run, deposit,
// 24Q data, Form 16 release; statutory files from the approved month.
func TestTaxAndStatutory(t *testing.T) {
	c, _, st := setupFull(t)
	for _, e := range []string{"EMP-1002", "EMP-1011", "EMP-1012", "EMP-1014"} {
		c.signIn(e)
	}
	yes, no := true, false
	_, tax := c.do("GET", "/api/tax/me", "EMP-1002", "Employee", nil)
	fy, _ := tax["fy"].(string)
	w := tax["windows"].(map[string]any)
	win := map[string]any{"declFrom": w["declFrom"], "declTo": w["declTo"], "declForce": yes, "proofFrom": w["proofFrom"], "proofTo": w["proofTo"], "proofForce": yes}
	if s, r := c.do("PUT", "/api/tax/windows", "EMP-1002", "HR Admin", win); s != 200 {
		t.Fatal(r)
	}
	if s, _ := c.do("PUT", "/api/tax/me", "EMP-1014", "Employee", map[string]any{"regime": "New", "items": map[string]any{}}); s != 400 {
		t.Fatal("contractual staff do not declare")
	}
	decl := map[string]any{"regime": "Old", "items": map[string]any{"c_ppf": 150000, "d_self": 25000, "hra": 240000}, "submit": true}
	if s, _ := c.do("PUT", "/api/tax/me", "EMP-1002", "Employee", decl); s != 400 {
		t.Fatal("rent needs the landlord")
	}
	decl["landlordName"], decl["landlordPan"] = "Ramesh Kapoor", "ABCPK1234F"
	if s, r := c.do("PUT", "/api/tax/me", "EMP-1002", "Employee", decl); s != 200 {
		t.Fatal(r)
	}
	_, doc := c.upload("EMP-1002", "Employee", "employee", "EMP-1002", "PPF passbook")
	_, pf := c.do("POST", "/api/tax/me/proofs", "EMP-1002", "Employee", map[string]any{"item": "c_ppf", "doc": doc["id"], "amount": 150000, "remark": "FY deposit"})
	pid := fmt.Sprint(pf["id"])
	_, list := c.do("GET", "/api/tax", "EMP-1011", "Payroll", nil)
	found := false
	for _, x := range list["rows"].([]any) {
		m := x.(map[string]any)
		if m["id"] == "EMP-1002" {
			found = m["status"] == "Proofs to review"
		}
	}
	if !found {
		t.Fatal("EMP-1002 should have a proof to review")
	}
	if s, _ := c.do("POST", "/api/tax/proofs/"+pid+"/approve", "EMP-1011", "Payroll", map[string]any{"amount": 100000}); s != 200 {
		t.Fatal("approve proof")
	}
	if s, _ := c.do("POST", "/api/tax/finalise", "EMP-1012", "Payroll Manager", nil); s != 400 {
		t.Fatal("finalise needs the proof window closed")
	}
	win["proofForce"], win["declForce"] = no, no
	c.do("PUT", "/api/tax/windows", "EMP-1002", "HR Admin", win)
	if s, _ := c.do("POST", "/api/tax/finalise", "EMP-1011", "Payroll", nil); s != 403 {
		t.Fatal("only a payroll approver finalises")
	}
	if s, _ := c.do("POST", "/api/tax/finalise", "EMP-1012", "Payroll Manager", nil); s != 400 {
		t.Fatal("before the proof window's end date finalising needs an explicit early finalisation (RUN-87)")
	}
	if s, r := c.do("POST", "/api/tax/finalise", "EMP-1012", "Payroll Manager", map[string]any{"force": true, "reason": "Year-end test"}); s != 200 || r["employees"].(float64) < 1 {
		t.Fatal(r)
	}
	_, me := c.do("GET", "/api/tax/me", "EMP-1002", "Employee", nil)
	fig := me["figure"].(map[string]any)
	if fig["basis"] != "approved" {
		t.Fatal(fig["basis"])
	}
	annual := fig["annual"].(float64)
	if annual <= 0 {
		t.Fatal("EMP-1002 earns enough to pay tax")
	}

	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open := state["openYm"].(string)
	c.pcCloseAll(t, open)
	c.do("POST", "/api/payroll/"+open+"/dry-run", "EMP-1011", "Payroll", nil)
	_, one := c.do("GET", "/api/payroll/"+open+"/rows/EMP-1002", "EMP-1011", "Payroll", nil)
	row := one["row"].(map[string]any)
	last := fig["last"].(string)
	left := 0
	for m := open; m <= last; {
		left++
		tm, _ := time.Parse("2006-01", m)
		m = tm.AddDate(0, 1, 0).Format("2006-01")
	}
	if want := float64(int(annual/float64(left) + 0.5)); row["stds"] != want {
		t.Fatalf("monthly TDS %v, want %v (annual %v over %d months)", row["stds"], want, annual, left)
	}
	c.do("POST", "/api/payroll/"+open+"/verify", "EMP-1011", "Payroll", nil)
	c.do("POST", "/api/payroll/"+open+"/approve", "EMP-1012", "Payroll Manager", nil)
	if s, r := c.do("POST", "/api/payroll/"+open+"/publish", "EMP-1012", "Payroll Manager", nil); s != 200 {
		t.Fatal(r)
	}
	hist := apiFloat(apiDoc(st, "tds_history", bson.M{"employee_id": "EMP-1002", "ym": open, "section": "192"})["amount"])
	if hist != row["stds"] {
		t.Fatalf("TDS history %v", hist)
	}

	if s, _ := c.do("PUT", "/api/tds/deposit/s/"+open, "EMP-1012", "Payroll Manager", map[string]any{"date": today(), "bsr": "12345", "challan": "00042"}); s != 400 {
		t.Fatal("BSR is 7 digits")
	}
	if s, r := c.do("PUT", "/api/tds/deposit/s/"+open, "EMP-1012", "Payroll Manager", map[string]any{"date": today(), "bsr": "0510002", "challan": "00042"}); s != 200 {
		t.Fatal(r)
	}
	_, tds := c.do("GET", "/api/tds", "EMP-1012", "Payroll Manager", nil)
	ok := false
	for _, x := range tds["months"].([]any) {
		m := x.(map[string]any)
		ok = ok || (m["ym"] == open && m["kind"] == "s" && m["status"] == "Deposited")
	}
	if !ok {
		t.Fatal(tds["months"])
	}
	get := func(path, emp, role string) int {
		req, _ := http.NewRequest("GET", c.base+path, nil)
		req.Header.Set("Authorization", "Bearer "+c.tokens[emp])
		req.Header.Set("X-Role", role)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	q := fmt.Sprint(map[string]int{"01": 4, "02": 4, "03": 4, "04": 1, "05": 1, "06": 1, "07": 2, "08": 2, "09": 2, "10": 3, "11": 3, "12": 3}[open[5:]])
	if s := get("/api/tds/return/24Q/"+q+"/sheet?fy="+fy, "EMP-1012", "Payroll Manager"); s != 400 {
		t.Fatalf("24Q needs the company TAN and PAN, got %d", s)
	}
	setup := map[string]any{"setup": map[string]any{"accent": "#0f6e6e", "footer": "Computer generated", "showEmployer": true}, "cin": "", "pan": "AAACS1234B", "tan": "DELS12345A",
		"pfCode": "", "esiCode": "", "cutoff": "calendar", "firstYm": ""}
	if s, r := c.do("PUT", "/api/payslip-setup", "EMP-1002", "HR Admin", setup); s != 200 {
		t.Fatal(r)
	}
	if s := get("/api/tds/return/24Q/"+q+"/sheet?fy="+fy, "EMP-1012", "Payroll Manager"); s != 200 {
		t.Fatalf("24Q data %d", s)
	}
	if s, _ := c.do("GET", "/api/form16/me?fy="+fy, "EMP-1014", "Employee", nil); s != 404 {
		t.Fatal("Form 16 is not released yet")
	}
	if s, r := c.do("POST", "/api/form16/release?fy="+fy, "EMP-1012", "Payroll Manager", nil); s != 200 || r["final"] != false {
		t.Fatal(r)
	}
	_, f16 := c.do("GET", "/api/form16/me?fy="+fy, "EMP-1002", "Employee", nil)
	pb, _ := f16["partB"].(map[string]any)
	if pb == nil || pb["total"] != annual || pb["done"] != hist {
		t.Fatalf("Form 16 %v", f16)
	}
	_, ecr := c.do("GET", "/api/statutory/ecr?ym="+open, "EMP-1012", "Payroll Manager", nil)
	if rows, _ := ecr["rows"].([]any); len(rows) == 0 {
		t.Fatal(ecr)
	}
	if s := get("/api/statutory/ecr.txt?ym="+open, "EMP-1012", "Payroll Manager"); s != 200 {
		t.Fatalf("ECR text %d", s)
	}
	ms := strings.Split(fy, "-")
	if s, r := c.do("GET", "/api/statutory/pf?from="+ms[0]+"-04-01&to="+today(), "EMP-1012", "Payroll Manager", nil); s != 200 {
		t.Fatal(r)
	}
	if s, _ := c.do("GET", "/api/statutory/pf?from="+ms[0]+"-04-01&to="+today(), "EMP-1014", "Employee", nil); s != 403 {
		t.Fatal("employees cannot download statutory files")
	}
}

// Separation: resignation with an earlier last day and recovered shortfall, clearance by three
// parties, assets and uniform from the inventory, full and final, letters, close; termination,
// absconding with show-cause and reporting back, and withdrawal.
func TestSeparationAndInventory(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	for _, e := range []string{"EMP-1002", "EMP-1004", "EMP-1009", "EMP-1011", "EMP-1020"} {
		c.signIn(e)
	}
	approveAll := func(id string) {
		t.Helper()
		for i := 0; i < 8; i++ {
			_, d := c.do("GET", "/api/approvals/"+id, "EMP-1002", "HR Admin", nil)
			if d["status"] != "Pending" {
				if d["status"] != "Approved" {
					t.Fatalf("%s %v", id, d["status"])
				}
				return
			}
			c.do("POST", "/api/approvals/"+id+"/approve", "EMP-1002", "HR Admin", map[string]string{"note": "separation test"})
		}
		t.Fatalf("%s never approved", id)
	}
	shirt := apiFloat(apiDoc(st, "inv_item", bson.M{"name": "Shirt"})["id"])
	laptop := apiFloat(apiDoc(st, "inv_item", bson.M{"name": "Laptop"})["id"])
	hrbpStore := "hrbp:EMP-1004"

	t.Run("inventory", func(t *testing.T) {
		if s, _ := c.do("POST", "/api/inventory/stock/add", "EMP-1004", "HRBP", map[string]any{"to": hrbpStore, "lines": []any{map[string]any{"itemId": shirt, "qty": 5}}}); s != 403 {
			t.Fatal("an HRBP does not add stock centrally")
		}
		if s, r := c.do("POST", "/api/inventory/stock/add", "EMP-1002", "HR Admin", map[string]any{"to": "central", "lines": []any{
			map[string]any{"itemId": shirt, "qty": 10}, map[string]any{"itemId": laptop, "serials": []string{"lp-1", "LP-2"}}}}); s != 200 {
			t.Fatal(r)
		}
		if s, r := c.do("POST", "/api/inventory/stock/transfer", "EMP-1002", "HR Admin", map[string]any{"from": "central", "to": hrbpStore, "lines": []any{
			map[string]any{"itemId": shirt, "qty": 4}, map[string]any{"itemId": laptop, "serials": []string{"LP-1"}}}}); s != 200 {
			t.Fatal(r)
		}
		if s, _ := c.do("POST", "/api/inventory/issue", "EMP-1004", "HRBP", map[string]any{"store": hrbpStore, "employeeId": "EMP-1020", "lines": []any{map[string]any{"itemId": shirt, "qty": 5}}}); s != 400 {
			t.Fatal("only 4 shirts in the HRBP store")
		}
		_, u := c.do("POST", "/api/inventory/issue", "EMP-1004", "HRBP", map[string]any{"store": hrbpStore, "employeeId": "EMP-1020", "lines": []any{map[string]any{"itemId": shirt, "qty": 2}}})
		if u["recover"] != true || u["amount"] != 900.0 {
			t.Fatal(u)
		}
		if n := apiCount(st, "ledger", bson.M{"source_id": u["id"]}); n != 3 {
			t.Fatalf("uniform: two recoveries and a refund, got %d lines", n)
		}
		aid, _ := apiDoc(st, "asset", bson.M{"serial": "LP-1"})["id"].(int64)
		if s, r := c.do("POST", "/api/inventory/issue", "EMP-1004", "HRBP", map[string]any{"store": hrbpStore, "employeeId": "EMP-1020", "assetId": aid}); s != 200 {
			t.Fatal(r)
		}
		_, inv := c.do("GET", "/api/inventory?store="+hrbpStore, "EMP-1004", "HRBP", nil)
		for _, x := range inv["summary"].([]any) {
			m := x.(map[string]any)
			if m["name"] == "Shirt" && m["closing"] != 2.0 {
				t.Fatalf("shirt closing %v", m["closing"])
			}
		}
	})

	var sep string
	t.Run("resignation to close", func(t *testing.T) {
		_, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "resign", "payload": map[string]any{"reason": "Relocation"}})
		req, _ := r["id"].(string)
		sep, _ = r["case"].(string)
		if req == "" || sep == "" {
			t.Fatal(r)
		}
		_, d := c.do("GET", "/api/separations/"+sep, "EMP-1020", "Employee", nil)
		if d["stage"] != "Approval" {
			t.Fatal(d["stage"])
		}
		notice := d["request"].(map[string]any)["noticeLwd"].(string)
		early := mustDate(notice).AddDate(0, 0, -10).Format("2006-01-02")
		if s, _ := c.do("POST", "/api/separations/"+sep+"/terms", "EMP-1009", "Line Manager", map[string]any{"lwd": mustDate(notice).AddDate(0, 0, 3).Format("2006-01-02"), "shortfall": "waive"}); s != 400 {
			t.Fatal("the last day can only come earlier")
		}
		if s, r := c.do("POST", "/api/separations/"+sep+"/terms", "EMP-1009", "Line Manager", map[string]any{"lwd": early, "shortfall": "recover"}); s != 200 {
			t.Fatal(r)
		}
		approveAll(req)
		lwd, _ := apiDoc(st, "employee", bson.M{"_id": "EMP-1020", "exit_status": "On notice"})["lwd"].(string)
		if lwd != early {
			t.Fatalf("on notice to %s, want %s", lwd, early)
		}
		ntc := apiFloat(apiDoc(st, "ledger", bson.M{"source_id": "NTC-EMP-1020"})["amount"])
		if ntc <= 0 {
			t.Fatal("the 10-day shortfall is recovered")
		}
		_, d = c.do("GET", "/api/separations/"+sep, "EMP-1009", "Line Manager", nil)
		if d["stage"] != "Notice" {
			t.Fatal(d["stage"])
		}
		items := d["clearance"].([]any)
		sign := func(owner, emp, role string, want int) {
			for _, x := range items {
				m := x.(map[string]any)
				if m["owner"] == owner && m["auto"] == nil {
					if s, r := c.do("POST", fmt.Sprintf("/api/separations/%s/clearance/%v", sep, m["id"]), emp, role, nil); s != want {
						t.Fatalf("%s by %s: %d %v", m["name"], role, s, r)
					}
				}
			}
		}
		sign("Reporting manager", "EMP-1020", "Employee", 403)
		sign("Reporting manager", "EMP-1009", "Line Manager", 200)
		sign("HRBP", "EMP-1004", "HRBP", 200)
		sign("Finance", "EMP-1011", "Payroll", 200)
		// The last working day passes: the end of last month, so the uniform's second instalment
		// (this month) falls after it and is brought forward, whatever day the test runs.
		monthStart, _ := time.Parse("2006-01-02", store.Today()[:8]+"01")
		_, _ = st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1020"}, bson.M{"$set": bson.M{"lwd": monthStart.AddDate(0, 0, -1).Format("2006-01-02")}})
		_, d = c.do("GET", "/api/separations/"+sep, "EMP-1004", "HRBP", nil)
		if d["stage"] != "Clearance" || !strings.Contains(fmt.Sprint(d["next"]), "HRBP") {
			t.Fatalf("assets and exit interview are still open: %v %v", d["stage"], d["next"])
		}
		if s, _ := c.do("POST", "/api/fnf/EMP-1020", "EMP-1011", "Payroll", nil); s != 400 {
			t.Fatal("full and final waits for the laptop")
		}
		aid, _ := apiDoc(st, "asset", bson.M{"serial": "LP-1"})["id"].(int64)
		if s, r := c.do("POST", "/api/inventory/return", "EMP-1004", "HRBP", map[string]any{"store": hrbpStore, "assetId": aid}); s != 200 {
			t.Fatal(r)
		}
		ratings := map[string]any{"Manager": 4, "Work and shifts": 3, "Pay and benefits": 2, "Growth": 3, "Workplace and team": 4}
		if s, r := c.do("POST", "/api/separations/"+sep+"/interview", "EMP-1020", "Employee", map[string]any{"mainReason": "Relocation", "ratings": ratings, "recommend": "Yes"}); s != 200 {
			t.Fatal(r)
		}
		_, d = c.do("GET", "/api/separations/"+sep, "EMP-1004", "HRBP", nil)
		if d["stage"] != "Full and final" {
			t.Fatal(d["stage"], d["next"])
		}
		_, pv := c.do("GET", "/api/fnf/EMP-1020", "EMP-1011", "Payroll", nil)
		keys := ""
		for _, l := range pv["lines"].([]any) {
			keys += l.(map[string]any)["key"].(string) + " "
		}
		// The uniform refund, due after the exit, is not paid (policy of 13 Nov 2026, uniform.go): an info line, and
		// the recovery still to come is brought forward.
		if !strings.Contains(keys, "ntc") || !strings.Contains(keys, "rec") || !strings.Contains(keys, "unirefund") || strings.Contains(keys, "earn2") {
			t.Fatalf("lines %s", keys)
		}
		_, f := c.do("POST", "/api/fnf/EMP-1020", "EMP-1011", "Payroll", nil)
		fid, _ := f["id"].(string)
		if fid == "" {
			t.Fatal(f)
		}
		approveAll(fid)
		if voided := apiCount(st, "ledger", bson.M{"employee_id": "EMP-1020", "void": true, "void_reason": bson.M{"$regex": "^Brought forward"}}); voided < 1 {
			t.Fatalf("brought-forward lines voided: %d", voided)
		}
		if n := apiCount(st, "ledger", bson.M{"employee_id": "EMP-1020", "void": true, "kind": "earn", "source_id": bson.M{"$regex": "^UNI-"},
			"void_reason": bson.M{"$regex": "^No uniform refund"}}); n != 1 {
			t.Fatalf("the uniform refund is cancelled by the full and final: %d", n)
		}
		if s, _ := c.do("POST", "/api/separations/"+sep+"/close", "EMP-1004", "HRBP", map[string]any{"rehire": "Eligible"}); s != 400 {
			t.Fatal("the relieving letter comes first")
		}
		if s, r := c.do("POST", "/api/separations/"+sep+"/letter", "EMP-1004", "HRBP", map[string]any{"kind": "Relieving letter"}); s != 200 {
			t.Fatal(r)
		}
		if s, r := c.do("POST", "/api/separations/"+sep+"/close", "EMP-1004", "HRBP", map[string]any{"rehire": "Eligible"}); s != 200 {
			t.Fatal(r)
		}
		_, d = c.do("GET", "/api/separations/"+sep, "EMP-1020", "Employee", nil)
		if d["stage"] != "Closed" || d["exitInterview"] == nil {
			t.Fatal(d["stage"])
		}
	})

	t.Run("termination, absconding, withdrawal", func(t *testing.T) {
		if s, _ := c.do("POST", "/api/separations", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1016", "type": "Termination", "reason": "Misconduct", "noticeOption": "pay"}); s != 400 {
			t.Fatal("termination needs a note")
		}
		if s, r := c.do("POST", "/api/separations", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1016", "type": "Termination", "reason": "Misconduct",
			"noticeOption": "pay", "note": "Repeated cash shortages after two warnings"}); s != 200 || r["requestId"] == nil {
			t.Fatal(r)
		}
		if s, _ := c.do("POST", "/api/separations", "EMP-1020", "Employee", map[string]any{"employeeId": "EMP-1018", "type": "Termination"}); s != 403 {
			t.Fatal("employees start only resignations")
		}
		_, a := c.do("POST", "/api/separations", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1022", "type": "Absconding", "note": "Not reachable on phone since Monday"})
		abs, _ := a["id"].(string)
		if abs == "" {
			t.Fatal(a)
		}
		holds := apiCount(st, "salary_hold", bson.M{"employee_id": "EMP-1022", "status": "Active"})
		if holds != 1 {
			t.Fatal("absconding holds the salary")
		}
		if s, r := c.do("POST", "/api/separations/"+abs+"/show-cause", "EMP-1004", "HRBP", nil); s != 200 {
			t.Fatal(r)
		}
		if s, _ := c.do("POST", "/api/separations/"+abs+"/show-cause", "EMP-1004", "HRBP", nil); s != 400 {
			t.Fatal("the second letter waits 3 days")
		}
		if s, r := c.do("POST", "/api/separations/"+abs+"/reported-back", "EMP-1004", "HRBP", nil); s != 200 {
			t.Fatal(r)
		}
		holds = apiCount(st, "salary_hold", bson.M{"employee_id": "EMP-1022", "status": "Active"})
		if holds != 0 {
			t.Fatal("reporting back releases the hold")
		}
		_, w := c.do("POST", "/api/separations", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1017", "type": "Resignation", "reason": "Higher studies"})
		wid, _ := w["id"].(string)
		if s, r := c.do("POST", "/api/separations/"+wid+"/withdraw", "EMP-1002", "HR Admin", nil); s != 200 {
			t.Fatal(r)
		}
		_, d := c.do("GET", "/api/separations/"+wid, "EMP-1002", "HR Admin", nil)
		if d["stage"] != "Withdrawn" {
			t.Fatal(d["stage"])
		}
	})
}

func mustDate(s string) time.Time { t, _ := time.Parse("2006-01-02", s); return t }

// Reports run for every role within scope; downloads need the right; the punch connector turns
// device punches into attendance days.
func TestReportsAndConnector(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	for _, e := range []string{"EMP-1002", "EMP-1004", "EMP-1008", "EMP-1011", "EMP-1020"} {
		c.signIn(e)
	}
	_, list := c.do("GET", "/api/reports", "EMP-1002", "HR Admin", nil)
	reps, _ := list["reports"].([]any)
	if len(reps) < 30 {
		t.Fatalf("%d reports", len(reps))
	}
	for _, x := range reps {
		id := x.(map[string]any)["id"].(string)
		if s, r := c.do("GET", "/api/reports/"+id, "EMP-1002", "HR Admin", nil); s != 200 {
			t.Errorf("report %s: %d %v", id, s, r["error"])
		}
	}
	// Scope: an HRBP sees only mapped properties.
	_, all := c.do("GET", "/api/reports/emp_master", "EMP-1002", "HR Admin", nil)
	_, mine := c.do("GET", "/api/reports/emp_master", "EMP-1004", "HRBP", nil)
	allowed := map[string]bool{}
	if m := apiDoc(st, "user_role_map", bson.M{"employee_id": "EMP-1004", "role_name": "HRBP"}); m != nil {
		emps, _ := st.DB.C("employee").Find(ctx, bson.M{"location_id": bson.M{"$in": m["location_ids"]}})
		for _, e := range emps {
			allowed[e["id"].(string)] = true
		}
	}
	if mine["total"].(float64) >= all["total"].(float64) {
		t.Fatal("the HRBP sees fewer people than HR Admin")
	}
	for _, row := range mine["rows"].([]any) {
		if id := row.([]any)[0].(string); !allowed[id] {
			t.Fatalf("scope leak: %s", id)
		}
	}
	get := func(path, emp, role string) int {
		req, _ := http.NewRequest("GET", c.base+path, nil)
		req.Header.Set("Authorization", "Bearer "+c.tokens[emp])
		req.Header.Set("X-Role", role)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if s := get("/api/reports/emp_master/sheet", "EMP-1008", "Property Manager"); s != 403 {
		t.Fatalf("Property Manager views but does not download reports: %d", s)
	}
	if s := get("/api/reports/emp_master/sheet", "EMP-1004", "HRBP"); s != 200 {
		t.Fatalf("HRBP downloads: %d", s)
	}
	if s, _ := c.do("GET", "/api/reports/register", "EMP-1004", "HRBP", nil); s != 404 {
		t.Fatal("payroll reports need payroll rights")
	}
	if s, _ := c.do("GET", "/api/reports", "EMP-1020", "Employee", nil); s != 403 {
		t.Fatal("employees have no reports")
	}
	if s, _ := c.do("GET", "/api/dashboard?tab=pay", "EMP-1011", "Payroll", nil); s != 200 {
		t.Fatal("payroll dashboard")
	}
	if s, _ := c.do("GET", "/api/dashboard?tab=pay", "EMP-1020", "Employee", nil); s != 403 {
		t.Fatal("employees have no payroll dashboard")
	}

	_, dev := c.do("POST", "/api/devices", "EMP-1002", "HR Admin", map[string]any{"name": "Sector 29 front desk", "vendor": "eSSL"})
	key, _ := dev["key"].(string)
	if len(key) < 32 {
		t.Fatal(dev)
	}
	push := func(k string, body any) (int, map[string]any) {
		var buf bytes.Buffer
		_ = json.NewEncoder(&buf).Encode(body)
		req, _ := http.NewRequest("POST", c.base+"/api/punches", &buf)
		req.Header.Set("X-Device-Key", k)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	day := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	body := map[string]any{"punches": []any{map[string]any{"employeeId": "emp-1020", "time": day + " 09:02", "direction": "IN"},
		map[string]any{"employeeId": "EMP-1020", "time": day + " 18:05", "direction": "OUT"}, map[string]any{"employeeId": "EMP-9999", "time": day + " 09:00"}}}
	if s, _ := push("wrong", body); s != 401 {
		t.Fatal("a wrong key is refused")
	}
	s, r := push(key, body)
	if s != 200 || r["accepted"] != 2.0 || len(r["rejected"].([]any)) != 1 {
		t.Fatal(s, r)
	}
	a := apiDoc(st, "attendance_day", bson.M{"employee_id": "EMP-1020", "date": day})
	in, _ := a["in_time"].(string)
	out, _ := a["out_time"].(string)
	src, _ := a["source"].(string)
	if in != "09:02" || out != "18:05" || src != "Punch machine" {
		t.Fatalf("day %s %s %s", in, out, src)
	}
	if _, r := push(key, body); r["accepted"] != 0.0 {
		t.Fatal("a resent punch counts once")
	}
}
