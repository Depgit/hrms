package mdm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

type cmsClient struct {
	t      *testing.T
	url    string
	secret string
}

func (c cmsClient) send(ts time.Time, body []byte, sig string) (int, map[string]any) {
	c.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, c.url+"/api/mdm/cms/events", bytes.NewReader(body))
	unix := ts.Unix()
	req.Header.Set("X-CMS-Timestamp", strconv.FormatInt(unix, 10))
	if sig == "" {
		sig = Sign(c.secret, unix, body)
	}
	req.Header.Set("X-CMS-Signature", sig)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	b, _ := io.ReadAll(res.Body)
	_ = json.Unmarshal(b, &out)
	return res.StatusCode, out
}

func (c cmsClient) event(id, typ, version string, at time.Time, seq *int64, data map[string]any) (int, map[string]any) {
	c.t.Helper()
	env := map[string]any{"event_id": id, "event_type": typ, "schema_version": version, "timestamp": at.UTC().Format(time.RFC3339), "source": "CMS", "data": data}
	if seq != nil {
		env["sequence"] = *seq
	}
	b, _ := json.Marshal(env)
	return c.send(time.Now(), b, "")
}

func property(t *testing.T, ctx context.Context, st *store.Store, pid string) store.Row {
	t.Helper()
	r, err := st.DB.C(collProperty).FindOne(ctx, bson.M{"property_id": pid})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func inbox(t *testing.T, ctx context.Context, st *store.Store, id string) store.Row {
	t.Helper()
	r, err := st.DB.C(collInbox).FindOne(ctx, bson.M{"event_id": id})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Inbound CMS events: signature, stale and replayed requests, idempotency, out-of-order, unknown versions.
func TestCMSInbound(t *testing.T) {
	svc, st, ctx := testEnv(t, Options{})
	srv := feedServer(svc)
	defer srv.Close()
	c := cmsClient{t: t, url: srv.URL}
	body := []byte(`{"event_id":"e0","event_type":"PROPERTY_CREATED","schema_version":"v1","timestamp":"2026-11-11T09:00:00Z","data":{"property_id":"P-1","name":"Saket"}}`)
	if s, _ := c.send(time.Now(), body, "sha256=00"); s != 503 {
		t.Fatalf("before the secret is set: %d", s)
	}
	sec, view, err := svc.RotateInboundSecret(ctx, "HR Admin (EMP-1)")
	if err != nil {
		t.Fatal(err)
	}
	c.secret = sec
	if view["inboundSecretSet"] != true || view["inboundSecretLast4"] != last4(sec) || !strings.HasPrefix(sec, prefixInboundSecret) {
		t.Fatalf("settings %v", view)
	}

	// Signature and freshness.
	if s, out := c.send(time.Now(), body, Sign("wrong", time.Now().Unix(), body)); s != 401 || !strings.Contains(str(out["error"]), "Signature") {
		t.Fatalf("wrong secret: %d %v", s, out)
	}
	tampered := bytes.Replace(body, []byte("Saket"), []byte("SakeT"), 1)
	if s, _ := c.send(time.Now(), tampered, Sign(sec, time.Now().Unix(), body)); s != 401 {
		t.Fatalf("tampered body: %d", s)
	}
	old := time.Now().Add(-6 * time.Minute)
	if s, out := c.send(old, body, ""); s != 401 || !strings.Contains(str(out["error"]), "stale") {
		t.Fatalf("stale request: %d %v", s, out)
	}
	if s, _ := c.send(time.Now().Add(6*time.Minute), body, ""); s != 401 {
		t.Fatalf("request from the future: %d", s)
	}
	if n, _ := st.DB.C(collInbox).Count(ctx, nil); n != 0 {
		t.Fatalf("refused requests reached the inbox: %d", n)
	}

	// Applied once; the replay (same event_id) is a duplicate.
	t0 := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	data := map[string]any{"property_id": "P-1", "name": "Saket Residency", "city": "New Delhi", "state": "Delhi", "status": "Active"}
	if s, out := c.event("e1", PropertyCreated, "v1", t0, nil, data); s != 200 || out["status"] != "applied" {
		t.Fatalf("created: %d %v", s, out)
	}
	if s, out := c.event("e1", PropertyCreated, "v1", t0, nil, data); s != 200 || out["status"] != "duplicate" {
		t.Fatalf("replayed: %d %v", s, out)
	}
	p := property(t, ctx, st, "P-1")
	if str(p["name"]) != "Saket Residency" || str(p["city"]) != "New Delhi" || str(p["status"]) != "Active" || str(p["last_event_id"]) != "e1" || str(p["source"]) != "event" {
		t.Fatalf("property %v", p)
	}
	if n, _ := st.DB.C(collInbox).Count(ctx, nil); n != 1 {
		t.Fatalf("inbox holds %d", n)
	}

	// Out of order: a newer update, then an older one that arrives late.
	data2 := map[string]any{"property_id": "P-1", "name": "Saket Grand", "status": "Active"}
	if s, out := c.event("e3", PropertyUpdated, "v1", t0.Add(2*time.Minute), nil, data2); s != 200 || out["status"] != "applied" {
		t.Fatalf("newer: %d %v", s, out)
	}
	data1 := map[string]any{"property_id": "P-1", "name": "Saket Old Name", "status": "Active"}
	if s, out := c.event("e2", PropertyUpdated, "v1", t0.Add(time.Minute), nil, data1); s != 200 || out["status"] != "ignored" {
		t.Fatalf("older: %d %v", s, out)
	}
	if p := property(t, ctx, st, "P-1"); str(p["name"]) != "Saket Grand" {
		t.Fatalf("an older event overwrote a newer one: %v", p["name"])
	}
	if r := inbox(t, ctx, st, "e2"); str(r["status"]) != "ignored" || !strings.Contains(str(r["reason"]), "Older") {
		t.Fatalf("inbox e2 %v", r)
	}
	// By sequence when both carry one.
	if s, out := c.event("s5", PropertyUpdated, "v1", t0.Add(3*time.Minute), ptr(int64(5)), map[string]any{"property_id": "P-1", "name": "Seq Five"}); s != 200 || out["status"] != "applied" {
		t.Fatalf("seq 5: %d %v", s, out)
	}
	if s, out := c.event("s4", PropertyUpdated, "v1", t0.Add(4*time.Minute), ptr(int64(4)), map[string]any{"property_id": "P-1", "name": "Seq Four"}); s != 200 || out["status"] != "ignored" {
		t.Fatalf("seq 4 after 5: %d %v", s, out)
	}

	// Unknown schema version: refused and kept as rejected; bad data likewise.
	if s, out := c.event("e9", PropertyUpdated, "v9", t0.Add(5*time.Minute), nil, data2); s != 422 || !strings.Contains(str(out["error"]), "v9") {
		t.Fatalf("unknown version: %d %v", s, out)
	}
	if r := inbox(t, ctx, st, "e9"); str(r["status"]) != "rejected" || str(r["property_id"]) != "P-1" || r["payload"] == nil {
		t.Fatalf("inbox e9 %v", r)
	}
	if s, _ := c.event("e10", PropertyCreated, "v1", t0, nil, map[string]any{"name": "No id"}); s != 422 {
		t.Fatalf("no property_id: %d", s)
	}
	if s, _ := c.event("e11", "PROPERTY_DELETED", "v1", t0, nil, data); s != 400 {
		t.Fatalf("unknown type: %d", s)
	}
	// A rejected event sent again in a version HRMS reads is evaluated again.
	if s, out := c.event("e9", PropertyUpdated, "v1", t0.Add(5*time.Minute), nil, data2); s != 200 || out["status"] != "applied" {
		t.Fatalf("rejected, then resent as v1: %d %v", s, out)
	}

	// Two copies of a new event at once: one applies, the other is a duplicate.
	var wg sync.WaitGroup
	results := make([]string, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, out := c.event("race", PropertyCreated, "v1", t0, nil, map[string]any{"property_id": "P-2", "name": "Noida 62"})
			results[i] = str(out["status"])
		}(i)
	}
	wg.Wait()
	applied := 0
	for _, r := range results {
		if r == "applied" {
			applied++
		} else if r != "duplicate" {
			t.Fatalf("concurrent copies: %v", results)
		}
	}
	if applied != 1 {
		t.Fatalf("concurrent copies applied %d times: %v", applied, results)
	}
}

// A location linked to a CMS property follows its name and status; the link is set once; the masters lock.
func TestPropertyLinkRenameDeactivate(t *testing.T) {
	svc, st, ctx := testEnv(t, Options{})
	o := seedOrg(t, ctx, st)
	addEmployee(t, ctx, st, o, "EMP-8001", "Kiran", nil)
	srv := feedServer(svc)
	defer srv.Close()
	sec, _, _ := svc.RotateInboundSecret(ctx, "")
	c := cmsClient{t: t, url: srv.URL, secret: sec}
	t0 := time.Now().Add(-time.Hour)
	if s, _ := c.event("p1", PropertyCreated, "v1", t0, nil, map[string]any{"property_id": "P-100", "name": "Saket Suites", "city": "New Delhi", "state": "Delhi"}); s != 200 {
		t.Fatal(s)
	}
	if s, _ := c.event("p2", PropertyCreated, "v1", t0, nil, map[string]any{"property_id": "P-200", "name": "Noida 62", "status": "Active"}); s != 200 {
		t.Fatal(s)
	}
	loc := func(id string) store.Row {
		r, _ := st.DB.C("location").FindOne(ctx, bson.M{"_id": id})
		return r
	}
	link := func(locID, pid string) ([]string, error) {
		var notes []string
		err := st.Tx(ctx, func(tx store.Q) error {
			var err error
			notes, err = svc.LinkLocation(ctx, tx, locID, pid, Actor{Name: "Asha Rao (EMP-1)", Role: "HR Admin"})
			return err
		})
		return notes, err
	}

	if _, err := link(o.loc, "P-999"); err == nil || !strings.Contains(err.Error(), "not known") {
		t.Fatalf("unknown property: %v", err)
	}
	if _, err := link(o.loc, "P-100"); err != nil {
		t.Fatal(err)
	}
	if l := loc(o.loc); str(l["property_id"]) != "P-100" || str(l["name"]) != "Saket Suites" {
		t.Fatalf("after linking: %v", l)
	}
	if n, _ := st.DB.C("audit_log").Count(ctx, bson.M{"entity": "location", "entity_id": o.loc, "action": bson.M{"$in": bson.A{"Linked location to CMS property", "Changed location"}}}); n != 2 {
		t.Fatalf("audit entries: %d", n)
	}
	if a, _ := st.DB.C("audit_log").FindOne(ctx, bson.M{"action": "Linked location to CMS property"}); str(a["actor"]) != "Asha Rao (EMP-1)" || str(a["actor_role"]) != "HR Admin" {
		t.Fatalf("link audited as %v / %v", a["actor"], a["actor_role"])
	}
	if _, err := link(o.loc, "P-200"); err == nil || !strings.Contains(err.Error(), "cannot change") {
		t.Fatalf("a second link: %v", err)
	}
	if _, err := link(o.loc2, "P-100"); err == nil || !strings.Contains(err.Error(), "already linked") {
		t.Fatalf("a property linked twice: %v", err)
	}
	// The other location already carries the CMS name of P-200 ("Noida 62"): linking it is a no-op rename.
	if notes, err := link(o.loc2, "P-200"); err != nil || len(notes) > 0 {
		t.Fatalf("link P-200: %v %v", notes, err)
	}
	// The unique index holds even past the app's checks.
	if _, err := st.DB.C("location").Insert(ctx, bson.M{"name": "Dup", "cluster_id": o.cluster, "property_id": "P-100"}); err == nil {
		t.Fatal("two locations with one property_id")
	}

	// Employees at the location now publish its property_id.
	if _, err := svc.Publish(ctx, []string{"EMP-8001"}, nil); err != nil {
		t.Fatal(err)
	}
	pub, _ := st.DB.C(collPublished).FindOne(ctx, bson.M{"employee_code": "EMP-8001"})
	if d := pub["data"].(map[string]any); d["property_id"] != "P-100" || d["location"] != "Saket Suites" {
		t.Fatalf("snapshot %v", d)
	}

	// Renamed in the CMS → renamed in HRMS, with the scheduled job changes that move someone there.
	if _, err := st.DB.C("employee_change").Insert(ctx, bson.M{"employee_id": "EMP-8001", "field": "Location", "old_value": "Noida 62", "new_value": "Saket Suites",
		"reason": "Transfer", "effective_from": "2099-01-01", "state": "Scheduled", "apply_column": "location_id", "apply_value": o.loc, "changed_by": "HR"}); err != nil {
		t.Fatalf("scheduled change: %v", err)
	}
	if s, out := c.event("p3", PropertyUpdated, "v1", t0.Add(time.Minute), nil, map[string]any{"property_id": "P-100", "name": "Saket Grand", "status": "Active"}); s != 200 || out["status"] != "applied" {
		t.Fatalf("rename: %d %v", s, out)
	}
	if l := loc(o.loc); str(l["name"]) != "Saket Grand" {
		t.Fatalf("not renamed: %v", l["name"])
	}
	if ch, _ := st.DB.C("employee_change").FindOne(ctx, bson.M{"apply_value": o.loc}); str(ch["new_value"]) != "Saket Grand" {
		t.Fatalf("scheduled change shows %v", ch["new_value"])
	}
	// A CMS name another location already has: the property follows, the location keeps its name, the inbox says why.
	if s, out := c.event("p4", PropertyUpdated, "v1", t0.Add(2*time.Minute), nil, map[string]any{"property_id": "P-100", "name": "noida 62"}); s != 200 || !strings.Contains(str(out["reason"]), "not renamed") {
		t.Fatalf("name clash: %d %v", s, out)
	}
	if l := loc(o.loc); str(l["name"]) != "Saket Grand" {
		t.Fatalf("renamed into a clash: %v", l["name"])
	}

	// Deactivated in the CMS → retired today with the CMS reason; active again when the CMS reactivates it.
	today := store.Today()
	if s, out := c.event("p5", PropertyDeactivated, "v1", t0.Add(3*time.Minute), nil, map[string]any{"property_id": "P-100"}); s != 200 || out["status"] != "applied" {
		t.Fatalf("deactivate: %d %v", s, out)
	}
	if l := loc(o.loc); str(l["status"]) != "Inactive" || str(l["inactive_since"]) != today || str(l["inactive_reason"]) != CMSRetireReason {
		t.Fatalf("after deactivation: %v", l)
	}
	if p := property(t, ctx, st, "P-100"); str(p["status"]) != "Inactive" || str(p["name"]) != "noida 62" {
		t.Fatalf("property after deactivation: %v", p)
	}
	if err := CheckReactivate(ctx, st.DB, loc(o.loc)); err == nil || !strings.Contains(err.Error(), "Reactivate the property in the CMS") {
		t.Fatalf("reactivating in HRMS what the CMS deactivated: %v", err)
	}
	if s, _ := c.event("p6", PropertyUpdated, "v1", t0.Add(4*time.Minute), nil, map[string]any{"property_id": "P-100", "name": "Saket Grand", "status": "Active"}); s != 200 {
		t.Fatal(s)
	}
	if l := loc(o.loc); str(l["status"]) != "Active" || l["inactive_since"] != nil || l["inactive_reason"] != nil || str(l["name"]) != "Saket Grand" {
		t.Fatalf("after reactivation: %v", l)
	}
	if err := CheckReactivate(ctx, st.DB, loc(o.loc)); err != nil {
		t.Fatal(err)
	}
	// A location HR retired for its own reason stays retired when the CMS reports the property active.
	if _, err := st.DB.C("location").Update(ctx, bson.M{"_id": o.loc2}, bson.M{"$set": bson.M{"status": "Inactive", "inactive_since": today, "inactive_reason": "Closed for renovation"}}); err != nil {
		t.Fatal(err)
	}
	_, _ = c.event("p7", PropertyUpdated, "v1", t0.Add(5*time.Minute), nil, map[string]any{"property_id": "P-200", "name": "Noida 62", "status": "Active"})
	if l := loc(o.loc2); str(l["status"]) != "Inactive" {
		t.Fatal("the CMS reactivated a location HR retired")
	}

	// The masters lock (master_rules.go checkMaster → CheckLocation).
	check := func(before store.Row, vals map[string]any) error {
		return st.Tx(ctx, func(tx store.Q) error { return CheckLocation(ctx, tx, before, vals) })
	}
	l := loc(o.loc)
	if err := check(l, map[string]any{"name": "Something Else"}); err == nil || !strings.Contains(err.Error(), "comes from the CMS") {
		t.Fatalf("renaming a linked location: %v", err)
	}
	if err := check(l, map[string]any{"name": "Saket Grand"}); err != nil {
		t.Fatalf("the CMS's own name: %v", err)
	}
	if err := check(l, map[string]any{"property_id": "P-200"}); err == nil {
		t.Fatal("changing the link through the masters")
	}
	if err := check(nil, map[string]any{"name": "Free Text"}); err != nil {
		t.Fatalf("switch off, no property: %v", err)
	}
	on := true
	if _, err := svc.SaveSettings(ctx, "", SettingsInput{PropertiesFromCMS: &on}); err != nil {
		t.Fatal(err)
	}
	if err := check(nil, map[string]any{"name": "Free Text"}); err == nil || !strings.Contains(err.Error(), "Properties come from the CMS") {
		t.Fatalf("switch on, no property: %v", err)
	}
	if s, _ := c.event("p8", PropertyCreated, "v1", t0, nil, map[string]any{"property_id": "P-300", "name": "Gurugram 29"}); s != 200 {
		t.Fatal(s)
	}
	vals := map[string]any{"name": "typed", "property_id": "P-300"}
	if err := check(nil, vals); err != nil || vals["name"] != "Gurugram 29" {
		t.Fatalf("switch on, with a property: %v %v", err, vals)
	}
	if err := check(nil, map[string]any{"name": "x", "property_id": "P-100"}); err == nil || !strings.Contains(err.Error(), "already linked") {
		t.Fatalf("a linked property for a new location: %v", err)
	}
	rows := []store.Row{loc(o.loc), {"id": "x", "property_id": nil}}
	if err := LocationFlags(ctx, st.DB, rows); err != nil || !strings.Contains(str(rows[0]["cmsLocked"]), "P-100") || rows[1]["cmsLocked"] != nil {
		t.Fatalf("flags %v %v", rows, err)
	}
	props, err := svc.CMSProperties(ctx)
	if err != nil || props["fromCms"] != true || len(props["properties"].([]map[string]any)) != 3 {
		t.Fatalf("properties %v %v", props, err)
	}
}
