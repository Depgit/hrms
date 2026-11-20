package mdm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// The nightly reconciliation fixes a change the watcher missed, and drift against the CMS's property list.
func TestReconcile(t *testing.T) {
	svc, st, ctx := testEnv(t, Options{})
	o := seedOrg(t, ctx, st)
	addEmployee(t, ctx, st, o, "EMP-9001", "Anil", nil)
	addEmployee(t, ctx, st, o, "EMP-9002", "Bina", nil)
	run, err := svc.Reconcile(ctx, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if run["employeesChecked"] != int64(2) || run["employeesFixed"] != int64(2) || run["propertiesChecked"] != int64(0) || len(run["errors"].([]any)) != 0 {
		t.Fatalf("first run (nothing published yet) %v", run)
	}
	// A change nobody published (no watcher running): the next run publishes it, once.
	setEmp(t, ctx, st, "EMP-9002", bson.M{"grade": "G2", "designation_id": o.desig2, "department_id": o.dept2})
	run, err = svc.Reconcile(ctx, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if run["checked"] != int64(2) || run["fixed"] != int64(1) || run["finishedAt"] == nil {
		t.Fatalf("second run %v", run)
	}
	rows := outbox(t, ctx, st)
	if got := summarize(rows[2:]); !reflect.DeepEqual(got, []evSummary{{EmployeeUpdated, "EMP-9002", 2}}) {
		t.Fatalf("events %v", got)
	}
	if d := rows[2]["data"].(map[string]any); d["grade"] != "G2" || d["department"] != "Housekeeping" || d["designation"] != "Room Attendant" {
		t.Fatalf("fixed snapshot %v", d)
	}
	if run, _ := svc.Reconcile(ctx, "manual"); run["fixed"] != int64(0) {
		t.Fatalf("third run fixed %v", run["fixed"])
	}

	// The CMS side: GET {base}/properties, paged, with the API key.
	var auth []string
	cms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		if r.URL.Path != "/v1/properties" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"properties": []map[string]any{
				{"property_id": "P-1", "name": "Saket Grand", "city": "New Delhi", "state": "Delhi", "status": "Active", "updated_at": "2026-11-10T08:00:00Z"},
			}, "next_cursor": "page2"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"properties": []map[string]any{
			{"property_id": "P-2", "name": "Noida Sixty Two", "city": "Noida", "state": "Uttar Pradesh", "status": "Inactive"},
			{"property_id": "", "name": "Broken", "status": "Active"},
		}, "next_cursor": nil})
	}))
	defer cms.Close()
	base, key := cms.URL+"/v1/", "cms-key-123456"
	if _, err := svc.SaveSettings(ctx, "", SettingsInput{CMSBaseURL: &base, CMSAPIKey: &key}); err != nil {
		t.Fatal(err)
	}
	// Local state that drifted: P-1 has an old name, P-2 is unknown, P-9 is not in the CMS's list any more.
	for _, p := range []bson.M{
		{"property_id": "P-1", "name": "Saket", "status": "Active", "source": "event"},
		{"property_id": "P-9", "name": "Gone", "status": "Active", "source": "event"},
	} {
		if _, err := st.DB.C(collProperty).Insert(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB.C("location").Update(ctx, bson.M{"_id": o.loc}, bson.M{"$set": bson.M{"property_id": "P-1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.C("location").Update(ctx, bson.M{"_id": o.loc2}, bson.M{"$set": bson.M{"property_id": "P-2"}}); err != nil {
		t.Fatal(err)
	}
	run, err = svc.Reconcile(ctx, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if run["propertiesChecked"] != int64(2) || run["propertiesFixed"] != int64(2) {
		t.Fatalf("CMS run %v", run)
	}
	errs := strings.Join(func() []string {
		var l []string
		for _, e := range run["errors"].([]any) {
			l = append(l, e.(string))
		}
		return l
	}(), " | ")
	if !strings.Contains(errs, "property_id, name and status") || !strings.Contains(errs, "P-9 is in HRMS but not in the CMS") {
		t.Fatalf("errors: %s", errs)
	}
	if len(auth) != 2 || auth[0] != "Bearer "+key {
		t.Fatalf("authorization sent: %v", auth)
	}
	if p := property(t, ctx, st, "P-1"); str(p["name"]) != "Saket Grand" || str(p["source"]) != "reconcile" {
		t.Fatalf("P-1 %v", p)
	}
	if p := property(t, ctx, st, "P-2"); str(p["status"]) != "Inactive" || str(p["state"]) != "Uttar Pradesh" {
		t.Fatalf("P-2 %v", p)
	}
	l1, _ := st.DB.C("location").FindOne(ctx, bson.M{"_id": o.loc})
	l2, _ := st.DB.C("location").FindOne(ctx, bson.M{"_id": o.loc2})
	if str(l1["name"]) != "Saket Grand" || str(l2["name"]) != "Noida Sixty Two" || str(l2["status"]) != "Inactive" || str(l2["inactive_reason"]) != CMSRetireReason {
		t.Fatalf("locations %v / %v", l1, l2)
	}
	// The employees at the renamed location are published again (the location's name is in their snapshot).
	run, _ = svc.Reconcile(ctx, "manual")
	if run["employeesFixed"] != int64(2) || run["propertiesFixed"] != int64(0) {
		t.Fatalf("after the CMS fixes %v", run)
	}
	runs, err := svc.Runs(ctx, 10)
	if err != nil || len(runs) != 5 {
		t.Fatalf("runs %d %v", len(runs), err)
	}

	// A CMS that fails: the run records it.
	bad := cms.URL + "/nowhere"
	_, _ = svc.SaveSettings(ctx, "", SettingsInput{CMSBaseURL: &bad})
	run, _ = svc.Reconcile(ctx, "manual")
	if errs := run["errors"].([]any); len(errs) != 1 || !strings.Contains(errs[0].(string), "HTTP 404") {
		t.Fatalf("a failing CMS: %v", errs)
	}
}

// The nightly run: once per business day, after 02:00 business time, one server.
func TestReconcileIfDue(t *testing.T) {
	var now time.Time
	svc, st, ctx := testEnv(t, Options{Now: func() time.Time { return now }})
	o := seedOrg(t, ctx, st)
	addEmployee(t, ctx, st, o, "EMP-9101", "Night", nil)
	day := time.Date(2026, 11, 12, 0, 0, 0, 0, store.Loc())
	now = day.Add(time.Hour + 59*time.Minute)
	if ran, err := svc.ReconcileIfDue(ctx); ran || err != nil {
		t.Fatalf("at 01:59: %v %v", ran, err)
	}
	now = day.Add(2*time.Hour + 5*time.Minute)
	other := newSvc(openStore(t), Options{Now: func() time.Time { return now }})
	if ran, err := svc.ReconcileIfDue(ctx); !ran || err != nil {
		t.Fatalf("at 02:05: %v %v", ran, err)
	}
	if ran, _ := other.ReconcileIfDue(ctx); ran {
		t.Fatal("ran twice the same night")
	}
	now = day.Add(23 * time.Hour)
	if ran, _ := svc.ReconcileIfDue(ctx); ran {
		t.Fatal("ran twice the same day")
	}
	now = day.Add(24*time.Hour + 2*time.Hour + time.Minute)
	if ran, _ := other.ReconcileIfDue(ctx); !ran {
		t.Fatal("did not run the next night")
	}
	runs, _ := svc.Runs(ctx, 10)
	if len(runs) != 2 || runs[0]["trigger"] != "nightly" {
		t.Fatalf("runs %v", runs)
	}
	if n := len(outbox(t, ctx, st)); n != 1 {
		t.Fatalf("events %d", n)
	}
}
