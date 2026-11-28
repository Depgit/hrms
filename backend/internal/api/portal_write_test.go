package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/api"
	"github.com/saltstayz/hrms/internal/jobs"
	"github.com/saltstayz/hrms/internal/realtime"
	"github.com/saltstayz/hrms/internal/realtime/rtfake"
	"github.com/saltstayz/hrms/internal/store"
)

// ptJoined takes a new candidate to Mark joined today and returns the record's id.
func ptJoined(t *testing.T, c *client, st *store.Store, first, last, phone string) string {
	t.Helper()
	si, _ := hireAccepted(t, c, st, first, last, phone)
	s, r := c.do("POST", "/api/candidates/"+si+"/joined", "EMP-1004", "HRBP", map[string]any{"date": today()})
	if s != 200 {
		t.Fatalf("joined: %d %v", s, r)
	}
	return r["id"].(string)
}

// ptDigits6 is the digits of an id, zero-padded to 6.
func ptDigits6(id string) string {
	d := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, id)
	if len(d) < 6 {
		d = strings.Repeat("0", 6-len(d)) + d
	}
	return d
}

// ptWait polls until ok or fails after 20 seconds.
func ptWait(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(20 * time.Second); !ok(); time.Sleep(150 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func ptSync(st *store.Store, id string) map[string]any {
	e := apiDoc(st, "employee", bson.M{"_id": id})
	m, _ := e["portal_sync"].(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

// Automatic writes to the (fake) Realtime portal (user, 19 Nov 2026): Mark joined queues one create job and the
// portal gets exactly one create; a missing mapping holds it without touching the portal; a refused create is not
// tried again by itself (job, sweep) but by HR's Retry.
func TestPortalAutoCreate(t *testing.T) {
	c, app, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	ctx := context.Background()
	fp := rtfake.New(t)
	old := realtime.Spec
	realtime.Spec = rtfake.Spec
	defer func() { realtime.Spec = old }()
	undo := api.SetMachinePortal(fp.Config(), true, nil)
	defer undo()
	jctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := app.StartJobs(jctx); err != nil {
		t.Fatal(err)
	}
	defer app.StopJobs(ctx)
	jobsOf := func(id string) int {
		n, _ := st.DB.Database().Collection(jobs.CollectionJobs).CountDocuments(ctx, bson.M{"kind": "portal_write", "args": bson.M{"$regex": `"employeeId":"` + id + `"`}})
		return int(n)
	}

	// No mapping yet: held with the reason; nothing reaches the portal.
	a := ptJoined(t, c, st, "Ravi", "Kumar", "9876503001")
	ptWait(t, "the held create", func() bool { return ptSync(st, a)["phase"] == "blocked" })
	if fp.PostCount() != 0 || !strings.Contains(fmt.Sprint(ptSync(st, a)["phase_msg"]), "Map the") {
		t.Fatalf("held: %d posts, %v", fp.PostCount(), ptSync(st, a))
	}
	// The mapping screen lists the department, designation, property and shift people have (25 Nov 2026 fix: the
	// usage counts read the group key "id", so these lists were empty and nothing could be mapped).
	if s, r := c.do("GET", "/api/attendance/portal/map", "EMP-1002", "HR Admin", nil); s != 200 {
		t.Fatalf("map: %d %v", s, r)
	} else if kinds, _ := r["kinds"].(map[string]any); kinds == nil {
		t.Fatalf("map: no kinds %v", r)
	} else {
		for _, k := range []string{"dept", "desig", "branch", "shift"} {
			if l, _ := kinds[k].([]any); len(l) == 0 {
				t.Fatalf("mapping list %s is empty", k)
			}
		}
	}
	// Map everything; the sweep queues it and the job creates it once.
	for _, ck := range [][2]string{{"department", "dept"}, {"designation", "desig"}, {"location", "branch"}, {"shift", "shift"}, {"work_policy", "policy"}, {"work_policy", "wo"}} {
		coll, kind := ck[0], ck[1]
		rows, _ := st.DB.C(coll).Find(ctx, bson.M{})
		for _, r := range rows {
			ref := fmt.Sprint(r["id"])
			if kind == "shift" {
				ref = fmt.Sprint(r["code"])
			}
			// the fake portal's lists (the live form's)
			val := map[string]string{"dept": "Front Office", "desig": "GSA", "branch": "Saltstayz Basil", "shift": "GEN", "policy": "OPR", "wo": "Tuesday"}[kind]
			if _, err := st.DB.C("portal_map").Insert(ctx, bson.M{"kind": kind, "ref": ref, "value": val}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n, err := app.PortalSweep(ctx); err != nil || n != 1 {
		t.Fatalf("sweep queued %d: %v", n, err)
	}
	ptWait(t, "the create", func() bool { return ptSync(st, a)["state"] == "on" })
	if e := apiDoc(st, "employee", bson.M{"_id": a}); fp.PostCount() != 1 || e["attendance_code"] == nil || fp.Emps[fmt.Sprint(e["attendance_code"])] == nil {
		t.Fatalf("created: %d posts, code %v", fp.PostCount(), e["attendance_code"])
	}
	// The code sent is the number of the ID, 6 digits (owner, 25 Nov 2026): JI-000006 → 000006.
	if e := apiDoc(st, "employee", bson.M{"_id": a}); fmt.Sprint(e["attendance_code"]) != ptDigits6(a) || fp.Emps[ptDigits6(a)] == nil {
		t.Fatalf("code %v for %s, want %s", e["attendance_code"], a, ptDigits6(a))
	}
	// On the fake of the live form (25 Nov 2026): card = code padded to 8, the work policy's office time policy and
	// weekly off as mapped, the shift starting on the joining day, the company left as the form has it.
	const mc = "ctl00$MainContent$"
	if e := fp.Emps[ptDigits6(a)]; e[mc+"Txtempname"] != "Ravi Kumar" || e[mc+"Txtcardno"] != "00"+ptDigits6(a) || e[mc+"cboofficetimepolicy"] != "3" ||
		e[mc+"cboFWOff"] != "3" || e[mc+"cbodeptname"] != "4" || e[mc+"cbobranchname"] != "8" || e[mc+"cboshiftname"] != "2" ||
		e[mc+"cbocompnayname"] != "1" || e[mc+"txtshiftstartdate"] != e[mc+"TxtDOJ"] || e[mc+"TxtDOJ"] == "" {
		t.Fatalf("on the portal: %v", e)
	}
	// No punch machine mapped to their cluster: created without a device, with a note kept on the send (owner, 25 Nov
	// 2026: a note, never a hold).
	if r := apiDoc(st, "portal_send", bson.M{"employee_id": a, "action": "create"}); r["device"] != nil || !strings.Contains(fmt.Sprint(r["notes"]), "No punch machine chosen") {
		t.Fatalf("no machine: %v", r)
	}
	// A change of details → one update of that employee's form; an exit → mark left (resignation date).
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": a}, bson.M{"$set": bson.M{"name": "Ravi K. Kumar"}}); err != nil {
		t.Fatal(err)
	}
	if n, err := app.PortalSweep(ctx); err != nil || n != 1 {
		t.Fatalf("update queued %d: %v", n, err)
	}
	ptWait(t, "the update", func() bool {
		return fp.Emps[ptDigits6(a)][mc+"Txtempname"] == "Ravi K. Kumar" && ptSync(st, a)["action"] == "update"
	})
	if fp.PostCount() != 2 || ptSync(st, a)["state"] != "on" {
		t.Fatalf("update: %d posts, %v", fp.PostCount(), ptSync(st, a))
	}
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": a}, bson.M{"$set": bson.M{"status": "Inactive", "lwd": today()}}); err != nil {
		t.Fatal(err)
	}
	if n, err := app.PortalSweep(ctx); err != nil || n != 1 {
		t.Fatalf("left queued %d: %v", n, err)
	}
	ptWait(t, "the mark left", func() bool { return ptSync(st, a)["state"] == "left" })
	if e := fp.Emps[ptDigits6(a)]; fp.PostCount() != 3 || e[mc+"txtreginnationdate"] == "" || e[mc+"Txtempname"] != "Ravi K. Kumar" {
		t.Fatalf("left: %d posts, %v", fp.PostCount(), e)
	}

	// An update never sets the device.
	if r := apiDoc(st, "portal_send", bson.M{"employee_id": a, "action": "update"}); r["device"] != nil || r["notes"] != nil {
		t.Fatalf("update device: %v", r)
	}

	// A punch machine on every property (the fake portal's device): the create picks it by serial.
	locs, _ := st.DB.C("location").Find(ctx, bson.M{})
	for i, l := range locs {
		if _, err := st.DB.C("attendance_device").Insert(ctx, bson.M{"name": fmt.Sprintf("M%d", i), "vendor": "Realtime", "serial": "RSS20230761581",
			"location_id": l["id"], "status": "Active", "key_hash": fmt.Sprintf("pt-test-%d", i), "key_hint": "test", "created_by": "test"}); err != nil {
			t.Fatal(err)
		}
	}
	// Mapped: Mark joined → exactly one job and one create.
	b := ptJoined(t, c, st, "Sunita", "Verma", "9876503002")
	ptWait(t, "the second create", func() bool { return ptSync(st, b)["state"] == "on" })
	if fp.PostCount() != 4 || jobsOf(b) != 1 || apiCount(st, "portal_send", bson.M{"employee_id": b, "action": "create"}) != 1 {
		t.Fatalf("one create: %d posts, %d jobs", fp.PostCount(), jobsOf(b))
	}
	if r := apiDoc(st, "portal_send", bson.M{"employee_id": b, "action": "create"}); r["device"] != "RSS20230761581 / Gulmohar Marg" || r["notes"] != nil ||
		fp.Emps[ptDigits6(b)][mc+"Device_List"] != "RSS20230761581" {
		t.Fatalf("device: %v %v", r, fp.Emps[ptDigits6(b)])
	}
	if n, _ := app.PortalSweep(ctx); n != 0 {
		t.Fatalf("nothing due after the create, sweep queued %d", n)
	}

	// Cluster full (no room on its machines): the create still goes, without a device, with a note.
	if _, err := st.DB.C("attendance_device").UpdateMany(ctx, bson.M{}, bson.M{"$set": bson.M{"capacity": 0}}); err != nil {
		t.Fatal(err)
	}
	// Refused: failed with the portal's message, a bell, and never tried again by itself.
	fp.Refuse = "Card No. already exists"
	f := ptJoined(t, c, st, "Kiran", "Shah", "9876503003")
	ptWait(t, "the failed create", func() bool { return ptSync(st, f)["phase"] == "failed" })
	if fp.PostCount() != 5 || !strings.Contains(fmt.Sprint(ptSync(st, f)["phase_msg"]), "Card No. already exists") || apiDoc(st, "employee", bson.M{"_id": f})["attendance_code"] != nil {
		t.Fatalf("refused: %d posts, %v", fp.PostCount(), ptSync(st, f))
	}
	if n := notices(c, "EMP-1002", "HR Admin"); !strings.Contains(n, "failed") {
		t.Fatalf("HR bell: %s", n)
	}
	if n, _ := app.PortalSweep(ctx); n != 0 {
		t.Fatalf("the sweep retried a failed create (%d)", n)
	}
	if got, _ := app.PortalAutoRun(ctx, api.PortalWriteArgs{EmployeeID: f}); !strings.Contains(got, "Retry") {
		t.Fatalf("a job retried a failed create: %s", got)
	}
	time.Sleep(500 * time.Millisecond)
	if fp.PostCount() != 5 || jobsOf(f) != 1 {
		t.Fatalf("retried by itself: %d posts, %d jobs", fp.PostCount(), jobsOf(f))
	}
	// HR's Retry: one more try.
	if s, r := c.do("POST", "/api/employees/"+f+"/portal/retry", "EMP-1002", "HR Admin", nil); s != 200 {
		t.Fatalf("retry: %d %v", s, r)
	}
	ptWait(t, "the retried create", func() bool { return ptSync(st, f)["state"] == "on" })
	if fp.PostCount() != 6 {
		t.Fatalf("retry posts: %d", fp.PostCount())
	}
	if r := apiDoc(st, "portal_send", bson.M{"employee_id": f, "ok": true}); r["device"] != nil || !strings.Contains(fmt.Sprint(r["notes"]), "is full") {
		t.Fatalf("full cluster: %v", r)
	}
}

// Deleted on the portal by hand (owner, 26 Nov 2026): HR says so, the HRMS checks the portal's list first (refused
// while the code is still there), forgets the portal record, creates nothing by itself, and "Create on the portal"
// creates them again with the same code.
func TestPortalRemovedByHand(t *testing.T) {
	c, app, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	ctx := context.Background()
	fp := rtfake.New(t)
	old := realtime.Spec
	realtime.Spec = rtfake.Spec
	defer func() { realtime.Spec = old }()
	undo := api.SetMachinePortal(fp.Config(), true, nil)
	defer undo()
	jctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := app.StartJobs(jctx); err != nil {
		t.Fatal(err)
	}
	defer app.StopJobs(ctx)
	for _, ck := range [][2]string{{"department", "dept"}, {"designation", "desig"}, {"location", "branch"}, {"shift", "shift"}, {"work_policy", "policy"}, {"work_policy", "wo"}} {
		rows, _ := st.DB.C(ck[0]).Find(ctx, bson.M{})
		for _, r := range rows {
			ref := fmt.Sprint(r["id"])
			if ck[1] == "shift" {
				ref = fmt.Sprint(r["code"])
			}
			val := map[string]string{"dept": "Front Office", "desig": "GSA", "branch": "Saltstayz Basil", "shift": "GEN", "policy": "OPR", "wo": "Tuesday"}[ck[1]]
			if _, err := st.DB.C("portal_map").Insert(ctx, bson.M{"kind": ck[1], "ref": ref, "value": val}); err != nil {
				t.Fatal(err)
			}
		}
	}
	a := ptJoined(t, c, st, "Hand", "Removed", "9876503099")
	ptWait(t, "the create", func() bool { return ptSync(st, a)["state"] == "on" })
	code := ptDigits6(a)
	if s, r := c.do("POST", "/api/employees/"+a+"/portal/removed", "EMP-1002", "HR Admin", nil); s == 200 || !strings.Contains(fmt.Sprint(r), "Still on the portal") {
		t.Fatalf("removed while still there: %d %v", s, r)
	}
	fp.Remove(code)
	if s, r := c.do("POST", "/api/employees/"+a+"/portal/removed", "EMP-1002", "HR Admin", nil); s != 200 {
		t.Fatalf("removed: %d %v", s, r)
	}
	if e := apiDoc(st, "employee", bson.M{"_id": a}); e["attendance_code"] != nil || ptSync(st, a)["state"] != "removed" {
		t.Fatalf("after removed: code %v, %v", e["attendance_code"], ptSync(st, a))
	}
	posts := fp.PostCount()
	if n, _ := app.PortalSweep(ctx); n != 0 {
		t.Fatalf("the sweep re-created a removed employee by itself (%d)", n)
	}
	if s, r := c.do("POST", "/api/employees/"+a+"/portal/create", "EMP-1002", "HR Admin", nil); s != 200 {
		t.Fatalf("create again: %d %v", s, r)
	}
	ptWait(t, "the create again", func() bool { return ptSync(st, a)["state"] == "on" })
	if e := apiDoc(st, "employee", bson.M{"_id": a}); fp.PostCount() != posts+1 || fmt.Sprint(e["attendance_code"]) != code || fp.Emps[code] == nil {
		t.Fatalf("created again: %d posts, code %v", fp.PostCount()-posts, e["attendance_code"])
	}
}
