package api_test

// The masters lock of the master data SOP (SOP-ARCH-MDM-01): CMS is the source of truth for property_id, and HRMS
// locations link to CMS properties (internal/mdm, masters.go, master_rules.go, master_book.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/mdm"
)

const lockAdmin, lockRole = "EMP-1002", "HR Admin"

// cmsSecret sets up the CMS connection's inbound secret and returns it.
func cmsSecret(t *testing.T, c *client) string {
	t.Helper()
	s, r := c.do("POST", "/api/mdm/settings/cms/secret", lockAdmin, lockRole, nil)
	sec, _ := r["secret"].(string)
	if s != 200 || sec == "" {
		t.Fatalf("inbound secret: %d %v", s, r)
	}
	return sec
}

// cmsSend posts one CMS property event, signed with secret (sig overrides the signature when not "").
func cmsSend(t *testing.T, c *client, secret, sig string, ev map[string]any) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(ev)
	ts := time.Now().Unix()
	if sig == "" {
		sig = mdm.Sign(secret, ts, body)
	}
	req, _ := http.NewRequest("POST", c.base+"/api/mdm/cms/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CMS-Timestamp", fmt.Sprint(ts))
	req.Header.Set("X-CMS-Signature", sig)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

// cmsEvent is a v1 property event.
func cmsEvent(id, typ string, seq int64, pid, name, status string) map[string]any {
	d := map[string]any{"property_id": pid, "name": name, "city": "New Delhi", "state": "Delhi"}
	if status != "" {
		d["status"] = status
	}
	return map[string]any{"event_id": id, "event_type": typ, "schema_version": "v1", "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"source": "CMS", "sequence": seq, "data": d}
}

func cmsApplied(t *testing.T, what string, s int, r map[string]any) {
	t.Helper()
	if s != 200 || r["status"] != "applied" {
		t.Fatalf("%s: %d %v", what, s, r)
	}
}

func lockList(c *client) map[string]map[string]any {
	_, l := c.do("GET", "/api/masters/location", lockAdmin, lockRole, nil)
	out := map[string]map[string]any{}
	items, _ := l["list"].([]any)
	for _, x := range items {
		m := x.(map[string]any)
		out[m["id"].(string)] = m
	}
	return out
}

func TestMDMLocationLock(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	db := mbDB{t, ctx, st}
	c.signIn(lockAdmin)
	cluster := db.id("cluster", bson.M{"status": "Active"})
	loc := func(id string) map[string]any { return apiDoc(st, "location", bson.M{"_id": id}) }

	var plain string
	t.Run("switch off: locations behave as before (create, rename, retire, reactivate)", func(t *testing.T) {
		s, r := c.do("POST", "/api/masters/location", lockAdmin, lockRole, map[string]any{"name": "Lock Test Place", "cluster_id": cluster})
		if s != 200 || r["name"] != "Lock Test Place" || r["property_id"] != nil {
			t.Fatalf("create: %d %v", s, r)
		}
		plain = r["id"].(string)
		// property_id given empty is the same as not given
		s, r = c.do("POST", "/api/masters/location", lockAdmin, lockRole, map[string]any{"name": "Lock Test Place 2", "cluster_id": cluster, "property_id": " "})
		if s != 200 || r["property_id"] != nil {
			t.Fatalf("create with a blank property: %d %v", s, r)
		}
		if s, r := c.do("PATCH", "/api/masters/location/"+plain, lockAdmin, lockRole, map[string]any{"name": "Lock Test Renamed"}); s != 200 || r["name"] != "Lock Test Renamed" {
			t.Fatalf("rename: %d %v", s, r)
		}
		if s, r := c.do("POST", "/api/masters/location/"+plain+"/inactive", lockAdmin, lockRole, map[string]any{"since": today(), "reason": "Discontinued"}); s != 200 || r["status"] != "Inactive" {
			t.Fatalf("retire: %d %v", s, r)
		}
		if s, r := c.do("POST", "/api/masters/location/"+plain+"/reactivate", lockAdmin, lockRole, nil); s != 200 || r["status"] != "Active" {
			t.Fatalf("reactivate: %d %v", s, r)
		}
		if x := lockList(c)[plain]; x == nil || x["cmsLocked"] != nil {
			t.Fatalf("list row: %v", x)
		}
	})

	sec := cmsSecret(t, c)
	t.Run("signed CMS events fill the local copy; a bad signature or a replay is refused", func(t *testing.T) {
		s, r := cmsSend(t, c, sec, "", cmsEvent("ev-1", mdm.PropertyCreated, 1, "P-LOCK-1", "CMS Saket", ""))
		cmsApplied(t, "create P-LOCK-1", s, r)
		s, r = cmsSend(t, c, sec, "", cmsEvent("ev-2", mdm.PropertyCreated, 1, "P-LOCK-2", "CMS Noida", ""))
		cmsApplied(t, "create P-LOCK-2", s, r)
		if s, r := cmsSend(t, c, sec, "sha256=00", cmsEvent("ev-3", mdm.PropertyCreated, 1, "P-LOCK-3", "Forged", "")); s != 401 {
			t.Fatalf("bad signature: %d %v", s, r)
		}
		if s, r := cmsSend(t, c, sec, "", cmsEvent("ev-1", mdm.PropertyCreated, 1, "P-LOCK-1", "CMS Saket", "")); s != 200 || r["status"] != "duplicate" {
			t.Fatalf("replay: %d %v", s, r)
		}
		if apiCount(st, "mdm_property", bson.M{"property_id": "P-LOCK-3"}) != 0 {
			t.Fatal("a forged event was applied")
		}
	})

	t.Run("a property_id is set once (Link) and cannot change", func(t *testing.T) {
		if s, r := c.do("PATCH", "/api/masters/location/"+plain, lockAdmin, lockRole, map[string]any{"property_id": "P-LOCK-1"}); s != 400 || !strings.Contains(fmt.Sprint(r["error"], r["message"]), "Link to CMS property") {
			t.Fatalf("set through Edit: %d %v", s, r)
		}
		s, r := c.do("POST", "/api/mdm/locations/"+plain+"/link", lockAdmin, lockRole, map[string]any{"propertyId": "P-LOCK-1"})
		if s != 200 {
			t.Fatalf("link: %d %v", s, r)
		}
		if l := loc(plain); l["property_id"] != "P-LOCK-1" || l["name"] != "CMS Saket" {
			t.Fatalf("linked location: %v", l)
		}
		if s, r := c.do("POST", "/api/mdm/locations/"+plain+"/link", lockAdmin, lockRole, map[string]any{"propertyId": "P-LOCK-2"}); s != 409 {
			t.Fatalf("relink: %d %v", s, r)
		}
		if s, r := c.do("PATCH", "/api/masters/location/"+plain, lockAdmin, lockRole, map[string]any{"property_id": "P-LOCK-2"}); s != 400 || !strings.Contains(fmt.Sprint(r), "set once") {
			t.Fatalf("change through Edit: %d %v", s, r)
		}
		if s, r := c.do("PATCH", "/api/masters/location/"+plain, lockAdmin, lockRole, map[string]any{"property_id": "P-LOCK-1"}); s != 200 {
			t.Fatalf("the same property_id is no change: %d %v", s, r)
		}
		other := db.id("location", bson.M{"name": "Lock Test Place 2"})
		if s, r := c.do("POST", "/api/mdm/locations/"+other+"/link", lockAdmin, lockRole, map[string]any{"propertyId": "P-LOCK-1"}); s != 409 {
			t.Fatalf("a property linked twice: %d %v", s, r)
		}
		if l := loc(plain); l["property_id"] != "P-LOCK-1" {
			t.Fatalf("property_id changed: %v", l)
		}
	})

	t.Run("a linked location is not renamed in HRMS; a signed PROPERTY_UPDATED renames it", func(t *testing.T) {
		if s, r := c.do("PATCH", "/api/masters/location/"+plain, lockAdmin, lockRole, map[string]any{"name": "My own name"}); s != 400 || !strings.Contains(fmt.Sprint(r), "Change the name in the CMS") {
			t.Fatalf("rename in HRMS: %d %v", s, r)
		}
		if s, r := c.do("PATCH", "/api/masters/location/"+plain, lockAdmin, lockRole, map[string]any{"headcount_budget": 12}); s != 200 {
			t.Fatalf("other fields still change: %d %v", s, r)
		}
		x := lockList(c)[plain]
		if why, _ := x["cmsLocked"].(string); !strings.Contains(why, "P-LOCK-1") {
			t.Fatalf("list row not locked: %v", x)
		}
		s, r := cmsSend(t, c, sec, "", cmsEvent("ev-4", mdm.PropertyUpdated, 2, "P-LOCK-1", "CMS Saket Grand", ""))
		cmsApplied(t, "rename", s, r)
		if l := loc(plain); l["name"] != "CMS Saket Grand" {
			t.Fatalf("not renamed by the CMS: %v", l)
		}
		if s, r := cmsSend(t, c, sec, "", cmsEvent("ev-5", mdm.PropertyUpdated, 1, "P-LOCK-1", "Stale name", "")); s != 200 || r["status"] != "ignored" {
			t.Fatalf("an older event: %d %v", s, r)
		}
		if l := loc(plain); l["name"] != "CMS Saket Grand" {
			t.Fatalf("an older event renamed it: %v", l)
		}
	})

	t.Run("switch on: a new location needs a linkable CMS property", func(t *testing.T) {
		if s, r := c.do("PUT", "/api/mdm/settings/cms", lockAdmin, lockRole, map[string]any{"propertiesFromCms": true}); s != 200 {
			t.Fatalf("switch on: %d %v", s, r)
		}
		n := apiCount(st, "location", nil)
		if s, r := c.do("POST", "/api/masters/location", lockAdmin, lockRole, map[string]any{"name": "No property", "cluster_id": cluster}); s != 400 || !strings.Contains(fmt.Sprint(r), "Properties come from the CMS") {
			t.Fatalf("create without a property: %d %v", s, r)
		}
		if s, r := c.do("POST", "/api/masters/location", lockAdmin, lockRole, map[string]any{"name": "Unknown", "cluster_id": cluster, "property_id": "P-NOPE"}); s != 400 {
			t.Fatalf("an unknown property: %d %v", s, r)
		}
		if s, r := c.do("POST", "/api/masters/location", lockAdmin, lockRole, map[string]any{"name": "Taken", "cluster_id": cluster, "property_id": "P-LOCK-1"}); s != 409 {
			t.Fatalf("a linked property: %d %v", s, r)
		}
		if apiCount(st, "location", nil) != n {
			t.Fatal("a refused location was written")
		}
		s, r := c.do("POST", "/api/masters/location", lockAdmin, lockRole, map[string]any{"name": "typed name", "cluster_id": cluster, "property_id": "P-LOCK-2"})
		if s != 200 || r["property_id"] != "P-LOCK-2" || r["name"] != "CMS Noida" {
			t.Fatalf("create from a CMS property: %d %v", s, r)
		}
		if s, r := c.do("PUT", "/api/mdm/settings/cms", lockAdmin, lockRole, map[string]any{"propertiesFromCms": false}); s != 200 {
			t.Fatalf("switch off: %d %v", s, r)
		}
	})

	t.Run("PROPERTY_DEACTIVATED retires the location; it is reactivated from the CMS only", func(t *testing.T) {
		s, r := cmsSend(t, c, sec, "", map[string]any{"event_id": "ev-6", "event_type": mdm.PropertyDeactivated, "schema_version": "v1",
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano), "source": "CMS", "sequence": 3, "data": map[string]any{"property_id": "P-LOCK-1"}})
		cmsApplied(t, "deactivate", s, r)
		l := loc(plain)
		if l["status"] != "Inactive" || l["inactive_since"] != today() || l["inactive_reason"] != mdm.CMSRetireReason || l["name"] != "CMS Saket Grand" {
			t.Fatalf("not retired: %v", l)
		}
		if s, r := c.do("POST", "/api/masters/location/"+plain+"/reactivate", lockAdmin, lockRole, nil); s != 400 || !strings.Contains(fmt.Sprint(r), "Reactivate the property in the CMS") {
			t.Fatalf("reactivate in HRMS: %d %v", s, r)
		}
		s, r = cmsSend(t, c, sec, "", cmsEvent("ev-7", mdm.PropertyUpdated, 4, "P-LOCK-1", "CMS Saket Grand", "Active"))
		cmsApplied(t, "reactivate", s, r)
		if l := loc(plain); l["status"] != "Active" || l["inactive_since"] != nil {
			t.Fatalf("not reactivated: %v", l)
		}
	})

	t.Run("Master update: a read-only CMS property column, ignored on upload", func(t *testing.T) {
		s, b := c.getRaw("/api/master-update/download?data=1", lockAdmin, lockRole)
		if s != 200 {
			t.Fatalf("download: %d", s)
		}
		f := mbOpen(t, b)
		defer f.Close()
		rows, _ := f.GetRows("Locations")
		if strings.Join(rows[0], "|") != "Name|Cluster|CMS property (read-only)|Status|Inactive from|Inactive reason|Row ID (leave as is; blank for a new item)" {
			t.Fatalf("Locations header %v", rows[0])
		}
		row := -1
		for i, x := range rows {
			if len(x) > 6 && x[6] == plain {
				row = i
			}
		}
		if row < 0 || rows[row][2] != "P-LOCK-1" || rows[row][0] != "CMS Saket Grand" {
			t.Fatalf("the linked row: %v", rows)
		}
		if r := c.mbRun(t, "check", lockAdmin, lockRole, b); r["changes"] != float64(0) || r["errors"] != float64(0) {
			t.Fatalf("unchanged upload: %v", r)
		}
		_ = f.SetCellValue("Locations", fmt.Sprintf("C%d", row+1), "P-LOCK-2")
		r := c.mbRun(t, "apply", lockAdmin, lockRole, mbBytes(t, f))
		if r["changes"] != float64(0) || r["errors"] != float64(0) {
			t.Fatalf("an edited CMS property column: %v", r)
		}
		if l := loc(plain); l["property_id"] != "P-LOCK-1" {
			t.Fatalf("property_id changed by the workbook: %v", l)
		}
		// a rename of the linked location in the workbook is refused like on the Masters screen, and writes nothing
		_ = f.SetCellValue("Locations", fmt.Sprintf("A%d", row+1), "Workbook name")
		r = c.mbRun(t, "apply", lockAdmin, lockRole, mbBytes(t, f))
		got := mbRowsOf(mbSheetOf(t, r, "Locations"))
		if r["errors"] != float64(1) || len(got) != 1 || !strings.Contains(mbText(got[0]["errors"]), "Change the name in the CMS") {
			t.Fatalf("rename in the workbook: %v", r)
		}
		if l := loc(plain); l["name"] != "CMS Saket Grand" {
			t.Fatalf("renamed by the workbook: %v", l)
		}
	})
}

// feedEvents reads the pull feed after pos.
func feedEvents(t *testing.T, c *client, key string, after int64) ([]map[string]any, int64) {
	t.Helper()
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/mdm/events?after=%d&limit=1000", c.base, after), nil)
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var p struct {
		Events []map[string]any `json:"events"`
		Head   int64            `json:"head"`
	}
	if res.StatusCode != 200 || json.NewDecoder(res.Body).Decode(&p) != nil {
		t.Fatalf("feed: %d", res.StatusCode)
	}
	return p.Events, p.Head
}

func eventsOf(evs []map[string]any, code string) []map[string]any {
	out := []map[string]any{}
	for _, e := range evs {
		if d, _ := e["data"].(map[string]any); d["employee_code"] == code {
			out = append(out, e)
		}
	}
	return out
}

func TestMDMEmployeeEvents(t *testing.T) {
	c, app, st := setupFull(t)
	ctx := context.Background()
	c.signIn(lockAdmin)
	const emp = "EMP-1020"
	e := apiDoc(st, "employee", bson.M{"_id": emp})
	locID := e["location_id"].(string)

	sec := cmsSecret(t, c)
	s, r := cmsSend(t, c, sec, "", cmsEvent("ev-e1", mdm.PropertyCreated, 1, "P-EMP-1", "CMS Hub", ""))
	cmsApplied(t, "create", s, r)
	if s, r := c.do("POST", "/api/mdm/locations/"+locID+"/link", lockAdmin, lockRole, map[string]any{"propertyId": "P-EMP-1"}); s != 200 {
		t.Fatalf("link: %d %v", s, r)
	}
	s, r = c.do("POST", "/api/mdm/keys", lockAdmin, lockRole, map[string]any{"name": "PMS"})
	key, _ := r["secret"].(string)
	if s != 200 || key == "" {
		t.Fatalf("feed key: %d %v", s, r)
	}

	jctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := app.StartJobs(jctx); err != nil {
		t.Fatal(err)
	}
	defer app.StopJobs(ctx)

	wait := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				st2, _ := st.DB.C("mdm_state").Find(ctx, nil)
				t.Fatalf("timed out waiting for %s (outbox %d, state %v)", what, apiCount(st, "mdm_outbox", nil), st2)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	// The watcher's first scan publishes every employee.
	var head int64
	wait("the first publication", func() bool {
		evs, h := feedEvents(t, c, key, 0)
		head = h
		return len(eventsOf(evs, emp)) == 1
	})
	evs, _ := feedEvents(t, c, key, 0)
	first := eventsOf(evs, emp)[0]
	if first["event_type"] != mdm.EmployeeCreated || first["data"].(map[string]any)["property_id"] != "P-EMP-1" {
		t.Fatalf("first event: %v", first)
	}
	time.Sleep(time.Second) // let the first scan finish
	_, head = feedEvents(t, c, key, 0)

	t.Run("an employee update emits one EMPLOYEE_UPDATED in the pull feed, with the location's property_id", func(t *testing.T) {
		mgr := "EMP-1002"
		if e["manager_id"] == mgr {
			mgr = "EMP-1001"
		}
		if s, r := c.do("PATCH", "/api/employees/"+emp+"/job", lockAdmin, lockRole, map[string]any{"changes": map[string]any{"manager": mgr}, "reason": "Data correction", "effectiveFrom": today()}); s != 200 {
			t.Fatalf("update: %d %v", s, r)
		}
		wait("the update event", func() bool {
			evs, _ := feedEvents(t, c, key, head)
			return len(eventsOf(evs, emp)) > 0
		})
		time.Sleep(2 * time.Second) // nothing more comes
		evs, _ := feedEvents(t, c, key, head)
		mine := eventsOf(evs, emp)
		if len(mine) != 1 || len(evs) != 1 {
			t.Fatalf("events after the update: %v", evs)
		}
		ev := mine[0]
		data := ev["data"].(map[string]any)
		if ev["event_type"] != mdm.EmployeeUpdated || data["property_id"] != "P-EMP-1" || data["manager_code"] != mgr || data["employee_code"] != emp ||
			ev["sequence"] != float64(2) || ev["source"] != "HRMS" || ev["schema_version"] != "v1" {
			t.Fatalf("update event: %v", ev)
		}
	})

	t.Run("the employee code can never change", func(t *testing.T) {
		for _, k := range []string{"id", "employee_code", "employeeId", "code"} {
			if s, r := c.do("PATCH", "/api/employees/"+emp+"/job", lockAdmin, lockRole, map[string]any{"changes": map[string]any{k: "EMP-9999"}, "reason": "Promotion", "effectiveFrom": today()}); s != 400 {
				t.Fatalf("job change of %s: %d %v", k, s, r)
			}
			if s, r := c.do("PATCH", "/api/employees/"+emp+"/personal", lockAdmin, lockRole, map[string]any{k: "EMP-9999"}); s != 400 {
				t.Fatalf("personal change of %s: %d %v", k, s, r)
			}
		}
		if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": emp}, bson.M{"$set": bson.M{"id": "EMP-9999"}}); err == nil {
			t.Fatal("the store let the employee code change")
		}
		if apiDoc(st, "employee", bson.M{"_id": emp}) == nil || apiDoc(st, "employee", bson.M{"_id": "EMP-9999"}) != nil {
			t.Fatal("the employee code changed")
		}
		if apiCount(st, "mdm_published", bson.M{"employee_code": "EMP-9999"}) != 0 {
			t.Fatal("a new code was published")
		}
	})
}
