package api_test

// Onboarding in stages (onboarding_stages.go): SI-0001 → hiring manager → Function Head → interview → OI-0001 →
// offer approval → release → accept → Mark joined → JI-0001 → documents → Employee ID → Payroll → active.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// hubPlace is the Sector 29 Hub (location, city).
func hubPlace(st *store.Store) (string, string) {
	return stagesPlace(st, bson.D{{Key: "name", Value: "Gurugram – Sector 29 Hub"}})
}

// stagesPlace is the first location (by name) matching filter, with the city of its cluster: ("", "") if none.
func stagesPlace(st *store.Store, filter bson.D) (string, string) {
	ctx := context.Background()
	l, _ := st.DB.C("location").FindOne(ctx, filter, options.FindOne().SetSort(bson.D{{Key: "name", Value: 1}}))
	if l == nil {
		return "", ""
	}
	k, _ := st.DB.C("cluster").FindOne(ctx, bson.D{{Key: "_id", Value: l["cluster_id"]}})
	if k == nil {
		return "", ""
	}
	return fmt.Sprint(l["id"]), fmt.Sprint(k["city_id"])
}

// stagesEmployee reads an employee record ({} when there is none).
func stagesEmployee(st *store.Store, id string) store.Row {
	e, _ := st.DB.C("employee").FindOne(context.Background(), bson.D{{Key: "_id", Value: id}})
	if e == nil {
		return store.Row{}
	}
	return e
}

// stagesCount counts the documents of a collection matching filter.
func stagesCount(st *store.Store, coll string, filter bson.D) int {
	n, _ := st.DB.C(coll).Count(context.Background(), filter)
	return int(n)
}

// shortlistBody is a filled shortlist: a Front Office supervisor at the Sector 29 Hub, hiring manager EMP-1009.
func shortlistBody(st *store.Store, first, last, phone string) map[string]any {
	hub, city := hubPlace(st)
	return map[string]any{"data": map[string]any{"first": first, "last": last, "phone": phone, "email": strings.ToLower(first+"."+last) + "@example.com",
		"dob": "1996-04-12", "gender": "Female", "addr": "Sector 14, Gurugram", "ctc": 300000, "city": city, "loc": hub, "dept": "Front Office",
		"desig": "Front Office Supervisor", "manager": "EMP-1009"},
		"experience": []map[string]any{{"company": "Hotel Orchid", "designation": "Front Desk Associate", "from": "2021-06", "to": "2024-03"}}}
}

// offerData is the offer form for a shortlisted Front Office supervisor.
func offerData(st *store.Store, first, last, phone string) map[string]any {
	hub, city := hubPlace(st)
	return map[string]any{"first": first, "last": last, "type": "Permanent", "doj": today(), "dept": "Front Office", "desig": "Front Office Supervisor",
		"grade": "G3", "city": city, "loc": hub, "gross": 26000, "phone": phone, "email": strings.ToLower(first+"."+last) + "@example.com",
		"jobRole": "Front desk", "jobSkill": "Guest service"} // required like the department (owner, 14 Nov 2026)
}

func joinData(pan string) map[string]any {
	return map[string]any{"gender": "Female", "dob": "1996-04-12", "aadhaar": "1234123412" + pan[len(pan)-3:len(pan)-1], "pan": pan, "uanAvail": "No", "pfChoice": "Yes",
		"bankName": "HDFC Bank", "acct": "501001234567" + pan[len(pan)-3:len(pan)-1], "ifsc": "HDFC0001234", "shift": "G", "attMode": "Biometric", "l1": "EMP-1009"}
}

// selectCandidate shortlists as the HRBP (EMP-1004), approves the shortlist as HR Admin and records Selected as
// the hiring manager (EMP-1009). It returns the SI id.
func selectCandidate(t *testing.T, c *client, body map[string]any) string {
	t.Helper()
	s, r := c.do("POST", "/api/candidates", "EMP-1004", "HRBP", body)
	if s != 200 {
		t.Fatalf("shortlist: %v", r)
	}
	id := r["id"].(string)
	ui3ApproveAll(t, c, r["request"].(map[string]any)["id"].(string))
	if s, r := c.do("POST", "/api/candidates/"+id+"/interview", "EMP-1009", "Line Manager", map[string]any{"date": today(), "result": "Selected", "feedback": "Good with guests"}); s != 200 {
		t.Fatalf("interview: %v", r)
	}
	return id
}

// approvedOffer raises the offer for a selected candidate (with the Aadhaar card) and approves it. It returns OI-….
func approvedOffer(t *testing.T, c *client, st *store.Store, si, first, last, phone string) string {
	t.Helper()
	if s, r := c.upload("EMP-1004", "HRBP", "candidate", si, "aadhaar"); s != 200 {
		t.Fatalf("aadhaar: %v", r)
	}
	s, r := c.do("POST", "/api/offers", "EMP-1004", "HRBP", map[string]any{"data": offerData(st, first, last, phone), "candidate": si})
	if s != 200 {
		t.Fatalf("offer: %v", r)
	}
	ui3ApproveAll(t, c, r["request"].(map[string]any)["id"].(string))
	return r["id"].(string)
}

// nextCode is the Employee ID the code series gives next.
func nextCode(st *store.Store) string {
	var prefix string
	var pad, n int
	if x, _ := st.DB.C("employee_code_series").FindOne(context.Background(), bson.D{{Key: "key", Value: "All"}}); x != nil {
		prefix, _ = x["prefix"].(string)
		p, _ := x["pad"].(int32)
		next, _ := x["next_no"].(int32)
		pad, n = int(p), int(next)
	}
	return fmt.Sprintf("%s%0*d", prefix, pad, n)
}

func notices(c *client, emp, role string) string {
	_, n := c.do("GET", "/api/notices", emp, role, nil)
	out := []string{}
	for _, x := range n["notices"].([]any) {
		out = append(out, fmt.Sprint(x.(map[string]any)["text"]))
	}
	return strings.Join(out, " | ")
}

func TestStagesFullPath(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009", "EMP-1010", "EMP-1011", "EMP-1012", "EMP-1014")
	// The Function Head heads Front Office too, so the shortlist has both default stages with a name.
	if _, err := st.DB.C("user_role_map").UpdateMany(ctx, bson.D{{Key: "employee_id", Value: "EMP-1010"}, {Key: "role_name", Value: "Function Head"}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "departments", Value: []string{"Sales", "Front Office"}}}}}); err != nil {
		t.Fatal(err)
	}
	body := shortlistBody(st, "Riya", "Sharma", "9876501234")

	// Stage 1: the shortlist, its check and its route.
	_, chk := c.do("POST", "/api/candidates/check", "EMP-1004", "HRBP", body)
	if errs := chk["errors"].([]any); len(errs) != 0 || chk["next"] != "SI-0001" {
		t.Fatalf("check: %v", chk)
	}
	plan := chk["plan"].(map[string]any)
	if fmt.Sprint(plan["chain"]) != "[Line Manager Function Head]" || !strings.Contains(fmt.Sprint(plan["people"]), "Anil Kapoor") || !strings.Contains(fmt.Sprint(plan["people"]), "Rohit Khanna") {
		t.Fatalf("route: %v", plan)
	}
	bad := map[string]any{"data": map[string]any{"first": "X", "phone": "12", "dob": "2015-01-01", "manager": "EMP-1004"}, "experience": []map[string]any{{"company": "A", "designation": "B", "from": "2025-05", "to": "2024-01"}}}
	_, chk = c.do("POST", "/api/candidates/check", "EMP-1004", "HRBP", bad)
	for _, want := range []string{"Last name is required", "Mobile number must be 10 digits", "at least 18", "Existing CTC is required", "ends before it starts", "someone else raises it"} {
		if !strings.Contains(fmt.Sprint(chk["errors"]), want) {
			t.Fatalf("check misses %q: %v", want, chk["errors"])
		}
	}
	if s, _ := c.do("POST", "/api/candidates", "EMP-1014", "Employee", body); s != 403 {
		t.Fatal("only onboarding roles shortlist")
	}
	s, r := c.do("POST", "/api/candidates", "EMP-1004", "HRBP", body)
	if s != 200 || r["id"] != "SI-0001" {
		t.Fatalf("shortlist: %d %v", s, r)
	}
	slReq := r["request"].(map[string]any)["id"].(string)
	if s, r := c.upload("EMP-1004", "HRBP", "candidate", "SI-0001", "resume"); s != 200 {
		t.Fatal(r)
	}
	if s, r := c.do("POST", "/api/candidates", "EMP-1004", "HRBP", body); s != 400 || !strings.Contains(fmt.Sprint(r["details"]), "SI-0001") {
		t.Fatalf("the same mobile twice: %d %v", s, r)
	}
	_, x := c.do("GET", "/api/candidates/SI-0001", "EMP-1004", "HRBP", nil)
	if x["bucket"] != "Shortlisted" || x["current_id"] != "SI-0001" || !strings.Contains(fmt.Sprint(x["waiting"]), "Anil Kapoor") {
		t.Fatalf("shortlisted: %v %v %v", x["bucket"], x["current_id"], x["waiting"])
	}
	// The hiring manager, then the Function Head approve.
	if _, a := c.do("POST", "/api/approvals/"+slReq+"/approve", "EMP-1009", "Line Manager", map[string]string{"note": "Strong profile"}); a["next"] != "Function Head" {
		t.Fatalf("hiring manager: %v", a)
	}
	if _, a := c.do("POST", "/api/approvals/"+slReq+"/approve", "EMP-1010", "Function Head", map[string]string{"note": "Go ahead"}); a["status"] != "Approved" {
		t.Fatalf("function head: %v", a)
	}

	// Stage 2: the interview waits on the hiring manager, and only on them.
	_, x = c.do("GET", "/api/candidates/SI-0001", "EMP-1004", "HRBP", nil)
	ivReq := fmt.Sprint(x["interview_request_id"])
	if x["bucket"] != "Interview" || !strings.HasPrefix(ivReq, "INT-") {
		t.Fatalf("interview: %v %v", x["bucket"], ivReq)
	}
	if n := notices(c, "EMP-1009", "Line Manager"); !strings.Contains(n, "Interview waiting for you: Riya Sharma (SI-0001)") {
		t.Fatalf("the hiring manager is told: %s", n)
	}
	_, q := c.do("GET", "/api/approvals?tab=mine", "EMP-1009", "Line Manager", nil)
	if !strings.Contains(fmt.Sprint(q["list"]), ivReq) {
		t.Fatal("the interview is in the hiring manager's Approvals")
	}
	iv := map[string]any{"date": today(), "result": "Selected", "feedback": "Calm and clear with guests"}
	for _, who := range [][2]string{{"EMP-1002", "HR Admin"}, {"EMP-1010", "Function Head"}, {"EMP-1004", "HRBP"}} {
		if s, _ := c.do("POST", "/api/candidates/SI-0001/interview", who[0], who[1], iv); s != 403 {
			t.Fatalf("%s recorded the interview: %d", who[1], s)
		}
	}
	if s, _ := c.do("POST", "/api/approvals/"+ivReq+"/approve", "EMP-1002", "HR Admin", map[string]string{"note": "x"}); s != 400 {
		t.Fatal("no generic approve of an interview")
	}
	if s, _ := c.do("POST", "/api/candidates/SI-0001/interview", "EMP-1009", "Line Manager", map[string]any{"date": today(), "result": "Selected"}); s != 400 {
		t.Fatal("feedback is required")
	}
	if s, _ := c.do("POST", "/api/candidates/SI-0001/interview", "EMP-1009", "Line Manager", map[string]any{"date": time.Now().AddDate(0, 0, 2).Format("2006-01-02"), "result": "Selected", "feedback": "x"}); s != 400 {
		t.Fatal("no interview in the future")
	}
	// Selected issues no Offer ID: that comes when the candidate accepts (hiring lifecycle, 16 Nov 2026).
	if s, r := c.do("POST", "/api/candidates/SI-0001/interview", "EMP-1009", "Line Manager", iv); s != 200 || r["candidateId"] != "SI-0001" || r["offerId"] != nil {
		t.Fatalf("selected: %d %v", s, r)
	}
	if n := notices(c, "EMP-1004", "HRBP"); !strings.Contains(n, "Riya Sharma was selected at the interview (SI-0001)") {
		t.Fatalf("HR is told: %s", n)
	}

	// The offer is a version under SI-0001 (any of the candidate's numbers finds them). No direct offer without a candidate.
	_, x = c.do("GET", "/api/candidates/OI-0001", "EMP-1004", "HRBP", nil)
	if x["bucket"] != "Offer" || x["current_id"] != "SI-0001" || x["prefill"].(map[string]any)["l1"] != "EMP-1009" {
		t.Fatalf("offer stage: %v %v %v", x["bucket"], x["current_id"], x["prefill"])
	}
	od := offerData(st, "Riya", "Sharma", "9876501234")
	if s, _ := c.do("POST", "/api/offers", "EMP-1004", "HRBP", map[string]any{"data": od}); s != 400 {
		t.Fatal("every new hire starts at Shortlist")
	}
	_, chk = c.do("POST", "/api/offers/check", "EMP-1004", "HRBP", map[string]any{"data": od, "candidate": "OI-0001"})
	if fmt.Sprint(chk["errors"]) != "[Aadhaar card must be uploaded before the offer is released]" {
		t.Fatalf("offer check: %v", chk["errors"])
	}
	if s, r := c.upload("EMP-1004", "HRBP", "candidate", "SI-0001", "aadhaar"); s != 200 {
		t.Fatal(r)
	}
	s, r = c.do("POST", "/api/offers", "EMP-1004", "HRBP", map[string]any{"data": od, "candidate": "SI-0001"})
	if s != 200 || r["id"] != "OI-0001" {
		t.Fatalf("offer: %d %v", s, r)
	}
	if s, _ := c.do("POST", "/api/offers", "EMP-1004", "HRBP", map[string]any{"data": od, "candidate": "SI-0001"}); s != 400 {
		t.Fatal("one offer at a time")
	}
	before := nextCode(st)
	ui3ApproveAll(t, c, r["request"].(map[string]any)["id"].(string))
	_, o := c.do("GET", "/api/offers/OI-0001", "EMP-1004", "HRBP", nil)
	if o["status"] != "Approved" || o["employee_id"] != nil || nextCode(st) != before {
		t.Fatalf("no Employee ID on offer approval: %v %v %s", o["status"], o["employee_id"], nextCode(st))
	}
	c.do("POST", "/api/offers/OI-0001/release", "EMP-1004", "HRBP", nil)
	if s, _ := c.do("POST", "/api/candidates/OI-0001/joined", "EMP-1004", "HRBP", map[string]any{"date": today()}); s != 400 {
		t.Fatal("joined only after acceptance")
	}
	if _, a := c.do("POST", "/api/offers/OI-0001/accept", "EMP-1004", "HRBP", nil); !strings.Contains(fmt.Sprint(a["result"]), "Offer ID OI-0001") {
		t.Fatalf("accepted: %v", a)
	}

	// Stage 3: joined → JI-0001, a normal employee from day 1. The Employee ID waits for every approval.
	if s, _ := c.do("POST", "/api/candidates/OI-0001/joined", "EMP-1004", "HRBP", map[string]any{"date": time.Now().AddDate(0, 0, 1).Format("2006-01-02")}); s != 400 {
		t.Fatal("not in the future")
	}
	s, r = c.do("POST", "/api/candidates/OI-0001/joined", "EMP-1004", "HRBP", map[string]any{"date": today()})
	if s != 200 || r["id"] != "JI-0001" || r["employeeId"] != nil {
		t.Fatalf("joined: %d %v", s, r)
	}
	if ji := stagesEmployee(st, "JI-0001"); ji["status"] != "Active" || ji["stage"] != "Joining" || nextCode(st) != before {
		t.Fatalf("the Joining ID record: %v %v, next code %s", ji["status"], ji["stage"], nextCode(st))
	}
	_, x = c.do("GET", "/api/candidates/JI-0001", "EMP-1004", "HRBP", nil)
	if x["bucket"] != "Joined" || x["current_id"] != "JI-0001" {
		t.Fatalf("joined: %v %v", x["bucket"], x["current_id"])
	}
	if s, _ := c.do("POST", "/api/offers/OI-0001/send", "EMP-1004", "HRBP", nil); s != 400 {
		t.Fatal("documents come first")
	}
	if s, r := c.do("POST", "/api/offers/OI-0001/joining", "EMP-1004", "HRBP", map[string]any{"data": joinData("ABCDE1234F")}); s != 200 || r["employeeId"] != nil {
		t.Fatalf("joining details: %d %v", s, r)
	}
	for _, k := range []string{"pan", "photo", "police"} {
		if s, r := c.upload("EMP-1004", "HRBP", "candidate", "SI-0001", k); s != 200 || r["note"] != nil {
			t.Fatalf("%s: %v", k, r)
		}
	}
	if nextCode(st) != before {
		t.Fatal("no Employee ID before the last mandatory document")
	}
	if _, r = c.upload("EMP-1004", "HRBP", "candidate", "SI-0001", "bankProof"); r["note"] != nil || nextCode(st) != before {
		t.Fatalf("documents alone issue no Employee ID: %v", r)
	}
	emp := "JI-0001"
	e := stagesEmployee(st, emp)
	status, doj := fmt.Sprint(e["status"]), fmt.Sprint(e["doj"])
	if status != "Active" || doj != today() {
		t.Fatalf("employee %s: %s %s", emp, status, doj)
	}
	// The candidate's documents, and those of them not on the record (one without an employee_id is not on it either).
	kinds := stagesCount(st, "document", bson.D{{Key: "owner_id", Value: "SI-0001"}, {Key: "owner_kind", Value: "candidate"}})
	unlinked := stagesCount(st, "document", bson.D{{Key: "owner_id", Value: "SI-0001"}, {Key: "employee_id", Value: bson.D{{Key: "$ne", Value: emp}}}})
	if kinds != 6 || unlinked != 0 {
		t.Fatalf("documents: %d with the candidate, %d not on the record", kinds, unlinked)
	}

	// The background check (it blocks the Employee ID by default), then Payroll verification → the Employee ID.
	c.upload("EMP-1004", "HRBP", "candidate", "SI-0001", "bgvReport")
	if s, b := c.do("POST", "/api/candidates/SI-0001/bgv", "EMP-1004", "HRBP", map[string]string{"status": "Clear"}); s != 200 || b["employeeId"] != nil {
		t.Fatalf("background check: %d %v", s, b)
	}
	s, v := c.do("POST", "/api/offers/OI-0001/send", "EMP-1004", "HRBP", nil)
	if s != 200 {
		t.Fatal(v)
	}
	_, x = c.do("GET", "/api/candidates/SI-0001", "EMP-1004", "HRBP", nil)
	if x["bucket"] != "With Payroll" {
		t.Fatal(x["bucket"])
	}
	vid := v["id"].(string)
	c.do("POST", "/api/approvals/"+vid+"/approve", "EMP-1011", "Payroll", nil)
	if _, a := c.do("POST", "/api/approvals/"+vid+"/approve", "EMP-1012", "Payroll Manager", nil); a["status"] != "Approved" {
		t.Fatal(a)
	}
	emp = before
	e = stagesEmployee(st, emp)
	status = fmt.Sprint(e["status"])
	mgr, _ := e["manager_id"].(string)
	if status != "Active" || mgr != "EMP-1009" || e["stage"] != "Probation" || fmt.Sprint(e["alias_ids"]) != "[JI-0001]" || stagesEmployee(st, "JI-0001")["id"] != nil {
		t.Fatalf("active on %s: %s %s %v %v", emp, status, mgr, e["stage"], e["alias_ids"])
	}
	_, x = c.do("GET", "/api/candidates/SI-0001", "EMP-1004", "HRBP", nil)
	ids := []string{}
	for _, tr := range x["trail"].([]any) {
		ids = append(ids, fmt.Sprint(tr.(map[string]any)["id"]))
	}
	if x["bucket"] != "Done" || x["current_id"] != emp || strings.Join(ids, " → ") != "SI-0001 → OI-0001 → JI-0001 → "+emp {
		t.Fatalf("done: %v %v %v", x["bucket"], x["current_id"], ids)
	}
	_, l := c.do("GET", "/api/candidates", "EMP-1004", "HRBP", nil)
	if l["counts"].(map[string]any)["Done"] != 1.0 {
		t.Fatalf("pipeline counts: %v", l["counts"])
	}
}

// The old "at joining" setting applies only to candidates who joined before the Joining ID record; a joiner now is
// always a normal employee on JI- from the joining day, and the Employee ID waits for every approval.
func TestStagesEmployeeIDAtJoining(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	if s, _ := c.do("PUT", "/api/onboarding/settings", "EMP-1004", "HRBP", map[string]string{"employeeIdAt": "joining"}); s != 403 {
		t.Fatal("onboarding setup only")
	}
	if s, r := c.do("PUT", "/api/onboarding/settings", "EMP-1002", "HR Admin", map[string]string{"employeeIdAt": "joining"}); s != 200 {
		t.Fatal(r)
	}
	if _, g := c.do("GET", "/api/onboarding/settings", "EMP-1004", "HRBP", nil); g["employeeIdAt"] != "joining" {
		t.Fatal(g)
	}
	si := selectCandidate(t, c, shortlistBody(st, "Tara", "Mehta", "9876501299"))
	oi := approvedOffer(t, c, st, si, "Tara", "Mehta", "9876501299")
	c.do("POST", "/api/offers/"+oi+"/release", "EMP-1004", "HRBP", nil)
	c.do("POST", "/api/offers/"+oi+"/accept", "EMP-1004", "HRBP", nil)
	want := nextCode(st)
	s, r := c.do("POST", "/api/candidates/"+si+"/joined", "EMP-1004", "HRBP", map[string]any{"date": today()})
	if s != 200 || r["employeeId"] != nil || r["id"] != "JI-0001" || nextCode(st) != want {
		t.Fatalf("joined: %d %v (next code still %s)", s, r, want)
	}
	status, _ := stagesEmployee(st, "JI-0001")["status"].(string)
	if status != "Active" {
		t.Fatal(status)
	}
	if s, _ := c.do("POST", "/api/candidates/"+si+"/employee-id", "EMP-1004", "HRBP", nil); s != 400 {
		t.Fatal("no Employee ID on request for a Joining ID")
	}
}

// Rejected, not selected, declined and not joined use no Employee ID; numbers are never reused.
func TestStagesClosedUseNoEmployeeID(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	before := nextCode(st)
	employees := stagesCount(st, "employee", bson.D{})

	// Rejected by the hiring manager.
	_, r := c.do("POST", "/api/candidates", "EMP-1004", "HRBP", shortlistBody(st, "Asha", "Rao", "9876502001"))
	if s, a := c.do("POST", "/api/approvals/"+r["request"].(map[string]any)["id"].(string)+"/reject", "EMP-1009", "Line Manager", map[string]string{"note": "Not enough experience"}); s != 200 {
		t.Fatal(a)
	}
	// Not selected at the interview.
	_, r = c.do("POST", "/api/candidates", "EMP-1004", "HRBP", shortlistBody(st, "Bina", "Das", "9876502002"))
	ui3ApproveAll(t, c, r["request"].(map[string]any)["id"].(string))
	if s, a := c.do("POST", "/api/candidates/SI-0002/interview", "EMP-1009", "Line Manager", map[string]any{"date": today(), "result": "Not selected", "feedback": "Weak English"}); s != 200 || a["status"] != "Rejected" {
		t.Fatalf("not selected: %v", a)
	}
	// Declined the offer.
	si3 := selectCandidate(t, c, shortlistBody(st, "Chitra", "Iyer", "9876502003"))
	oi3 := approvedOffer(t, c, st, si3, "Chitra", "Iyer", "9876502003")
	c.do("POST", "/api/offers/"+oi3+"/release", "EMP-1004", "HRBP", nil)
	if s, a := c.do("POST", "/api/offers/"+oi3+"/decline", "EMP-1004", "HRBP", map[string]string{"reason": "Took another offer"}); s != 200 {
		t.Fatal(a)
	}
	// Accepted, never came.
	si4 := selectCandidate(t, c, shortlistBody(st, "Deepa", "Nair", "9876502004"))
	oi4 := approvedOffer(t, c, st, si4, "Deepa", "Nair", "9876502004")
	c.do("POST", "/api/offers/"+oi4+"/release", "EMP-1004", "HRBP", nil)
	c.do("POST", "/api/offers/"+oi4+"/accept", "EMP-1004", "HRBP", nil)
	if s, _ := c.do("POST", "/api/candidates/"+si4+"/not-joined", "EMP-1004", "HRBP", map[string]string{}); s != 400 {
		t.Fatal("a reason is needed")
	}
	if s, a := c.do("POST", "/api/candidates/"+si4+"/not-joined", "EMP-1004", "HRBP", map[string]string{"reason": "Did not report on the day"}); s != 200 {
		t.Fatal(a)
	}
	if s, _ := c.do("POST", "/api/candidates/"+si4+"/joined", "EMP-1004", "HRBP", map[string]any{"date": today()}); s != 400 {
		t.Fatal("a closed candidate cannot join")
	}
	if s, _ := c.do("POST", "/api/offers/"+oi4+"/joining", "EMP-1004", "HRBP", map[string]any{"data": joinData("ABCDE1111F")}); s != 400 {
		t.Fatal("no joining details for a closed candidate")
	}
	_, l := c.do("GET", "/api/candidates?stage=Closed", "EMP-1004", "HRBP", nil)
	kinds := map[string]string{}
	for _, x := range l["rows"].([]any) {
		m := x.(map[string]any)
		kinds[fmt.Sprint(m["id"])] = fmt.Sprint(m["closed_kind"]) + " " + fmt.Sprint(m["current_id"])
	}
	// The Offer ID exists only for the one who accepted.
	want := map[string]string{"SI-0001": "Rejected SI-0001", "SI-0002": "Not selected SI-0002", "SI-0003": "Declined SI-0003", "SI-0004": "Not joined OI-0004"}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("closed: %v", kinds)
	}
	now := stagesCount(st, "employee", bson.D{})
	if nextCode(st) != before || now != employees {
		t.Fatalf("an Employee ID was used: next %s (was %s), %d employees (was %d)", nextCode(st), before, now, employees)
	}
	// A new shortlist takes the next number; closed ones are never reused.
	if _, r := c.do("POST", "/api/candidates", "EMP-1004", "HRBP", shortlistBody(st, "Asha", "Rao", "9876502001")); r["id"] != "SI-0005" {
		t.Fatalf("next number: %v", r)
	}
}

// Rights and scope: another location's HR, the Function Head's department, My view.
func TestStagesScope(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1008", "EMP-1009", "EMP-1010")
	blr, blrCity := stagesPlace(st, bson.D{{Key: "name", Value: bson.D{{Key: "$regex", Value: "^Bengaluru"}}}})
	south := shortlistBody(st, "Esha", "Kumar", "9876503001")
	south["data"].(map[string]any)["loc"], south["data"].(map[string]any)["city"] = blr, blrCity
	south["data"].(map[string]any)["dept"], south["data"].(map[string]any)["desig"] = "Housekeeping", "Housekeeping Supervisor"
	south["data"].(map[string]any)["manager"] = "EMP-1008"
	if s, _ := c.do("POST", "/api/candidates", "EMP-1004", "HRBP", south); s != 403 {
		t.Fatalf("the HRBP shortlists only for their locations: %d", s)
	}
	s, r := c.do("POST", "/api/candidates", "EMP-1002", "HR Admin", south)
	if s != 200 {
		t.Fatal(r)
	}
	southID := r["id"].(string)
	c.upload("EMP-1002", "HR Admin", "candidate", southID, "resume")
	sales := shortlistBody(st, "Farah", "Ali", "9876503002")
	sales["data"].(map[string]any)["dept"], sales["data"].(map[string]any)["desig"] = "Sales", "Sales Executive"
	_, r = c.do("POST", "/api/candidates", "EMP-1004", "HRBP", sales)
	salesID := r["id"].(string)
	_, r = c.do("POST", "/api/candidates", "EMP-1004", "HRBP", shortlistBody(st, "Gita", "Bose", "9876503003"))
	frontID := r["id"].(string)

	_, l := c.do("GET", "/api/candidates", "EMP-1004", "HRBP", nil)
	if strings.Contains(fmt.Sprint(l["rows"]), southID) || !strings.Contains(fmt.Sprint(l["rows"]), salesID) {
		t.Fatal("another location's candidate is listed for the HRBP")
	}
	if s, _ := c.do("GET", "/api/candidates/"+southID, "EMP-1004", "HRBP", nil); s != 403 {
		t.Fatal("another location's candidate opens for the HRBP")
	}
	if s, _ := c.do("GET", "/api/files?ownerKind=candidate&ownerId="+southID, "EMP-1004", "HRBP", nil); s != 403 {
		t.Fatal("another location's résumé")
	}
	// Its hiring manager (EMP-1008, Property Manager of another place) reads it while deciding.
	if s, _ := c.do("GET", "/api/candidates/"+southID, "EMP-1008", "Property Manager", nil); s != 200 {
		t.Fatal("the hiring manager reads their candidate from any role view")
	}
	if s, _ := c.do("GET", "/api/files?ownerKind=candidate&ownerId="+southID, "EMP-1008", "Property Manager", nil); s != 200 {
		t.Fatal("the hiring manager reads the résumé")
	}
	// The Function Head (Sales) reads the Sales candidate they approve, not a Front Office one.
	if s, _ := c.do("GET", "/api/candidates/"+salesID, "EMP-1010", "Function Head", nil); s != 200 {
		t.Fatal("the Function Head reads their department's candidate")
	}
	if s, _ := c.do("GET", "/api/candidates/"+frontID, "EMP-1010", "Function Head", nil); s != 403 {
		t.Fatal("the Function Head reads another department's candidate")
	}
	if s, _ := c.do("GET", "/api/candidates", "EMP-1010", "Function Head", nil); s != 403 {
		t.Fatal("the pipeline is for onboarding roles")
	}
	// My view never shows candidates.
	if s, _ := c.do("GET", "/api/candidates/"+frontID, "EMP-1009", "Employee", nil); s != 403 {
		t.Fatal("a candidate in My view")
	}
	_, x := c.do("GET", "/api/candidates/"+frontID, "EMP-1004", "HRBP", nil)
	req := fmt.Sprint(x["shortlist_request_id"])
	if s, _ := c.do("GET", "/api/approvals/"+req, "EMP-1004", "Employee", nil); s != 403 {
		t.Fatal("the raiser's My view opens a candidate's request")
	}
	_, q := c.do("GET", "/api/approvals?tab=open", "EMP-1004", "Employee", nil)
	if strings.Contains(fmt.Sprint(q["list"]), "SI-") {
		t.Fatal("candidates in My view's requests")
	}
	// The hiring manager sees the shortlist in their Approvals and opens the candidate (their team scope).
	_, q = c.do("GET", "/api/approvals?tab=mine", "EMP-1009", "Line Manager", nil)
	if !strings.Contains(fmt.Sprint(q["list"]), req) {
		t.Fatal("the shortlist is not waiting on the hiring manager")
	}
	if s, _ := c.do("GET", "/api/candidates/"+frontID, "EMP-1009", "Line Manager", nil); s != 200 {
		t.Fatal("the hiring manager reads the candidate")
	}
	if s, _ := c.do("GET", "/api/files?ownerKind=candidate&ownerId="+frontID, "EMP-1009", "Line Manager", nil); s != 200 {
		t.Fatal("the hiring manager reads the résumé")
	}
	if s, _ := c.upload("EMP-1009", "Line Manager", "candidate", frontID, "resume"); s != 403 {
		t.Fatal("only onboarding roles upload")
	}
}

// Rejoin starts a shortlist prefilled from the old record and can keep the old code.
func TestStagesRejoin(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	// The first of EMP-1009's team at the Sector 29 Hub leaves (last working day ten days ago).
	hub, _ := hubPlace(st)
	var old string
	if x, _ := st.DB.C("employee").FindOne(ctx, bson.D{{Key: "location_id", Value: hub}, {Key: "manager_id", Value: "EMP-1009"}},
		options.FindOne().SetSort(bson.D{{Key: "_id", Value: 1}})); x != nil {
		old = fmt.Sprint(x["id"])
	}
	day, _ := time.Parse("2006-01-02", store.Today())
	if _, err := st.DB.C("employee").Update(ctx, bson.D{{Key: "_id", Value: old}}, bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "Inactive"},
		{Key: "lwd", Value: day.AddDate(0, 0, -10).Format("2006-01-02")}, {Key: "exit_reason", Value: "Resigned"}}}}); err != nil {
		t.Fatal(err)
	}
	if s, _ := c.do("POST", "/api/employees/"+old+"/rejoin", "EMP-1002", "HR Admin", map[string]any{}); s != 400 {
		t.Fatal("no direct rejoin offer")
	}
	s, pre := c.do("GET", "/api/candidates/rejoin/"+old, "EMP-1004", "HRBP", nil)
	if s != 200 {
		t.Fatal(pre)
	}
	data := pre["data"].(map[string]any)
	if data["rejoinOf"] != old || data["manager"] != "EMP-1009" || len(pre["experience"].([]any)) != 1 {
		t.Fatalf("prefill: %v", pre)
	}
	data["codeMode"], data["ctc"], data["dob"], data["gender"] = "same", 250000, "1995-01-01", "Male"
	if data["phone"] == nil {
		data["phone"] = "9876504001"
	}
	if data["email"] == nil {
		data["email"] = "rejoin@example.com"
	}
	body := map[string]any{"data": data, "experience": pre["experience"]}
	si := selectCandidate(t, c, body)
	_, e := c.do("GET", "/api/employees/"+old, "EMP-1002", "HR Admin", nil)
	if u7map(e["rejoin"])["pending"] != true {
		t.Fatalf("pending rejoin: %v", e["rejoin"])
	}
	if s, _ := c.do("POST", "/api/candidates", "EMP-1004", "HRBP", body); s != 400 {
		t.Fatal("a second rejoin while one is open")
	}
	if s, _ := c.do("POST", "/api/employees/"+old+"/reactivate", "EMP-1002", "HR Admin", nil); s != 400 {
		t.Fatal("undo inactive while a rejoin is open")
	}
	first, last, _ := strings.Cut(fmt.Sprint(data["first"])+" "+fmt.Sprint(data["last"]), " ")
	if s, r := c.upload("EMP-1004", "HRBP", "candidate", si, "aadhaar"); s != 200 {
		t.Fatal(r)
	}
	od := offerData(st, first, last, fmt.Sprint(data["phone"]))
	od["email"] = data["email"]
	s, r := c.do("POST", "/api/offers", "EMP-1004", "HRBP", map[string]any{"data": od, "candidate": si})
	if s != 200 {
		t.Fatal(r)
	}
	ui3ApproveAll(t, c, r["request"].(map[string]any)["id"].(string))
	oi := r["id"].(string)
	_, o := c.do("GET", "/api/offers/"+oi, "EMP-1004", "HRBP", nil)
	if o["rejoin_of"] != old || u7map(o["data"])["codeMode"] != "same" {
		t.Fatalf("rejoin offer: %v %v", o["rejoin_of"], o["data"])
	}
	c.do("POST", "/api/offers/"+oi+"/release", "EMP-1004", "HRBP", nil)
	c.do("POST", "/api/offers/"+oi+"/accept", "EMP-1004", "HRBP", nil)
	// A rejoiner who keeps their code is on it from the joining day: no Joining ID, no re-key later.
	if s, r := c.do("POST", "/api/candidates/"+si+"/joined", "EMP-1004", "HRBP", map[string]any{"date": today()}); s != 200 || r["id"] != old {
		t.Fatalf("rejoined on the same code: %d %v", s, r)
	}
	status, _ := stagesEmployee(st, old)["status"].(string)
	periods := stagesCount(st, "employee_service", bson.D{{Key: "employee_id", Value: old}})
	if status != "Active" || periods != 1 || stagesEmployee(st, "JI-0001")["id"] != nil {
		t.Fatalf("rejoin: %s, %d earlier periods", status, periods)
	}
}

// An offer raised before stages (OFR-) still finishes as before: approval makes the pre-boarding employee.
func TestStagesLegacyOfferFinishes(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1003", "EMP-1004", "EMP-1011", "EMP-1012")
	hub, city := hubPlace(st)
	data := fmt.Sprintf(`{"first":"Old","last":"Offer","type":"Permanent","dept":"Front Office","desig":"Front Office Supervisor","grade":"G3","city":%q,"loc":%q,"gross":"26000","phone":"9876505001","email":"old.offer@example.com"}`, city, hub)
	// The offer for a Front Office Supervisor, its approval waiting on the HRBP Corporate, as raised before stages.
	dept, _ := st.DB.C("department").FindOne(ctx, bson.D{{Key: "name", Value: "Front Office"}})
	if dept == nil {
		t.Fatal("no Front Office department")
	}
	desig, _ := st.DB.C("designation").FindOne(ctx, bson.D{{Key: "department_id", Value: dept["id"]}, {Key: "name", Value: "Front Office Supervisor"}})
	if desig == nil {
		t.Fatal("no Front Office Supervisor designation")
	}
	if _, err := st.DB.C("offer").Insert(ctx, bson.D{{Key: "_id", Value: "OFR-1001"}, {Key: "candidate_name", Value: "Old Offer"}, {Key: "type", Value: "Permanent"},
		{Key: "doj", Value: store.Today()}, {Key: "department_id", Value: dept["id"]}, {Key: "designation_id", Value: desig["id"]}, {Key: "grade", Value: "G3"},
		{Key: "location_id", Value: hub}, {Key: "gross", Value: 26000}, {Key: "band_pos", Value: "within"}, {Key: "data", Value: data}, {Key: "raised_by", Value: "EMP-1004"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.C("approval_request").Insert(ctx, bson.D{{Key: "_id", Value: "REQ-9001"}, {Key: "wf", Value: "joining"}, {Key: "offer_id", Value: "OFR-1001"},
		{Key: "raised_by", Value: "EMP-1004"}, {Key: "flow", Value: "Operations"}, {Key: "facts", Value: `{"wf":"joining"}`}, {Key: "payload", Value: `{"offerId":"OFR-1001"}`},
		{Key: "chain", Value: `["HRBP Corporate"]`}, {Key: "step", Value: 0}, {Key: "status", Value: "Pending"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.C("offer").Update(ctx, bson.D{{Key: "_id", Value: "OFR-1001"}}, bson.D{{Key: "$set", Value: bson.D{{Key: "request_id", Value: "REQ-9001"}}}}); err != nil {
		t.Fatal(err)
	}
	_, l := c.do("GET", "/api/offers?legacy=1", "EMP-1004", "HRBP", nil)
	if !strings.Contains(fmt.Sprint(l["list"]), "OFR-1001") {
		t.Fatal("the legacy offer is listed")
	}
	if s, r := c.upload("EMP-1004", "HRBP", "offer", "OFR-1001", "aadhaar"); s != 200 {
		t.Fatal(r)
	}
	want := nextCode(st)
	if _, a := c.do("POST", "/api/approvals/REQ-9001/approve", "EMP-1003", "HRBP Corporate", map[string]string{"note": "ok"}); a["status"] != "Approved" {
		t.Fatal(a)
	}
	_, o := c.do("GET", "/api/offers/OFR-1001", "EMP-1004", "HRBP", nil)
	if o["status"] != "Approved" || o["employee_id"] != want {
		t.Fatalf("legacy approval makes the pre-boarding employee %s: %v %v", want, o["status"], o["employee_id"])
	}
	c.do("POST", "/api/offers/OFR-1001/release", "EMP-1004", "HRBP", nil)
	c.do("POST", "/api/offers/OFR-1001/accept", "EMP-1004", "HRBP", nil)
	jd := joinData("ABCDE9876F")
	jd["jobRole"], jd["jobSkill"] = "Front desk", "Guest service" // an offer made before Role and Skill were asked (hirefix.go)
	if s, r := c.do("POST", "/api/offers/OFR-1001/joining", "EMP-1004", "HRBP", map[string]any{"data": jd}); s != 200 {
		t.Fatal(r)
	}
	for _, k := range []string{"pan", "photo", "bankProof", "police"} {
		if s, r := c.upload("EMP-1004", "HRBP", "offer", "OFR-1001", k); s != 200 {
			t.Fatal(r)
		}
	}
	s, v := c.do("POST", "/api/offers/OFR-1001/send", "EMP-1004", "HRBP", nil)
	if s != 200 {
		t.Fatal(v)
	}
	vid := v["id"].(string)
	c.do("POST", "/api/approvals/"+vid+"/approve", "EMP-1011", "Payroll", nil)
	c.do("POST", "/api/approvals/"+vid+"/approve", "EMP-1012", "Payroll Manager", nil)
	status, _ := stagesEmployee(st, want)["status"].(string)
	linked := stagesCount(st, "document", bson.D{{Key: "owner_kind", Value: "offer"}, {Key: "owner_id", Value: "OFR-1001"}, {Key: "employee_id", Value: want}})
	if status != "Active" || linked != 5 {
		t.Fatalf("legacy offer finished: %s, %d documents on the record", status, linked)
	}
	_, cl := c.do("GET", "/api/candidates", "EMP-1004", "HRBP", nil)
	if len(cl["rows"].([]any)) != 0 {
		t.Fatal("a legacy offer is not a candidate")
	}
}
