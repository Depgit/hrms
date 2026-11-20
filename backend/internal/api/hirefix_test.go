package api_test

// Hiring fixes of 18 Nov 2026 (hirefix.go): Role and Skill required on import and offer, SI-/OI-/JI- for every
// employee (migration 018's backfill), and the offer letter released at joining.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

const hfImportHead = "Employee Code,Full Name,Employment Type,Date of Joining,Work City,Reporting Location,Department,Designation,Grade,Shift,Reporting Manager L1,Functional Manager L2,Work Policy,Attendance Mode,Role,Skill,Mobile,Email,Date of Birth,Gender,PAN,Aadhaar,UAN,Existing UAN?,ESIC No,Bank Name,Account Number,IFSC,Status,Last Working Day,Opening CL Balance,Opening PL Balance,Opening SL Balance"
const hfImportBase = ",Permanent,2023-06-12,Gurugram,Gurugram – Sector 29 Hub,Front Office,Front Office Supervisor,G3,G,EMP-1009,,,,"

func TestHireFixRoleSkillRequired(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	// Import employees: both required, and the columns are no longer optional.
	file := hfImportHead + "\nRS-1,No Role" + hfImportBase + ",,,,,,,,,,,,,,,,,,,\nRS-2,Has Both" + hfImportBase + "Kitchen,Cooking,,,,,,,,,,,,,,,,,\n"
	s, r := c.sendFile("/api/bulk/new/check", "EMP-1002", "HR Admin", "new.csv", []byte(file))
	if s != 200 || r["ok"] != 1.0 {
		t.Fatal(s, r)
	}
	if e := b1Errs(r, 0); !strings.Contains(e, "Role is required") || !strings.Contains(e, "Skill is required") {
		t.Fatal(e)
	}
	noCols := strings.Replace(hfImportHead, "Role,Skill,", "", 1) + "\nRS-3,Old File" + strings.TrimSuffix(hfImportBase, ",") + ",,,,,,,,,,,,,,,,,\n"
	if s, r := c.sendFile("/api/bulk/new/check", "EMP-1002", "HR Admin", "new.csv", []byte(noCols)); s == 200 && r["ok"] == 1.0 {
		t.Fatalf("a file without Role and Skill: %v", r)
	}
	// The offer API refuses an offer without them, whatever Form setup says.
	si := selectCandidate(t, c, shortlistBody(st, "Neha", "Verma", "9876512345"))
	if s, r := c.upload("EMP-1004", "HRBP", "candidate", si, "aadhaar"); s != 200 {
		t.Fatal(r)
	}
	od := offerData(st, "Neha", "Verma", "9876512345")
	delete(od, "jobRole")
	delete(od, "jobSkill")
	s, r = c.do("POST", "/api/offers", "EMP-1004", "HRBP", map[string]any{"data": od, "candidate": si})
	if s != 400 || !strings.Contains(fmt.Sprint(r), "Role is required") || !strings.Contains(fmt.Sprint(r), "Skill is required") {
		t.Fatalf("offer without Role and Skill: %d %v", s, r)
	}
	// A job change of an employee without them needs them (Employee master update); existing ones are not blocked
	// otherwise.
	ctx := context.Background()
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1020"}, bson.M{"$set": bson.M{"job_role": nil, "skill": nil}}); err != nil {
		t.Fatal(err)
	}
	other := apiDoc(st, "location", bson.M{"_id": bson.M{"$ne": stagesEmployee(st, "EMP-1020")["location_id"]}, "status": "Active"})
	if s, r := c.do("PATCH", "/api/employees/EMP-1020/job", "EMP-1002", "HR Admin", map[string]any{"changes": map[string]string{"location": fmt.Sprint(other["name"])}, "reason": "Transfer", "effectiveFrom": today()}); s != 400 || !strings.Contains(fmt.Sprint(r), "Role and Skill are required") {
		t.Fatalf("job edit without Role and Skill: %d %v", s, r)
	}
}

func TestHireFixHireNoBackfill(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	ctx := context.Background()
	// A hire through a candidate keeps the candidate's number.
	si, _ := hireAccepted(t, c, st, "Asha", "Rao", "9876523456")
	if s, r := c.do("POST", "/api/candidates/"+si+"/joined", "EMP-1004", "HRBP", map[string]any{"date": today()}); s != 200 {
		t.Fatal(r)
	}
	ji := hireCand(t, c, si)["employee_id"].(string)
	total := apiCount(st, "employee", bson.M{})
	n, err := store.BackfillHireNos(ctx, st.DB)
	if err != nil || n != total-1 {
		t.Fatalf("backfill: %d of %d, %v", n, total, err)
	}
	if e := stagesEmployee(st, ji); toIntT(e["hire_no"]) != 1 {
		t.Fatalf("the candidate's number: %v", e["hire_no"])
	}
	seen := map[int]string{}
	emps, _ := st.DB.C("employee").Find(ctx, bson.M{})
	for _, e := range emps {
		no := toIntT(e["hire_no"])
		if no <= 0 || seen[no] != "" {
			t.Fatalf("%v: number %d (also %s)", e["id"], no, seen[no])
		}
		seen[no] = fmt.Sprint(e["id"])
	}
	_, tl := c.do("GET", "/api/employees/EMP-1020/timeline", "EMP-1002", "HR Admin", nil)
	ids := tl["hireIds"].(map[string]any)
	no := toIntT(stagesEmployee(st, "EMP-1020")["hire_no"])
	if ids["si"] != fmt.Sprintf("SI-%04d", no) || ids["oi"] != fmt.Sprintf("OI-%04d", no) || ids["ji"] != fmt.Sprintf("JI-%04d", no) || !strings.Contains(fmt.Sprint(tl["events"]), "Hiring IDs: SI-") {
		t.Fatalf("one number, on the timeline: %v %v", ids, tl["events"])
	}
	// Idempotent: nothing more, nothing changed.
	if n, err := store.BackfillHireNos(ctx, st.DB); err != nil || n != 0 || toIntT(stagesEmployee(st, "EMP-1020")["hire_no"]) != no {
		t.Fatalf("second run: %d %v", n, err)
	}
	if k := apiCount(st, store.LifecycleEventColl, bson.M{"employee_id": "EMP-1020", "action": bson.M{"$regex": "^Hiring IDs"}}); k != 1 {
		t.Fatalf("timeline entries: %d", k)
	}
	// Import employees takes the next number of the same series.
	file := hfImportHead + "\nHF-1,New Import" + hfImportBase + "Kitchen,Cooking,,,,,,,,,,,,,,,,,\n"
	if _, r := c.sendFile("/api/bulk/new/apply", "EMP-1002", "HR Admin", "new.csv", []byte(file)); r["ok"] != 1.0 {
		t.Fatal(r)
	}
	if got := toIntT(stagesEmployee(st, "HF-1")["hire_no"]); got != total+1 {
		t.Fatalf("import: %d, want %d", got, total+1)
	}
}

func TestHireFixLetterAtJoining(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	if s, r := c.do("PUT", "/api/onboarding/settings", "EMP-1002", "HR Admin", map[string]any{"hiring": map[string]any{"waitDays": 7, "bgvBlocks": true, "offerDays": 7,
		"offerRemindDays": 2, "letterJoinDepts": []string{"Front Office"}}}); s != 200 {
		t.Fatal(r)
	}
	si, oi := hireAccepted(t, c, st, "Meera", "Iyer", "9876534567")
	_, o := c.do("GET", "/api/offers/"+oi, "EMP-1004", "HRBP", nil)
	if o["letterHeld"] != true || o["letter"] != "" || o["letterAt"] != "joining" || o["letterRule"] == nil {
		t.Fatalf("held before joining: %v %q %v", o["letterHeld"], o["letter"], o["letterAt"])
	}
	if s, r := c.do("POST", "/api/candidates/"+si+"/joined", "EMP-1004", "HRBP", map[string]any{"date": today()}); s != 200 {
		t.Fatal(r)
	}
	_, o = c.do("GET", "/api/offers/"+oi, "EMP-1004", "HRBP", nil)
	if o["letterHeld"] != false || !strings.Contains(fmt.Sprint(o["letter"]), "Meera") || o["letterReleasedAt"] == nil {
		t.Fatalf("released at joining: %v %q", o["letterHeld"], o["letter"])
	}
	if apiCount(st, "audit_log", bson.M{"action": bson.M{"$regex": "^Offer letter released at joining"}}) != 1 {
		t.Fatal("the release is audited")
	}
}

func toIntT(v any) int {
	switch x := v.(type) {
	case int32:
		return int(x)
	case int64:
		return int(x)
	case float64:
		return int(x)
	}
	return 0
}

// withRoleSkill adds the Role and Skill columns (Kitchen, Cooking) after Attendance Mode to an older import file.
func withRoleSkill(csv string) string {
	lines := strings.Split(csv, "\n")
	for i, l := range lines {
		f := strings.Split(l, ",")
		if len(f) < 14 {
			continue
		}
		add := []string{"Kitchen", "Cooking"}
		if i == 0 {
			add = []string{"Role", "Skill"}
		}
		lines[i] = strings.Join(append(append(append([]string{}, f[:14]...), add...), f[14:]...), ",")
	}
	return strings.Join(lines, "\n")
}

// hireFixRoleSkill gives an employee a Role and Skill (a job change of someone without them is refused).
func hireFixRoleSkill(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if _, err := st.DB.C("employee").Update(context.Background(), bson.M{"_id": id}, bson.M{"$set": bson.M{"job_role": "Front desk", "skill": "Guest service"}}); err != nil {
		t.Fatal(err)
	}
}
