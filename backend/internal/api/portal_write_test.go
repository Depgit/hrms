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
		n, _ := st.DB.Database().Collection(jobs.CollectionJobs).CountDocuments(ctx, bson.M{"kind": "portal_write", "args.employeeId": id})
		return int(n)
	}

	// No mapping yet: held with the reason; nothing reaches the portal.
	a := ptJoined(t, c, st, "Ravi", "Kumar", "9876503001")
	ptWait(t, "the held create", func() bool { return ptSync(st, a)["phase"] == "blocked" })
	if fp.PostCount() != 0 || !strings.Contains(fmt.Sprint(ptSync(st, a)["phase_msg"]), "Map the") {
		t.Fatalf("held: %d posts, %v", fp.PostCount(), ptSync(st, a))
	}
	// Map everything; the sweep queues it and the job creates it once.
	for coll, kind := range map[string]string{"department": "dept", "designation": "desig", "location": "branch", "shift": "shift"} {
		rows, _ := st.DB.C(coll).Find(ctx, bson.M{})
		for _, r := range rows {
			ref := fmt.Sprint(r["id"])
			if kind == "shift" {
				ref = fmt.Sprint(r["code"])
			}
			if _, err := st.DB.C("portal_map").Insert(ctx, bson.M{"kind": kind, "ref": ref, "value": fmt.Sprint(r["name"])}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n, err := app.PortalSweep(ctx); err != nil || n != 1 {
		t.Fatalf("sweep queued %d: %v", n, err)
	}
	ptWait(t, "the create", func() bool { t.Logf("%v", ptSync(st, a)); return ptSync(st, a)["state"] == "on" })
	if e := apiDoc(st, "employee", bson.M{"_id": a}); fp.PostCount() != 1 || e["attendance_code"] == nil || fp.Emps[fmt.Sprint(e["attendance_code"])] == nil {
		t.Fatalf("created: %d posts, code %v", fp.PostCount(), e["attendance_code"])
	}

	// Mapped: Mark joined → exactly one job and one create.
	b := ptJoined(t, c, st, "Sunita", "Verma", "9876503002")
	ptWait(t, "the second create", func() bool { return ptSync(st, b)["state"] == "on" })
	if fp.PostCount() != 2 || jobsOf(b) != 1 || apiCount(st, "portal_send", bson.M{"employee_id": b, "action": "create"}) != 1 {
		t.Fatalf("one create: %d posts, %d jobs", fp.PostCount(), jobsOf(b))
	}
	if n, _ := app.PortalSweep(ctx); n != 0 {
		t.Fatalf("nothing due after the create, sweep queued %d", n)
	}

	// Refused: failed with the portal's message, a bell, and never tried again by itself.
	fp.Refuse = "Card No. already exists"
	f := ptJoined(t, c, st, "Kiran", "Shah", "9876503003")
	ptWait(t, "the failed create", func() bool { return ptSync(st, f)["phase"] == "failed" })
	if fp.PostCount() != 3 || !strings.Contains(fmt.Sprint(ptSync(st, f)["phase_msg"]), "Card No. already exists") || apiDoc(st, "employee", bson.M{"_id": f})["attendance_code"] != nil {
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
	if fp.PostCount() != 3 || jobsOf(f) != 1 {
		t.Fatalf("retried by itself: %d posts, %d jobs", fp.PostCount(), jobsOf(f))
	}
	// HR's Retry: one more try.
	if s, r := c.do("POST", "/api/employees/"+f+"/portal/retry", "EMP-1002", "HR Admin", nil); s != 200 {
		t.Fatalf("retry: %d %v", s, r)
	}
	ptWait(t, "the retried create", func() bool { return ptSync(st, f)["state"] == "on" })
	if fp.PostCount() != 4 {
		t.Fatalf("retry posts: %d", fp.PostCount())
	}
}
