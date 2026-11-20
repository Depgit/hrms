package api_test

// The hiring lifecycle (hiring.go): offer versions and answers, the Offer ID at acceptance, expiry and lapse,
// joining outcomes, the waiting window, early exit, the Joining ID record, background check, the Employee ID with
// the re-key and the alias, contract staff without an Employee ID, the employee stage and timeline, the drop-off
// report and the migration's backfill.

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

	"github.com/saltstayz/hrms/internal/api"
	"github.com/saltstayz/hrms/internal/store"
)

func hireDay(n int) string { return time.Now().AddDate(0, 0, n).Format("2006-01-02") }

// hireCand reads the candidate view.
func hireCand(t *testing.T, c *client, id string) map[string]any {
	t.Helper()
	s, x := c.do("GET", "/api/candidates/"+id, "EMP-1004", "HRBP", nil)
	if s != 200 {
		t.Fatalf("candidate %s: %d %v", id, s, x)
	}
	return x
}

// hireAccepted takes a new candidate to an accepted offer; it returns the SI and the offer's key.
func hireAccepted(t *testing.T, c *client, st *store.Store, first, last, phone string) (string, string) {
	t.Helper()
	si := selectCandidate(t, c, shortlistBody(st, first, last, phone))
	oi := approvedOffer(t, c, st, si, first, last, phone)
	if s, r := c.do("POST", "/api/offers/"+oi+"/release", "EMP-1004", "HRBP", nil); s != 200 {
		t.Fatalf("release: %v", r)
	}
	if s, r := c.do("POST", "/api/offers/"+oi+"/accept", "EMP-1004", "HRBP", nil); s != 200 {
		t.Fatalf("accept: %v", r)
	}
	return si, oi
}

// hireEvent posts a candidate action.
func hireEvent(c *client, si string, body map[string]any) (int, map[string]any) {
	return c.do("POST", "/api/candidates/"+si+"/event", "EMP-1004", "HRBP", body)
}

func hireActions(x map[string]any) string {
	out := []string{}
	for _, e := range x["timeline"].([]any) {
		out = append(out, fmt.Sprint(e.(map[string]any)["action"]))
	}
	return strings.Join(out, " | ")
}

// hireBackdate moves the offer's release (and so the earliest joining date) n days back.
func hireBackdate(t *testing.T, st *store.Store, oi string, n int) {
	t.Helper()
	if _, err := st.DB.C("offer").Update(context.Background(), bson.D{{Key: "_id", Value: oi}}, bson.D{{Key: "$set", Value: bson.D{{Key: "released_at", Value: time.Now().AddDate(0, 0, -n)}}}}); err != nil {
		t.Fatal(err)
	}
}

// hireSince moves the start of a waiting window (to end it without waiting a week).
func hireSince(t *testing.T, st *store.Store, si, since string) {
	t.Helper()
	if _, err := st.DB.C("candidate").Raw().UpdateOne(context.Background(), bson.D{{Key: "_id", Value: si}}, bson.D{{Key: "$set", Value: bson.D{{Key: "waiting.since", Value: since}}}}); err != nil {
		t.Fatal(err)
	}
}

func TestHireOfferVersions(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	si := selectCandidate(t, c, shortlistBody(st, "Riya", "Sharma", "9876501234"))
	oi := approvedOffer(t, c, st, si, "Riya", "Sharma", "9876501234")
	x := hireCand(t, c, si)
	vs := x["offerVersions"].([]any)
	if x["current_id"] != si || x["ids"].(map[string]any)["oi"] != "" || len(vs) != 1 || vs[0].(map[string]any)["status"] != "Approved" {
		t.Fatalf("no Offer ID before acceptance: %v %v %v", x["current_id"], x["ids"], vs)
	}
	if s, _ := c.do("POST", "/api/offers/"+oi+"/release", "EMP-1004", "HRBP", map[string]string{"expires": hireDay(-1)}); s != 400 {
		t.Fatal("the answer is not due in the past")
	}
	if s, r := c.do("POST", "/api/offers/"+oi+"/release", "EMP-1004", "HRBP", map[string]string{"expires": hireDay(3)}); s != 200 {
		t.Fatal(r)
	}
	x = hireCand(t, c, si)
	if x["expires_on"] != hireDay(3) || x["offerVersions"].([]any)[0].(map[string]any)["status"] != "Sent" || !strings.Contains(fmt.Sprint(x["status"]), "answer due by") {
		t.Fatalf("sent: %v %v %v", x["expires_on"], x["offerVersions"], x["status"])
	}
	// The candidate asks about the breakup: HR records it and revises.
	if s, _ := c.do("POST", "/api/offers/"+oi+"/query", "EMP-1004", "HRBP", map[string]string{}); s != 400 {
		t.Fatal("the question is required")
	}
	if s, r := c.do("POST", "/api/offers/"+oi+"/query", "EMP-1004", "HRBP", map[string]string{"reason": "Why is HRA lower than the last job?"}); s != 200 {
		t.Fatal(r)
	}
	x = hireCand(t, c, si)
	if x["bucket"] != "Offer" || !strings.Contains(fmt.Sprint(x["status"]), "Question about the breakup") || x["can"].(map[string]any)["raiseOffer"] != true {
		t.Fatalf("question: %v %v %v", x["bucket"], x["status"], x["can"])
	}
	od := offerData(st, "Riya", "Sharma", "9876501234")
	od["gross"] = 28000
	s, r := c.do("POST", "/api/offers", "EMP-1004", "HRBP", map[string]any{"data": od, "candidate": si})
	if s != 200 || r["version"] != 2.0 || r["id"] != oi {
		t.Fatalf("revised: %d %v", s, r)
	}
	ui3ApproveAll(t, c, r["request"].(map[string]any)["id"].(string))
	c.do("POST", "/api/offers/"+oi+"/release", "EMP-1004", "HRBP", nil)
	if _, a := c.do("POST", "/api/offers/"+oi+"/accept", "EMP-1004", "HRBP", nil); !strings.Contains(fmt.Sprint(a["result"]), "Offer ID OI-0001") {
		t.Fatalf("accepted: %v", a)
	}
	x = hireCand(t, c, si)
	vs = x["offerVersions"].([]any)
	v1, v2 := vs[0].(map[string]any), vs[1].(map[string]any)
	if x["current_id"] != "OI-0001" || x["ids"].(map[string]any)["oi"] != "OI-0001" || len(vs) != 2 || v1["status"] != "Revised" || v1["answer"] != "Why is HRA lower than the last job?" ||
		v2["status"] != "Accepted" || v2["frozen"] != true || v2["gross"] != 28000.0 {
		t.Fatalf("accepted: %v %v %v", x["current_id"], x["ids"], vs)
	}
	if s, _ := c.do("POST", "/api/offers", "EMP-1004", "HRBP", map[string]any{"data": od, "candidate": si}); s != 400 {
		t.Fatal("the accepted version is frozen")
	}
	if a := hireActions(x); !strings.Contains(a, "Shortlisted | Shortlist approved | Selected at the interview | Offer v1 raised | Offer v1 approved | Offer v1 sent | Question about the breakup (v1) | Offer v2 raised | Offer v2 approved | Offer v2 sent | Offer accepted: Offer ID OI-0001") {
		t.Fatalf("timeline: %s", a)
	}
}

func TestHireOfferDeclineLapse(t *testing.T) {
	c, srv, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	// Declined, with the reason.
	si := selectCandidate(t, c, shortlistBody(st, "Asha", "Rao", "9876502001"))
	oi := approvedOffer(t, c, st, si, "Asha", "Rao", "9876502001")
	c.do("POST", "/api/offers/"+oi+"/release", "EMP-1004", "HRBP", nil)
	if s, _ := c.do("POST", "/api/offers/"+oi+"/decline", "EMP-1004", "HRBP", map[string]string{}); s != 400 {
		t.Fatal("the reason is required")
	}
	c.do("POST", "/api/offers/"+oi+"/decline", "EMP-1004", "HRBP", map[string]string{"reason": "Took another offer"})
	x := hireCand(t, c, si)
	if x["closed_kind"] != "Declined" || x["offerVersions"].([]any)[0].(map[string]any)["status"] != "Declined" || x["offer_no"] != nil {
		t.Fatalf("declined: %v %v %v", x["closed_kind"], x["offerVersions"], x["offer_no"])
	}
	// No answer: a reminder before the date, then it lapses.
	si2 := selectCandidate(t, c, shortlistBody(st, "Bina", "Das", "9876502002"))
	oi2 := approvedOffer(t, c, st, si2, "Bina", "Das", "9876502002")
	c.do("POST", "/api/offers/"+oi2+"/release", "EMP-1004", "HRBP", map[string]string{"expires": hireDay(1)})
	if s, _ := c.do("POST", "/api/offers/"+oi2+"/expiry", "EMP-1004", "HRBP", map[string]string{"expires": hireDay(2)}); s != 400 {
		t.Fatal("moving the expiry needs a reason")
	}
	if s, r := c.do("POST", "/api/offers/"+oi2+"/expiry", "EMP-1004", "HRBP", map[string]string{"expires": hireDay(2), "reason": "Candidate travelling"}); s != 200 {
		t.Fatal(r)
	}
	res, err := srv.HireSweep(ctx, time.Now())
	if err != nil || res.OfferReminders != 1 || res.Lapsed != 0 {
		t.Fatalf("reminder: %+v %v", res, err)
	}
	if res, _ = srv.HireSweep(ctx, time.Now()); res.OfferReminders != 0 {
		t.Fatal("one reminder a day")
	}
	if n := notices(c, "EMP-1004", "HRBP"); !strings.Contains(n, "lapses after") {
		t.Fatalf("HR is reminded: %s", n)
	}
	if res, err = srv.HireSweep(ctx, time.Now().AddDate(0, 0, 3)); err != nil || res.Lapsed != 1 {
		t.Fatalf("lapse: %+v %v", res, err)
	}
	x = hireCand(t, c, si2)
	if x["closed_kind"] != "Offer lapsed" || x["offerVersions"].([]any)[0].(map[string]any)["status"] != "Lapsed" || !strings.Contains(hireActions(x), "Closed: Offer lapsed") {
		t.Fatalf("lapsed: %v %v", x["closed_kind"], x["offerVersions"])
	}
	if _, o := c.do("GET", "/api/offers/"+oi2, "EMP-1004", "HRBP", nil); o["status"] != "Lapsed" {
		t.Fatal(o["status"])
	}
}

func TestHireJoiningOutcomesAndWaiting(t *testing.T) {
	c, srv, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	employees := stagesCount(st, "employee", bson.D{})

	// Joining date moved, then withdrew before it.
	si1, oi1 := hireAccepted(t, c, st, "Asha", "Rao", "9876502001")
	if s, _ := hireEvent(c, si1, map[string]any{"action": "doj", "date": hireDay(5)}); s != 400 {
		t.Fatal("moving the date needs a reason")
	}
	if s, r := hireEvent(c, si1, map[string]any{"action": "doj", "date": hireDay(5), "reason": "Notice period at the old job"}); s != 200 {
		t.Fatal(r)
	}
	if _, o := c.do("GET", "/api/offers/"+oi1, "EMP-1004", "HRBP", nil); o["doj"] != hireDay(5) {
		t.Fatal(o["doj"])
	}
	if s, _ := hireEvent(c, si1, map[string]any{"action": "no-show"}); s != 400 {
		t.Fatal("no no-show before the joining date")
	}
	if s, _ := hireEvent(c, si1, map[string]any{"action": "declined-day", "reason": "x"}); s != 400 {
		t.Fatal("declined on the day is on the day")
	}
	if s, r := hireEvent(c, si1, map[string]any{"action": "withdrew", "reason": "Family moved to Pune"}); s != 200 {
		t.Fatal(r)
	}
	// Came on the day and declined.
	si2, _ := hireAccepted(t, c, st, "Bina", "Das", "9876502002")
	if s, r := hireEvent(c, si2, map[string]any{"action": "declined-day", "reason": "Shift timings"}); s != 200 {
		t.Fatal(r)
	}
	// No-show: waiting, calls, an extension, then back within the window: joined on the actual date.
	si3, _ := hireAccepted(t, c, st, "Chitra", "Iyer", "9876502003")
	if s, r := hireEvent(c, si3, map[string]any{"action": "no-show", "reason": "Not answering"}); s != 200 || !strings.Contains(fmt.Sprint(r["result"]), "day 1 of 7") {
		t.Fatalf("no-show: %d %v", s, r)
	}
	x := hireCand(t, c, si3)
	if x["bucket"] != "Waiting" || !strings.Contains(fmt.Sprint(x["status"]), "No-show: waiting, day 1 of 7") {
		t.Fatalf("waiting: %v %v", x["bucket"], x["status"])
	}
	if res, _ := srv.HireSweep(ctx, time.Now()); res.WaitReminders != 1 {
		t.Fatalf("a daily reminder: %+v", res)
	}
	if n := notices(c, "EMP-1004", "HRBP"); !strings.Contains(n, "Chitra Iyer (OI-0003): No-show: waiting, day 1 of 7") {
		t.Fatalf("HR is reminded: %s", n)
	}
	hireEvent(c, si3, map[string]any{"action": "call", "reason": "Phone switched off"})
	if s, _ := hireEvent(c, si3, map[string]any{"action": "extend", "days": 3}); s != 400 {
		t.Fatal("an extension needs a reason")
	}
	hireEvent(c, si3, map[string]any{"action": "extend", "days": 3, "reason": "Brother says he is unwell"})
	if s, _ := hireEvent(c, si3, map[string]any{"action": "close"}); s != 400 {
		t.Fatal("the window is still open")
	}
	x = hireCand(t, c, si3)
	w := x["waitingWindow"].(map[string]any)
	if w["total"] != 10.0 || len(w["calls"].([]any)) != 1 || len(w["ext"].([]any)) != 1 {
		t.Fatalf("window: %v", w)
	}
	if s, r := hireEvent(c, si3, map[string]any{"action": "back", "date": today()}); s != 200 || r["employeeId"] != "JI-0003" {
		t.Fatalf("back: %d %v", s, r)
	}
	x = hireCand(t, c, si3)
	if x["bucket"] != "Joined" || x["waitingWindow"] != nil || stagesEmployee(st, "JI-0003")["status"] != "Active" {
		t.Fatalf("joined after waiting: %v %v", x["bucket"], x["waitingWindow"])
	}
	// No-show, the window ends: HR confirms the close.
	si4, _ := hireAccepted(t, c, st, "Deepa", "Nair", "9876502004")
	hireEvent(c, si4, map[string]any{"action": "no-show", "reason": "Not answering"})
	hireSince(t, st, si4, hireDay(-8))
	if s, r := hireEvent(c, si4, map[string]any{"action": "close"}); s != 200 {
		t.Fatal(r)
	}
	// Automatic close (setting).
	if s, _ := c.do("PUT", "/api/onboarding/settings", "EMP-1004", "HRBP", map[string]any{"hiring": map[string]any{"waitDays": 5, "waitAutoClose": true, "bgvBlocks": true, "offerDays": 7, "offerRemindDays": 2}}); s != 403 {
		t.Fatal("onboarding setup only")
	}
	if s, _ := c.do("PUT", "/api/onboarding/settings", "EMP-1002", "HR Admin", map[string]any{"hiring": map[string]any{"waitDays": 0, "offerDays": 7}}); s != 400 {
		t.Fatal("a window of at least a day")
	}
	if s, r := c.do("PUT", "/api/onboarding/settings", "EMP-1002", "HR Admin", map[string]any{"hiring": map[string]any{"waitDays": 5, "waitAutoClose": true, "bgvBlocks": true, "offerDays": 7, "offerRemindDays": 2}}); s != 200 {
		t.Fatal(r)
	}
	if _, g := c.do("GET", "/api/onboarding/settings", "EMP-1004", "HRBP", nil); g["hiring"].(map[string]any)["waitDays"] != 5.0 {
		t.Fatal(g)
	}
	si5, _ := hireAccepted(t, c, st, "Esha", "Kumar", "9876502005")
	hireEvent(c, si5, map[string]any{"action": "no-show", "reason": "Not answering"})
	if x := hireCand(t, c, si5); !strings.Contains(fmt.Sprint(x["status"]), "day 1 of 5") {
		t.Fatal(x["status"])
	}
	hireSince(t, st, si5, hireDay(-6))
	if res, err := srv.HireSweep(ctx, time.Now()); err != nil || res.AutoClosed != 1 {
		t.Fatalf("auto close: %+v %v", res, err)
	}
	_, l := c.do("GET", "/api/candidates?stage=Closed", "EMP-1004", "HRBP", nil)
	kinds := map[string]string{}
	for _, r := range l["rows"].([]any) {
		m := r.(map[string]any)
		kinds[fmt.Sprint(m["id"])] = fmt.Sprint(m["closed_kind"])
	}
	want := map[string]string{si1: "Withdrew before joining", si2: "Declined on joining day", si4: "No-show", si5: "No-show"}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("closed: %v", kinds)
	}
	if n := stagesCount(st, "employee", bson.D{}); n != employees+1 {
		t.Fatalf("only the one who joined has a record: %d (was %d)", n, employees)
	}
	// The drop-off report counts them.
	_, rep := c.do("GET", "/api/reports/hire_dropoff?from="+today()+"&to="+today()+"&ds=recruiter", "EMP-1004", "HRBP", nil)
	if fmt.Sprint(rep["cols"]) != "[Recruiter Offer declined Offer lapsed Withdrew before joining No-show Declined on joining day Early exit Total]" ||
		fmt.Sprint(rep["rows"]) != "[[Meera Kulkarni 0 0 1 2 1 0 4]]" {
		t.Fatalf("drop-off: %v %v", rep["cols"], rep["rows"])
	}
	_, rep = c.do("GET", "/api/reports/hire_dropoff?from="+today()+"&to="+today()+"&ds=month", "EMP-1004", "HRBP", nil)
	if fmt.Sprint(rep["rows"]) != fmt.Sprintf("[[%s 0 0 1 2 1 0 4]]", today()[:7]) {
		t.Fatalf("drop-off by month: %v", rep["rows"])
	}
}

func TestHireStoppedComingEarlyExit(t *testing.T) {
	c, srv, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	// Joined four days ago on JI-0001; stopped coming yesterday; back today: the process resumes.
	si, oi := hireAccepted(t, c, st, "Riya", "Sharma", "9876501234")
	hireBackdate(t, st, oi, 6)
	if s, r := c.do("POST", "/api/candidates/"+si+"/joined", "EMP-1004", "HRBP", map[string]any{"date": hireDay(-4)}); s != 200 || r["id"] != "JI-0001" {
		t.Fatalf("joined: %d %v", s, r)
	}
	if s, _ := hireEvent(c, si, map[string]any{"action": "stopped", "date": hireDay(-4)}); s != 400 {
		t.Fatal("the first missed day is after the joining date")
	}
	if s, r := hireEvent(c, si, map[string]any{"action": "stopped", "date": hireDay(-1), "reason": "Did not come, phone off"}); s != 200 {
		t.Fatal(r)
	}
	if x := hireCand(t, c, si); x["bucket"] != "Waiting" || !strings.Contains(fmt.Sprint(x["status"]), "Stopped coming: waiting, day 2 of 7") {
		t.Fatalf("stopped: %v %v", x["bucket"], x["status"])
	}
	if s, r := hireEvent(c, si, map[string]any{"action": "back", "date": today()}); s != 200 || !strings.Contains(fmt.Sprint(r["result"]), "1 missed day count as absent") {
		t.Fatalf("back: %d %v", s, r)
	}
	if x := hireCand(t, c, si); x["bucket"] != "Joined" || x["waitingWindow"] != nil || x["current_id"] != "JI-0001" {
		t.Fatalf("resumed: %v %v", x["bucket"], x["current_id"])
	}
	// Stops again; the window ends; HR closes: Early exit, paid for the days worked, no separation case.
	hireEvent(c, si, map[string]any{"action": "stopped", "date": today(), "reason": "Gone again"})
	hireSince(t, st, si, hireDay(-1))
	if _, err := st.DB.C("candidate").Raw().UpdateOne(ctx, bson.D{{Key: "_id", Value: si}}, bson.D{{Key: "$set", Value: bson.D{{Key: "waiting.days", Value: 1}}}}); err != nil {
		t.Fatal(err)
	}
	if s, r := hireEvent(c, si, map[string]any{"action": "close", "rehire": "No"}); s != 200 {
		t.Fatal(r)
	}
	e := stagesEmployee(st, "JI-0001")
	if e["status"] != "Inactive" || e["lwd"] != hireDay(-2) || e["exit_reason"] != "Early exit" || e["rehire"] != "No" {
		t.Fatalf("early exit: %v %v %v %v", e["status"], e["lwd"], e["exit_reason"], e["rehire"])
	}
	if n := stagesCount(st, "separation", bson.D{{Key: "employee_id", Value: "JI-0001"}}); n != 0 {
		t.Fatal("no separation case")
	}
	ym := hireDay(-2)[:7]
	f, err := api.PayEligible(ctx, st.DB, ym)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.DB.C("employee").Exists(ctx, bson.M{"$and": bson.A{f, bson.M{"_id": "JI-0001"}}}); !ok {
		t.Fatal("paid for the days worked in " + ym)
	}
	if x := hireCand(t, c, si); x["closed_kind"] != "Early exit" {
		t.Fatal(x["closed_kind"])
	}
	// Says they quit: Early exit at once.
	si2, oi2 := hireAccepted(t, c, st, "Bina", "Das", "9876502002")
	hireBackdate(t, st, oi2, 5)
	c.do("POST", "/api/candidates/"+si2+"/joined", "EMP-1004", "HRBP", map[string]any{"date": hireDay(-3)})
	if s, _ := hireEvent(c, si2, map[string]any{"action": "quit", "date": hireDay(-1)}); s != 400 {
		t.Fatal("quitting needs the reason")
	}
	if s, r := hireEvent(c, si2, map[string]any{"action": "quit", "date": hireDay(-1), "reason": "Found a job nearer home"}); s != 200 {
		t.Fatal(r)
	}
	if e := stagesEmployee(st, "JI-0002"); e["status"] != "Inactive" || e["lwd"] != hireDay(-1) || e["rehire"] != "Yes" {
		t.Fatalf("quit: %v %v %v", e["status"], e["lwd"], e["rehire"])
	}
	_, tl := c.do("GET", "/api/employees/JI-0002/timeline", "EMP-1002", "HR Admin", nil)
	if a := fmt.Sprint(tl["events"]); !strings.Contains(a, "Record made at joining") || !strings.Contains(a, "Early exit") || !strings.Contains(a, "Closed: Early exit") {
		t.Fatalf("timeline: %v", a)
	}
	if res, err := srv.HireSweep(ctx, time.Now()); err != nil || res.AutoClosed+res.WaitReminders != 0 {
		t.Fatalf("nothing waits: %+v %v", res, err)
	}
}

// The Joining-ID person is a normal employee; on full approval the record moves to the Employee ID.
func TestHireApprovalRekey(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009", "EMP-1011", "EMP-1012")
	si, oi := hireAccepted(t, c, st, "Riya", "Sharma", "9876501234")
	before := nextCode(st)
	if s, r := c.do("POST", "/api/candidates/"+si+"/joined", "EMP-1004", "HRBP", map[string]any{"date": today()}); s != 200 || r["id"] != "JI-0001" {
		t.Fatalf("joined: %v", r)
	}
	// Punches from day 1.
	_, dev := c.do("POST", "/api/devices", "EMP-1002", "HR Admin", map[string]any{"name": "Sector 29 front desk", "vendor": "eSSL"})
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(map[string]any{"punches": []any{map[string]any{"employeeId": "JI-0001", "time": time.Now().Add(-time.Minute).Format("2006-01-02 15:04"), "direction": "IN"}}})
	req, _ := http.NewRequest("POST", c.base+"/api/punches", &buf)
	req.Header.Set("X-Device-Key", fmt.Sprint(dev["key"]))
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != 200 {
		t.Fatalf("punch: %v %v", res, err)
	}
	if n := stagesCount(st, "punch", bson.D{{Key: "employee_id", Value: "JI-0001"}}); n != 1 {
		t.Fatalf("the Joining ID punches: %d", n)
	}
	f, _ := api.PayEligible(ctx, st.DB, today()[:7])
	if ok, _ := st.DB.C("employee").Exists(ctx, bson.M{"$and": bson.A{f, bson.M{"_id": "JI-0001"}}}); !ok {
		t.Fatal("the Joining ID is in payroll")
	}
	// A published month's rows as payroll wrote them (snapshots keep the ID of the time).
	snap := map[string]any{"emp": map[string]any{"id": "JI-0001"}}
	if _, err := st.DB.C("payroll_row").Raw().InsertOne(ctx, bson.M{"employee_id": "JI-0001", "ym": "2026-10", "net": 21000.0}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.C("payslip").Raw().InsertOne(ctx, bson.M{"employee_id": "JI-0001", "ym": "2026-10", "snap": snap}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.C("work_policy_map").Raw().InsertOne(ctx, bson.M{"policy_id": "p1", "level": "employee", "ref": "JI-0001", "on_map": true}); err != nil {
		t.Fatal(err)
	}
	// Joining details and documents, Payroll verification; the background check holds the Employee ID.
	if s, r := c.do("POST", "/api/offers/"+oi+"/joining", "EMP-1004", "HRBP", map[string]any{"data": joinData("ABCDE1234F")}); s != 200 {
		t.Fatal(r)
	}
	for _, k := range []string{"pan", "photo", "police", "bankProof"} {
		c.upload("EMP-1004", "HRBP", "candidate", si, k)
	}
	if s, _ := c.do("POST", "/api/candidates/"+si+"/bgv", "EMP-1004", "HRBP", map[string]string{"status": "Clear"}); s != 400 {
		t.Fatal("the report comes first")
	}
	if s, r := c.upload("EMP-1004", "HRBP", "candidate", si, "bgvReport"); s != 200 {
		t.Fatal(r)
	}
	if s, _ := c.do("POST", "/api/candidates/"+si+"/bgv", "EMP-1004", "HRBP", map[string]string{"status": "Discrepancy"}); s != 400 {
		t.Fatal("say what the discrepancy is")
	}
	if s, r := c.do("POST", "/api/candidates/"+si+"/bgv", "EMP-1004", "HRBP", map[string]string{"status": "Discrepancy", "note": "Previous employer dates differ"}); s != 200 {
		t.Fatal(r)
	}
	s, v := c.do("POST", "/api/offers/"+oi+"/send", "EMP-1004", "HRBP", nil)
	if s != 200 {
		t.Fatal(v)
	}
	ui3ApproveAll(t, c, v["id"].(string))
	x := hireCand(t, c, si)
	if x["current_id"] != "JI-0001" || x["bucket"] != "Joined" || !strings.Contains(fmt.Sprint(x["status"]), "waits for a clear background check") || nextCode(st) != before {
		t.Fatalf("held: %v %v %v", x["current_id"], x["bucket"], x["status"])
	}
	s, b := c.do("POST", "/api/candidates/"+si+"/bgv", "EMP-1004", "HRBP", map[string]string{"status": "Clear", "note": "Dates confirmed by the employer"})
	if s != 200 || b["employeeId"] != before {
		t.Fatalf("clear issues %s: %d %v", before, s, b)
	}
	emp := before
	// The record and every reference moved; the Joining ID is an alias.
	e := stagesEmployee(st, emp)
	if e["status"] != "Active" || e["stage"] != "Probation" || fmt.Sprint(e["alias_ids"]) != "[JI-0001]" || stagesEmployee(st, "JI-0001")["id"] != nil {
		t.Fatalf("moved: %v %v %v", e["status"], e["stage"], e["alias_ids"])
	}
	for coll, f := range map[string]string{"punch": "employee_id", "document": "employee_id", "candidate": "employee_id", "offer": "employee_id", "approval_request": "employee_id",
		"gross_history": "employee_id", "employee_pay": "employee_id", "employee_personal": "employee_id", "lifecycle_event": "employee_id", "payroll_row": "employee_id", "payslip": "employee_id"} {
		if n := stagesCount(st, coll, bson.D{{Key: f, Value: "JI-0001"}}); n != 0 {
			t.Fatalf("%s still has %d on JI-0001", coll, n)
		}
		if n := stagesCount(st, coll, bson.D{{Key: f, Value: emp}}); n == 0 {
			t.Fatalf("%s has nothing on %s", coll, emp)
		}
	}
	if stagesCount(st, "work_policy_map", bson.D{{Key: "level", Value: "employee"}, {Key: "ref", Value: emp}}) != 1 {
		t.Fatal("an employee-level work policy follows the record")
	}
	ps := apiDoc(st, "payslip", bson.D{{Key: "employee_id", Value: emp}})
	if fmt.Sprint(ps["snap"]) != "map[emp:map[id:JI-0001]]" || apiFloat(apiDoc(st, "payroll_row", bson.D{{Key: "employee_id", Value: emp}})["net"]) != 21000 {
		t.Fatalf("published rows keep their values: %v", ps["snap"])
	}
	// Lookups by the old ID still work.
	if s, r := c.do("GET", "/api/employees/JI-0001", "EMP-1002", "HR Admin", nil); s != 200 || !strings.Contains(fmt.Sprint(r), emp) {
		t.Fatalf("alias lookup: %d %v", s, r)
	}
	if _, r := c.do("GET", "/api/employees?q=JI-0001", "EMP-1002", "HR Admin", nil); !strings.Contains(fmt.Sprint(r), emp) {
		t.Fatalf("alias search: %v", r)
	}
	if x := hireCand(t, c, emp); x["id"] != si || x["bucket"] != "Done" || x["current_id"] != emp {
		t.Fatalf("the candidate by the Employee ID: %v %v %v", x["id"], x["bucket"], x["current_id"])
	}
	ids := []string{}
	for _, tr := range hireCand(t, c, si)["trail"].([]any) {
		ids = append(ids, fmt.Sprint(tr.(map[string]any)["id"]))
	}
	if strings.Join(ids, " → ") != "SI-0001 → OI-0001 → JI-0001 → "+emp {
		t.Fatalf("trail: %v", ids)
	}
	_, tl := c.do("GET", "/api/employees/JI-0001/timeline", "EMP-1002", "HR Admin", nil)
	if a := fmt.Sprint(tl["events"]); tl["id"] != emp || !strings.Contains(a, "Employee ID issued: "+emp) || !strings.Contains(a, "Background check: Discrepancy") || !strings.Contains(a, "Shortlisted") {
		t.Fatalf("timeline: %v %v", tl["id"], a)
	}
	// The stage is set by HR with a reason (emp.status).
	if s, _ := c.do("POST", "/api/employees/"+emp+"/stage", "EMP-1004", "HRBP", map[string]string{"stage": "Confirmed", "reason": "x"}); s != 403 {
		t.Fatal("the status right")
	}
	if s, _ := c.do("POST", "/api/employees/"+emp+"/stage", "EMP-1002", "HR Admin", map[string]string{"stage": "Joining", "reason": "x"}); s != 400 {
		t.Fatal("Joining is the system's")
	}
	if s, _ := c.do("POST", "/api/employees/"+emp+"/stage", "EMP-1002", "HR Admin", map[string]string{"stage": "Confirmed"}); s != 400 {
		t.Fatal("a reason")
	}
	if s, r := c.do("POST", "/api/employees/"+emp+"/stage", "EMP-1002", "HR Admin", map[string]string{"stage": "Confirmed", "reason": "Probation review passed"}); s != 200 {
		t.Fatal(r)
	}
	_, tl = c.do("GET", "/api/employees/"+emp+"/timeline", "EMP-1002", "HR Admin", nil)
	if tl["stage"] != "Confirmed" || !strings.Contains(fmt.Sprint(tl["events"]), "Stage: Confirmed") || stagesEmployee(st, emp)["status"] != "Active" {
		t.Fatalf("stage: %v", tl)
	}
}

// Employment types that never get an Employee ID keep their own series' number from joining.
func TestHireNoEmployeeIDType(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	if s, r := c.do("PUT", "/api/employee-codes", "EMP-1002", "HR Admin", map[string]any{"byType": false, "rejoin": "same", "series": []any{}, "noEmpId": []string{"Permanent"}}); s != 200 {
		t.Fatal(r)
	}
	if _, g := c.do("GET", "/api/employee-codes", "EMP-1002", "HR Admin", nil); fmt.Sprint(g["noEmpId"]) != "[Permanent]" {
		t.Fatal(g)
	}
	// bgv off: the last approval issues nothing new for them, it only approves.
	c.do("PUT", "/api/onboarding/settings", "EMP-1002", "HR Admin", map[string]any{"hiring": map[string]any{"waitDays": 7, "bgvBlocks": false, "offerDays": 7, "offerRemindDays": 2}})
	all := nextCode(st)
	si, oi := hireAccepted(t, c, st, "Kiran", "Pal", "9876506001")
	if s, r := c.do("POST", "/api/candidates/"+si+"/joined", "EMP-1004", "HRBP", map[string]any{"date": today()}); s != 200 || r["id"] != "PRM-00001" {
		t.Fatalf("joined on the type's series: %v", r)
	}
	c.do("POST", "/api/offers/"+oi+"/joining", "EMP-1004", "HRBP", map[string]any{"data": joinData("ABCDE5555F")})
	for _, k := range []string{"pan", "photo", "police", "bankProof"} {
		c.upload("EMP-1004", "HRBP", "candidate", si, k)
	}
	_, v := c.do("POST", "/api/offers/"+oi+"/send", "EMP-1004", "HRBP", nil)
	ui3ApproveAll(t, c, v["id"].(string))
	e := stagesEmployee(st, "PRM-00001")
	if e["status"] != "Active" || e["stage"] != "Probation" || e["alias_ids"] != nil || nextCode(st) != all || stagesEmployee(st, "JI-0001")["id"] != nil {
		t.Fatalf("no Employee ID: %v %v %v next %s", e["status"], e["stage"], e["alias_ids"], nextCode(st))
	}
	if x := hireCand(t, c, si); x["bucket"] != "Done" || x["current_id"] != "PRM-00001" {
		t.Fatalf("done: %v %v", x["bucket"], x["current_id"])
	}
}

// Migration 010: candidates selected under the old rule keep the OI- number they were given.
func TestHireBackfillOfferNos(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	si := selectCandidate(t, c, shortlistBody(st, "Riya", "Sharma", "9876501234"))
	approvedOffer(t, c, st, si, "Riya", "Sharma", "9876501234")
	si2 := selectCandidate(t, c, shortlistBody(st, "Asha", "Rao", "9876502001"))
	if x := hireCand(t, c, si); x["current_id"] != si {
		t.Fatal(x["current_id"])
	}
	n, err := store.BackfillOfferNos(ctx, st.DB)
	if err != nil || n != 2 {
		t.Fatalf("backfill: %d %v", n, err)
	}
	if n, _ := store.BackfillOfferNos(ctx, st.DB); n != 0 {
		t.Fatal("runs once")
	}
	if x := hireCand(t, c, si); x["current_id"] != "OI-0001" || apiDoc(st, "offer", bson.D{{Key: "candidate_id", Value: si}})["offer_no"] != "OI-0001" {
		t.Fatalf("kept: %v", x["current_id"])
	}
	if x := hireCand(t, c, si2); x["current_id"] != "OI-0002" {
		t.Fatal(x["current_id"])
	}
}
