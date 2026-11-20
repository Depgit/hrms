package mdm

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func feedServer(svc *Service) *httptest.Server {
	m := http.NewServeMux()
	m.Handle("GET /api/mdm/events", svc.EventsHandler())
	m.Handle("GET /api/mdm/employees", svc.EmployeesHandler())
	m.Handle("GET /api/mdm/employees/summary", svc.SummaryHandler())
	m.Handle("POST /api/mdm/cms/events", svc.CMSHandler())
	return httptest.NewServer(m)
}

func get(t *testing.T, url, key string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if out != nil && res.StatusCode == 200 {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s: %v (%s)", url, err, b)
		}
	}
	return res.StatusCode
}

// The pull feed: API key auth, paging by position; snapshots paging by code, summary checksum.
func TestPullFeed(t *testing.T) {
	svc, st, ctx := testEnv(t, Options{})
	o := seedOrg(t, ctx, st)
	codes := []string{"EMP-7001", "EMP-7002", "EMP-7003", "EMP-7004", "EMP-7005"}
	for _, c := range codes {
		addEmployee(t, ctx, st, o, c, "Person "+c, nil)
	}
	if _, err := svc.Publish(ctx, codes, nil); err != nil {
		t.Fatal(err)
	}
	setEmp(t, ctx, st, "EMP-7002", bson.M{"name": "Changed"})
	if _, err := svc.Publish(ctx, codes, nil); err != nil {
		t.Fatal(err)
	}
	srv := feedServer(svc)
	defer srv.Close()
	view, key, err := svc.AddKey(ctx, "", "Moneyball")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, prefixFeedKey) || view["keyLast4"] != last4(key) {
		t.Fatalf("key %q view %v", key, view)
	}
	if k, _ := st.DB.C(collKey).FindOne(ctx, bson.M{"_id": view["id"]}); str(k["key_hash"]) != keyHash(key) || strings.Contains(str(k["key_hash"]), key) {
		t.Fatal("the key must be stored as its hash only")
	}

	// Auth.
	if s := get(t, srv.URL+"/api/mdm/events", "", nil); s != 401 {
		t.Fatalf("no key: %d", s)
	}
	if s := get(t, srv.URL+"/api/mdm/events", key+"x", nil); s != 401 {
		t.Fatalf("wrong key: %d", s)
	}
	if s := get(t, srv.URL+"/api/mdm/events?limit=0", key, nil); s != 400 {
		t.Fatalf("bad limit: %d", s)
	}

	// Paging the events.
	var all []Event
	after := int64(0)
	pages := 0
	for {
		var p EventsPage
		if s := get(t, srv.URL+"/api/mdm/events?after="+strconv.FormatInt(after, 10)+"&limit=2", key, &p); s != 200 {
			t.Fatalf("page: %d", s)
		}
		pages++
		all = append(all, p.Events...)
		if p.Head != 6 {
			t.Fatalf("head %d", p.Head)
		}
		if !p.HasMore {
			break
		}
		after = p.NextAfter
	}
	if pages != 3 || len(all) != 6 {
		t.Fatalf("%d pages, %d events", pages, len(all))
	}
	for i, e := range all {
		if e.Pos != int64(i+1) || e.SchemaVersion != "v1" || e.Source != "HRMS" || e.EventID == "" {
			t.Fatalf("event %d: %+v", i, e)
		}
	}
	if all[5].EventType != EmployeeUpdated || all[5].Data["employee_code"] != "EMP-7002" || all[5].Sequence != 2 {
		t.Fatalf("last event %+v", all[5])
	}
	var tail EventsPage
	get(t, srv.URL+"/api/mdm/events?after=6", key, &tail)
	if len(tail.Events) != 0 || tail.NextAfter != 6 || tail.HasMore {
		t.Fatalf("at the head: %+v", tail)
	}

	// Snapshots and summary.
	var snaps []PublishedEmployee
	afterCode := ""
	for {
		var p EmployeesPage
		if s := get(t, srv.URL+"/api/mdm/employees?limit=2&after="+afterCode, key, &p); s != 200 {
			t.Fatalf("snapshots: %d", s)
		}
		snaps = append(snaps, p.Employees...)
		if p.NextAfter == nil {
			if p.HasMore {
				t.Fatal("has_more without next_after")
			}
			break
		}
		afterCode = *p.NextAfter
	}
	if len(snaps) != 5 {
		t.Fatalf("%d snapshots", len(snaps))
	}
	var pairs [][2]string
	for i, e := range snaps {
		if e.EmployeeCode != codes[i] || e.Hash != Hash(e.Data) {
			t.Fatalf("snapshot %d: %+v", i, e)
		}
		pairs = append(pairs, [2]string{e.EmployeeCode, strconv.FormatInt(e.Sequence, 10)})
	}
	var sum Summary
	if s := get(t, srv.URL+"/api/mdm/employees/summary", key, &sum); s != 200 {
		t.Fatalf("summary: %d", s)
	}
	if sum.Count != 5 || sum.Head != 6 || sum.Checksum != Checksum(pairs) {
		t.Fatalf("summary %+v, checksum of the pages %s", sum, Checksum(pairs))
	}

	// A paused key, a rotated key, a deleted key.
	if _, err := svc.SetKeyActive(ctx, str(view["id"]), false); err != nil {
		t.Fatal(err)
	}
	if s := get(t, srv.URL+"/api/mdm/events", key, nil); s != 401 {
		t.Fatalf("paused key: %d", s)
	}
	_, _ = svc.SetKeyActive(ctx, str(view["id"]), true)
	_, key2, err := svc.RotateKey(ctx, str(view["id"]))
	if err != nil {
		t.Fatal(err)
	}
	if s := get(t, srv.URL+"/api/mdm/events", key, nil); s != 401 {
		t.Fatalf("old key after rotating: %d", s)
	}
	if s := get(t, srv.URL+"/api/mdm/employees/summary", key2, nil); s != 200 {
		t.Fatalf("new key: %d", s)
	}
	if k, _ := st.DB.C(collKey).FindOne(ctx, bson.M{"_id": view["id"]}); k["last_used_at"] == nil {
		t.Fatal("last use not recorded")
	}
	if err := svc.DeleteKey(ctx, str(view["id"])); err != nil {
		t.Fatal(err)
	}
	if s := get(t, srv.URL+"/api/mdm/events", key2, nil); s != 401 {
		t.Fatalf("deleted key: %d", s)
	}
}
