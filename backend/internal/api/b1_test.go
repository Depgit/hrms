package api_test

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// The owner's requests of 14 Nov 2026 (Builder 1): PAN verification, job role and skill masters, Employee master
// update columns, ESIC and medical cards, opening optional holidays, import without a gross, children, Existing UAN?.

const b1PDF = "%PDF-1.4\n1 0 obj << >> endobj\ntrailer << >>\n%%EOF\n"

func b1Errs(r map[string]any, i int) string {
	res := r["results"].([]any)
	return strings.Join(toStrs(res[i].(map[string]any)["errors"].([]any)), "; ")
}

func b1Changes(r map[string]any, i int) string {
	out := []string{}
	for _, x := range r["results"].([]any)[i].(map[string]any)["changes"].([]any) {
		ch := x.(map[string]any)
		out = append(out, fmt.Sprintf("%s: %v → %v (%v)", ch["field"], ch["from"], ch["to"], ch["note"]))
	}
	return strings.Join(out, "; ")
}

func b1SetPAN(t *testing.T, st *store.Store, id, pan string) {
	t.Helper()
	if _, err := st.DB.C("employee_gov_id").Upsert(context.Background(), bson.M{"employee_id": id}, bson.M{"pan": pan}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestB1PanVerification(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009", "EMP-1014")
	// The setting is HR Admin's, off until set.
	if s, _ := c.do("GET", "/api/settings/pan-verify", "EMP-1004", "HRBP", nil); s != 403 {
		t.Fatal("HR Admin only")
	}
	if s, _ := c.do("PUT", "/api/settings/pan-verify", "EMP-1004", "HRBP", map[string]any{"provider": "Test", "on": true}); s != 403 {
		t.Fatal("HR Admin only")
	}
	_, g := c.do("GET", "/api/settings/pan-verify", "EMP-1002", "HR Admin", nil)
	// Default (19 Nov): the offline pattern check, on. Nothing is sent anywhere; the name is not compared.
	if g["provider"] != "Format check" || g["on"] != true || g["ready"] != true {
		t.Fatalf("default: %v", g)
	}
	b1SetPAN(t, st, "EMP-1020", "ABCPZ1234L") // surname K, 5th letter Z: the pattern check ignores the name
	if s, r := c.do("POST", "/api/employees/EMP-1020/pan/verify", "EMP-1002", "HR Admin", nil); s != 200 || !strings.Contains(fmt.Sprint(r), "Verified") {
		t.Fatalf("format check: %d %v", s, r)
	}
	b1SetPAN(t, st, "EMP-1020", "ABCCK1234L") // 4th letter C: a company's PAN
	if s, r := c.do("POST", "/api/employees/EMP-1020/pan/verify", "EMP-1002", "HR Admin", nil); s != 200 || !strings.Contains(fmt.Sprint(r), "Invalid") {
		t.Fatalf("format check invalid: %d %v", s, r)
	}
	b1SetPAN(t, st, "EMP-1020", "ABCPK1234L") // Karan Khan: surname K
	if s, _ := c.do("PUT", "/api/settings/pan-verify", "EMP-1002", "HR Admin", map[string]any{"provider": "Format check", "on": false}); s != 200 {
		t.Fatal("switch off")
	}
	if s, r := c.do("POST", "/api/employees/EMP-1020/pan/verify", "EMP-1002", "HR Admin", nil); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "off") {
		t.Fatalf("off: %d %v", s, r)
	}
	for _, bad := range []map[string]any{{"provider": "Acme", "on": true}, {"provider": "None", "on": true}, {"provider": "Surepass", "on": false, "baseUrl": "http://x.example"}} {
		if s, _ := c.do("PUT", "/api/settings/pan-verify", "EMP-1002", "HR Admin", bad); s != 400 {
			t.Fatalf("refused: %v", bad)
		}
	}
	// A real provider is a stub: not configured without a key, and still nothing sent with one. The key is sealed.
	_, r := c.do("PUT", "/api/settings/pan-verify", "EMP-1002", "HR Admin", map[string]any{"provider": "Surepass", "on": true, "baseUrl": "https://kyc.example.com"})
	if r["ready"] != false || r["keySet"] != false {
		t.Fatalf("no key: %v", r)
	}
	if s, r := c.do("POST", "/api/employees/EMP-1020/pan/verify", "EMP-1002", "HR Admin", nil); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "not configured") {
		t.Fatalf("stub: %d %v", s, r)
	}
	_, r = c.do("PUT", "/api/settings/pan-verify", "EMP-1002", "HR Admin", map[string]any{"provider": "Surepass", "on": true, "apiKey": "sk_live_secret_9876"})
	if r["keySet"] != true || r["keyLast4"] != "9876" || r["apiKey"] != nil {
		t.Fatalf("key: %v", r)
	}
	cs := apiDoc(st, "company_setting", bson.M{"_id": 1})
	if raw := fmt.Sprint(cs["pan_verify"]); strings.Contains(raw, "sk_live_secret") {
		t.Fatal("the key is stored in plain text")
	}
	if s, r := c.do("POST", "/api/employees/EMP-1020/pan/verify", "EMP-1002", "HR Admin", nil); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "not built yet") {
		t.Fatalf("stub with a key: %d %v", s, r)
	}
	// Saving without apiKey keeps the key.
	_, r = c.do("PUT", "/api/settings/pan-verify", "EMP-1002", "HR Admin", map[string]any{"provider": "Test", "on": true})
	if r["keySet"] != true || r["ready"] != true {
		t.Fatalf("key kept: %v", r)
	}

	// The Test provider: 4th letter P and the surname's initial → Verified; another initial → Name mismatch; not P → Invalid.
	if s, _ := c.do("POST", "/api/employees/EMP-1020/pan/verify", "EMP-1014", "Employee", nil); s != 403 {
		t.Fatalf("an employee without the government ID right at Edit: %d", s)
	}
	s, r := c.do("POST", "/api/employees/EMP-1020/pan/verify", "EMP-1002", "HR Admin", nil)
	if s != 200 || r["status"] != "Verified" || r["name"] != "KARAN KHAN" {
		t.Fatalf("verified: %d %v", s, r)
	}
	_, e := c.do("GET", "/api/employees/EMP-1020", "EMP-1002", "HR Admin", nil)
	pc := e["govId"].(map[string]any)["panCheck"].(map[string]any)
	if pc["status"] != "Verified" || pc["name"] != "KARAN KHAN" || pc["at"] == nil || !strings.Contains(fmt.Sprint(pc["by"]), "EMP-1002") {
		t.Fatalf("record: %v", pc)
	}
	if a := apiDoc(st, "audit_log", bson.M{"action": "PAN checked for EMP-1020: Verified"}); a == nil || strings.Contains(fmt.Sprint(a), "ABCPK1234L") {
		t.Fatalf("audited without the full PAN: %v", a)
	}
	b1SetPAN(t, st, "EMP-1020", "ABCPZ1234L")
	_, e = c.do("GET", "/api/employees/EMP-1020", "EMP-1002", "HR Admin", nil)
	if pc := e["govId"].(map[string]any)["panCheck"].(map[string]any); pc["status"] != "Not checked" {
		t.Fatalf("a new PAN is not checked: %v", pc)
	}
	if _, r = c.do("POST", "/api/employees/EMP-1020/pan/verify", "EMP-1002", "HR Admin", nil); r["status"] != "Name mismatch" {
		t.Fatalf("mismatch: %v", r)
	}
	b1SetPAN(t, st, "EMP-1020", "ABCCK1234L")
	if _, r = c.do("POST", "/api/employees/EMP-1020/pan/verify", "EMP-1002", "HR Admin", nil); r["status"] != "Invalid" {
		t.Fatalf("invalid: %v", r)
	}
	if g := apiDoc(st, "employee_gov_id", bson.M{"employee_id": "EMP-1020"}); g["pan_status"] != "Invalid" || g["pan_checked"] != "ABCCK1234L" {
		t.Fatalf("stored: %v", g)
	}

	// At joining: the PAN entered on the joining details is checked and carried to the record with the PAN.
	c.do("PUT", "/api/onboarding/settings", "EMP-1002", "HR Admin", map[string]string{"employeeIdAt": "joining"})
	si := selectCandidate(t, c, shortlistBody(st, "Pia", "Sood", "9876501277"))
	oi := approvedOffer(t, c, st, si, "Pia", "Sood", "9876501277")
	c.do("POST", "/api/offers/"+oi+"/release", "EMP-1004", "HRBP", nil)
	c.do("POST", "/api/offers/"+oi+"/accept", "EMP-1004", "HRBP", nil)
	if s, r := c.do("POST", "/api/offers/"+oi+"/joining", "EMP-1004", "HRBP", map[string]any{"data": map[string]any{"pan": "QRSPS4321T", "uanAvail": "Yes", "uan": "100200300400"}}); s != 200 {
		t.Fatal(r)
	}
	_, o := c.do("GET", "/api/offers/"+oi, "EMP-1004", "HRBP", nil)
	if pc := o["panCheck"].(map[string]any); pc["status"] != "Verified" || o["pan_check"] != nil {
		t.Fatalf("offer PAN check: %v", o["panCheck"])
	}
	want := "JI-0001" // the record is made at joining, on the Joining ID (hiring.go)
	if s, r := c.do("POST", "/api/candidates/"+si+"/joined", "EMP-1004", "HRBP", map[string]any{"date": today()}); s != 200 || r["id"] != want {
		t.Fatal(s, r)
	}
	if g := apiDoc(st, "employee_gov_id", bson.M{"employee_id": want}); g["pan_status"] != "Verified" || g["pan_checked"] != "QRSPS4321T" {
		t.Fatalf("carried to the record: %v", g)
	}
	if emp := apiDoc(st, "employee", bson.M{"_id": want}); emp["existing_uan"] != true || emp["job_role"] != "Front desk" || emp["skill"] != "Guest service" {
		t.Fatalf("existing UAN, role and skill on the record: %v", emp)
	}
}

func TestB1RoleSkillMasters(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	// Managed in Masters like the other job masters.
	_, l := c.do("GET", "/api/masters/job_role?status=Active", "EMP-1004", "HRBP", nil)
	if n := len(l["list"].([]any)); n < 3 {
		t.Fatalf("roles: %v", l)
	}
	if s, _ := c.do("POST", "/api/masters/job_role", "EMP-1004", "HRBP", map[string]any{"name": "Spa"}); s != 403 {
		t.Fatal("masters: job at Edit")
	}
	s, spa := c.do("POST", "/api/masters/job_role", "EMP-1002", "HR Admin", map[string]any{"name": "Spa"})
	if s != 200 || spa["kind"] != "job_role" {
		t.Fatal(s, spa)
	}
	if s, _ := c.do("POST", "/api/masters/skill", "EMP-1002", "HR Admin", map[string]any{"name": "Massage"}); s != 200 {
		t.Fatal("skill")
	}
	// Access roles are a different thing: the access role list does not change.
	if apiCount(st, "role", bson.M{"name": "Spa"}) != 0 {
		t.Fatal("a job role is not an access role")
	}

	// Required on the offer, like the department, and must be active.
	data := offerData(st, "Rina", "Roy", "9876501266")
	delete(data, "jobRole")
	delete(data, "jobSkill")
	si := selectCandidate(t, c, shortlistBody(st, "Rina", "Roy", "9876501266"))
	_, chk := c.do("POST", "/api/offers/check", "EMP-1004", "HRBP", map[string]any{"data": data, "candidate": si})
	errs := fmt.Sprint(chk["errors"])
	if !strings.Contains(errs, "Role is required") || !strings.Contains(errs, "Skill is required") {
		t.Fatalf("required: %v", errs)
	}
	data["jobRole"], data["jobSkill"] = "Astronaut", "Spa"
	_, chk = c.do("POST", "/api/offers/check", "EMP-1004", "HRBP", map[string]any{"data": data, "candidate": si})
	if errs := fmt.Sprint(chk["errors"]); !strings.Contains(errs, "Role “Astronaut” is not an active role") || !strings.Contains(errs, "Skill “Spa” is not an active skill") {
		t.Fatalf("active masters: %v", errs)
	}
	// The offer form offers the active names.
	_, setup := c.do("GET", "/api/onboarding/setup", "EMP-1002", "HR Admin", nil)
	found := false
	for _, x := range setup["fields"].([]any) {
		if f := x.(map[string]any); f["key"] == "jobRole" {
			found = strings.Contains(fmt.Sprint(f["options"]), "Spa") && f["offer"] == 2.0 && f["sys"] == true
		}
	}
	if !found {
		t.Fatalf("Role field: %v", setup["fields"])
	}
	// Retiring a role keeps employees who have it valid; a rename follows to the records.
	if _, err := st.DB.C("employee").Update(context.Background(), bson.M{"_id": "EMP-1020"}, bson.M{"$set": bson.M{"job_role": "Spa"}}); err != nil {
		t.Fatal(err)
	}
	if s, r := c.do("PATCH", "/api/masters/job_role/"+spa["id"].(string), "EMP-1002", "HR Admin", map[string]any{"name": "Spa and wellness"}); s != 200 {
		t.Fatal(r)
	}
	if e := apiDoc(st, "employee", bson.M{"_id": "EMP-1020"}); e["job_role"] != "Spa and wellness" {
		t.Fatalf("rename: %v", e["job_role"])
	}
	_, e := c.do("GET", "/api/employees/EMP-1020", "EMP-1002", "HR Admin", nil)
	if e["job_role"] != "Spa and wellness" {
		t.Fatalf("on the record: %v", e["job_role"])
	}
}

func TestB1MasterUpdateColumns(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004")
	_, l := c.do("GET", "/api/bulk", "EMP-1002", "HR Admin", nil)
	var cols string
	for _, x := range l["list"].([]any) {
		if m := x.(map[string]any); m["key"] == "upd" {
			cols = strings.Join(toStrs(m["cols"].([]any)), ",")
		}
	}
	if !strings.Contains(cols, "Attendance Mode,Role,Skill,Medical,Accommodation,Reason for change") {
		t.Fatalf("columns: %s", cols)
	}
	emp := apiDoc(st, "employee", bson.M{"_id": "EMP-1020"})
	if emp["type"] != "Permanent" {
		t.Skip("EMP-1020 is not permanent in this sample")
	}
	others := b1Perm(t, st, 2)
	head := "Employee ID,Work City,Reporting Location,Department,Designation,Grade,Shift,Reporting Manager L1,Functional Manager L2,Work Policy,Attendance Mode,Role,Skill,Medical,Accommodation,Reason for change,Effective From\n"
	pre := ",,,,,,,,,,"
	file := head +
		"EMP-1020" + pre + ",Kitchen,Cooking,450,1500,Data correction," + today() + "\n" +
		others[0] + pre + ",Astronaut,,,,Data correction," + today() + "\n" +
		others[1] + pre + ",Kitchen,,,,,\n"
	s, r := c.sendFile("/api/bulk/upd/check", "EMP-1002", "HR Admin", "upd.csv", []byte(file))
	if s != 200 {
		t.Fatal(s, r)
	}
	ch := b1Changes(r, 0)
	for _, want := range []string{"Role:  → Kitchen", "Skill:  → Cooking", "Medical insurance (₹ a month): Standard → ₹450", "Accommodation deduction: ₹0 → ₹1,500"} {
		if !strings.Contains(ch, want) {
			t.Fatalf("missing %q in %s (%s)", want, ch, b1Errs(r, 0))
		}
	}
	if e := b1Errs(r, 1); !strings.Contains(e, "Role “Astronaut” is not an active role") {
		t.Fatal(e)
	}
	if e := b1Errs(r, 2); !strings.Contains(e, "Reason for change and Effective From are needed") {
		t.Fatal(e)
	}
	if apiDoc(st, "employee", bson.M{"_id": "EMP-1020"})["job_role"] != nil {
		t.Fatal("check changes nothing")
	}
	if _, r = c.sendFile("/api/bulk/upd/apply", "EMP-1002", "HR Admin", "upd.csv", []byte(file)); r["ok"] != 1.0 {
		t.Fatal(r)
	}
	e := apiDoc(st, "employee", bson.M{"_id": "EMP-1020"})
	p := apiDoc(st, "employee_pay", bson.M{"employee_id": "EMP-1020"})
	if e["job_role"] != "Kitchen" || e["skill"] != "Cooking" || apiFloat(p["medical"]) != 450 || apiFloat(p["accommodation"]) != 1500 {
		t.Fatalf("applied: %v %v", e, p)
	}
	if apiCount(st, "employee_change", bson.M{"employee_id": "EMP-1020", "field": bson.M{"$in": bson.A{"Role", "Skill", "Medical insurance (₹ a month)", "Accommodation deduction"}}}) != 4 {
		t.Fatal("history")
	}
	// The record shows the medical amount with the pay options; the CTC uses it in place of the structure's rate.
	_, rec := c.do("GET", "/api/employees/EMP-1020", "EMP-1002", "HR Admin", nil)
	if pay := rec["salary"].(map[string]any)["pay"].(map[string]any); pay["medical"] != 450.0 || pay["accommodation"] != 1500.0 {
		t.Fatalf("pay on the record: %v", pay)
	}
	// Uploading the same again is no change; Standard goes back to the structure's rule; old files without the columns work.
	if _, r = c.sendFile("/api/bulk/upd/check", "EMP-1002", "HR Admin", "upd.csv", []byte(head+"EMP-1020"+pre+",kitchen,Cooking,450,1500,,\n")); b1Errs(r, 0) != "" || b1Changes(r, 0) != "" {
		t.Fatalf("same values: %v", r)
	}
	if _, r = c.sendFile("/api/bulk/upd/apply", "EMP-1002", "HR Admin", "upd.csv", []byte(head+"EMP-1020"+pre+",,,Standard,,Data correction,"+today()+"\n")); r["ok"] != 1.0 {
		t.Fatal(r)
	}
	if p := apiDoc(st, "employee_pay", bson.M{"employee_id": "EMP-1020"}); p["medical"] != nil {
		t.Fatalf("standard: %v", p["medical"])
	}
	old := "Employee ID,Work City,Reporting Location,Department,Designation,Grade,Shift,Reporting Manager L1,Functional Manager L2,Work Policy,Attendance Mode,Reason for change,Effective From\nEMP-1020,,,,,,,,,,,,\n"
	if s, r := c.sendFile("/api/bulk/upd/check", "EMP-1002", "HR Admin", "upd.csv", []byte(old)); s != 200 || b1Errs(r, 0) != "" {
		t.Fatalf("old file: %d %v", s, r)
	}
	// Medical and Accommodation need the salary right: the HRBP's role does not have it at Edit.
	if _, r = c.sendFile("/api/bulk/upd/check", "EMP-1004", "HRBP", "upd.csv", []byte(head+"EMP-1020"+pre+",,,500,,Data correction,"+today()+"\n")); r != nil && r["results"] != nil {
		if e := b1Errs(r, 0); !strings.Contains(e, "salar") && !strings.Contains(e, "outside your access") {
			t.Fatalf("salary right: %s", e)
		}
	}
}

func b1Zip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, data := range files {
		f, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write(data)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestB1CardsZip(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1020")
	big := append([]byte(b1PDF), bytes.Repeat([]byte{'x'}, 5<<20)...)
	z := b1Zip(t, map[string][]byte{
		"EMP-1020_ESIC.pdf":            []byte(b1PDF),
		"cards/emp-1020-medical.PNG":   append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...),
		"EMP-9999_ESIC.pdf":            []byte(b1PDF),
		"Karan.pdf":                    []byte(b1PDF),
		"../EMP-1021_ESIC.pdf":         []byte(b1PDF),
		"/etc/EMP-1021_MEDICAL.pdf":    []byte(b1PDF),
		"EMP-1021_ESIC.pdf":            []byte("just text, not a card"),
		"EMP-1022_ESIC.pdf":            big,
		"EMP-1023_MEDICAL.pdf":         []byte(b1PDF),
		"sub/EMP-1023-medical.pdf":     []byte(b1PDF),
		"__MACOSX/._EMP-1020_ESIC.pdf": []byte("junk"),
	})
	if s, _ := c.sendFile("/api/bulk/cards/check", "EMP-1020", "Employee", "cards.zip", z); s != 403 {
		t.Fatal("whoever uploads employee documents")
	}
	if s, r := c.sendFile("/api/bulk/cards/check", "EMP-1002", "HR Admin", "cards.zip", []byte("not a zip")); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "ZIP") {
		t.Fatal(s, r)
	}
	s, r := c.sendFile("/api/bulk/cards/check", "EMP-1002", "HR Admin", "cards.zip", z)
	if s != 200 {
		t.Fatal(s, r)
	}
	sum := r["summary"].(map[string]any)
	all := fmt.Sprint(r["results"])
	if sum["matched"] != 3.0 || sum["unmatched"] != 1.0 || sum["invalid"] != 6.0 || r["ok"] != 3.0 {
		t.Fatalf("summary %v: %s", sum, all)
	}
	for _, want := range []string{"Unmatched: No employee EMP-9999", "Name it by Employee ID and card", "Unsafe path in the ZIP (..)", "Unsafe path in the ZIP (absolute)",
		"Not a PDF, JPG or PNG file", "Larger than 5 MB", "A second Medical card for EMP-1023"} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing %q in %s", want, all)
		}
	}
	if strings.Contains(all, "__MACOSX") {
		t.Fatal("Mac junk is left out")
	}
	if apiCount(st, "document", bson.M{"doc_key": bson.M{"$in": bson.A{"esicCard", "medicalCard"}}}) != 0 {
		t.Fatal("check stores nothing")
	}
	if _, r = c.sendFile("/api/bulk/cards/apply", "EMP-1002", "HR Admin", "cards.zip", z); r["ok"] != 3.0 {
		t.Fatal(r)
	}
	if n := apiCount(st, "document", bson.M{"owner_kind": "employee", "owner_id": "EMP-1020", "doc_key": bson.M{"$in": bson.A{"esicCard", "medicalCard"}}, "replaced_at": nil}); n != 2 {
		t.Fatalf("stored: %d", n)
	}
	// A new card replaces the current one, which stays in the history.
	z2 := b1Zip(t, map[string][]byte{"EMP-1020_esic.pdf": []byte(b1PDF + "%v2\n")})
	_, r = c.sendFile("/api/bulk/cards/check", "EMP-1002", "HR Admin", "cards.zip", z2)
	if !strings.Contains(fmt.Sprint(r["results"]), "replaces the current card") {
		t.Fatal(r)
	}
	c.sendFile("/api/bulk/cards/apply", "EMP-1002", "HR Admin", "cards.zip", z2)
	if n := apiCount(st, "document", bson.M{"owner_id": "EMP-1020", "doc_key": "esicCard"}); n != 2 {
		t.Fatalf("history: %d", n)
	}
	if n := apiCount(st, "document", bson.M{"owner_id": "EMP-1020", "doc_key": "esicCard", "replaced_at": nil}); n != 1 {
		t.Fatalf("one current: %d", n)
	}
	// The record lists them by their label.
	_, d := c.do("GET", "/api/employees/EMP-1020/documents", "EMP-1002", "HR Admin", nil)
	if l := fmt.Sprint(d["list"]); !strings.Contains(l, "ESIC card") || !strings.Contains(l, "Medical card") {
		t.Fatalf("labels: %s", l)
	}
	if apiDoc(st, "audit_log", bson.M{"action": "Bulk update “ESIC and medical cards” applied: 1 file"}) == nil {
		t.Fatal("audited")
	}
}

func TestB1OptionalHolidayOpening(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1014")
	head := "Employee ID,Leave type,Balance,As at,Optional Holiday Balance\n" // Opening leave balances (optional holidays merged in, 17 Nov 2026)
	others := b1Perm(t, st, 2)
	file := head + "EMP-1020,,," + today() + ",1.5\n" + others[0] + ",,," + today() + ",40\n" + others[1] + ",,,2099-01-01,1\n"
	if s, _ := c.sendFile("/api/bulk/lvopen/check", "EMP-1014", "Employee", "o.csv", []byte(file)); s != 403 {
		t.Fatal("policies at Edit")
	}
	s, r := c.sendFile("/api/bulk/lvopen/check", "EMP-1002", "HR Admin", "o.csv", []byte(file))
	if s != 200 || r["ok"] != 1.0 || !strings.Contains(b1Errs(r, 1), "Optional Holiday Balance must be a number from 0 to 31") || !strings.Contains(b1Errs(r, 2), "As at must be a date up to today") {
		t.Fatal(s, r)
	}
	c.sendFile("/api/bulk/lvopen/apply", "EMP-1002", "HR Admin", "o.csv", []byte(file))
	if o := apiDoc(st, store.OptHolOpeningColl, bson.M{"employee_id": "EMP-1020"}); apiFloat(o["balance"]) != 1.5 {
		t.Fatalf("stored: %v", o)
	}
	// Loading the same employee and date again replaces it.
	c.sendFile("/api/bulk/lvopen/apply", "EMP-1002", "HR Admin", "o.csv", []byte(head+"EMP-1020,,,"+today()+",1\n"))
	if n := apiCount(st, store.OptHolOpeningColl, bson.M{"employee_id": "EMP-1020"}); n != 1 {
		t.Fatalf("replaced: %d", n)
	}
	_, b := c.do("GET", "/api/leave/balances/EMP-1020", "EMP-1002", "HR Admin", nil)
	if oh, _ := b["optHol"].(map[string]any); oh == nil || oh["opening"].(map[string]any)["left"] != 1.0 {
		t.Fatalf("leave balances: %v", b["optHol"])
	}
	_, e := c.do("GET", "/api/employees/EMP-1020", "EMP-1002", "HR Admin", nil)
	if o := e["optHolOpening"].(map[string]any); o["left"] != 1.0 || o["at"] != today() {
		t.Fatalf("record: %v", o)
	}
}

func TestB1ImportWithoutGross(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002")
	_, l := c.do("GET", "/api/bulk", "EMP-1002", "HR Admin", nil)
	for _, x := range l["list"].([]any) {
		if m := x.(map[string]any); m["key"] == "new" && strings.Contains(strings.Join(toStrs(m["cols"].([]any)), ","), "Monthly Gross") {
			t.Fatal("Monthly Gross is not a column")
		}
	}
	head := "Employee Code,Full Name,Employment Type,Date of Joining,Work City,Reporting Location,Department,Designation,Grade,Shift,Reporting Manager L1,Functional Manager L2,Work Policy,Attendance Mode,Role,Skill,Mobile,Email,Date of Birth,Gender,PAN,Aadhaar,UAN,Existing UAN?,ESIC No,Bank Name,Account Number,IFSC,Status,Last Working Day,Opening CL Balance,Opening PL Balance,Opening SL Balance"
	base := ",Permanent,2023-06-12,Gurugram,Gurugram – Sector 29 Hub,Front Office,Front Office Supervisor,G3,G,EMP-1009,,,,"
	file := head + "\n" +
		"NG-1,No Gross" + base + "Kitchen,Cooking,,,,,,,100200300401,Yes,,,,,,,,,\n" +
		"NG-2,Bad Role" + base + "Astronaut,,,,,,,,,Yes,,,,,,,,,\n"
	s, r := c.sendFile("/api/bulk/new/check", "EMP-1002", "HR Admin", "new.csv", []byte(file))
	if s != 200 || r["ok"] != 1.0 {
		t.Fatal(s, r)
	}
	if e := b1Errs(r, 1); !strings.Contains(e, "Role “Astronaut” is not an active role") || !strings.Contains(e, "UAN is required when Existing UAN? is Yes") {
		t.Fatal(e)
	}
	c.sendFile("/api/bulk/new/apply", "EMP-1002", "HR Admin", "new.csv", []byte(file))
	e := apiDoc(st, "employee", bson.M{"_id": "NG-1"})
	if e == nil || e["job_role"] != "Kitchen" || e["skill"] != "Cooking" || e["existing_uan"] != true {
		t.Fatalf("imported: %v", e)
	}
	if apiCount(st, "gross_history", bson.M{"employee_id": "NG-1"}) != 0 {
		t.Fatal("no salary until Salary breakup sets it")
	}
	// The record says there is no gross; the dashboard counts the person under missing details.
	_, rec := c.do("GET", "/api/employees/NG-1", "EMP-1002", "HR Admin", nil)
	if rec["salary"].(map[string]any)["gross"] != nil {
		t.Fatalf("gross: %v", rec["salary"])
	}
	_, d := c.do("GET", "/api/dashboard", "EMP-1002", "HR Admin", nil)
	if !strings.Contains(fmt.Sprint(d["alerts"]), "bank or salary pending") {
		t.Fatalf("dashboard: %v", d["alerts"])
	}
	// Payroll does not fail on them: the dry run has the person with no pay, flagged "Zero or negative net".
	signAll(c, "EMP-1011")
	open := pfaOpen(t, c)
	c.pcCloseAll(t, open)
	pfaMust(t, c, open, "dry-run", "EMP-1011", "Payroll")
	if row := c.row(t, open, "NG-1"); num(row["net"]) != 0 || !strings.Contains(fmt.Sprint(row["flags"]), "zero") {
		t.Fatalf("dry run row: net %v flags %v", row["net"], row["flags"])
	}
	// An old file with Monthly Gross: the column is ignored, with a note.
	old := strings.Replace(head, "Attendance Mode,Role,Skill", "Attendance Mode,Monthly Gross,Role,Skill", 1) + "\nNG-3,Old File" + base + "24500,Kitchen,Cooking,,,,,,,,,,,,,,,,,\n"
	s, r = c.sendFile("/api/bulk/new/check", "EMP-1002", "HR Admin", "new.csv", []byte(old))
	if s != 200 || r["ok"] != 1.0 || !strings.Contains(b1Changes(r, 0), "Monthly Gross ignored") {
		t.Fatalf("old file: %d %v", s, r)
	}
	// Salary breakup then sets the salary.
	sal := "Employee ID,Monthly Gross,Variable Pay %,Variable Payout Cycle,Meal Deduction,Accommodation Deduction,Salary Structure,Structure Effective From,Reason for change,Effective From\n" +
		"NG-1,24500,,,,,,,Annual increment," + today() + "\n"
	if _, r = c.sendFile("/api/bulk/sal/apply", "EMP-1002", "HR Admin", "sal.csv", []byte(sal)); r["ok"] != 1.0 {
		t.Fatalf("salary breakup: %v", r)
	}
}

func TestB1ChildrenAndExistingUAN(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002")
	_, l := c.do("GET", "/api/bulk", "EMP-1002", "HR Admin", nil)
	for _, x := range l["list"].([]any) {
		if m := x.(map[string]any); m["key"] == "pers" {
			if cols := strings.Join(toStrs(m["cols"].([]any)), ","); !strings.Contains(cols, "Father or Spouse Name,Spouse Name,1st Child,2nd Child,Marital Status") || !strings.HasSuffix(cols, "Existing UAN?") {
				t.Fatalf("columns: %s", cols)
			}
		}
	}
	if _, err := st.DB.C("employee_gov_id").Upsert(context.Background(), bson.M{"employee_id": "EMP-1020"}, bson.M{"uan": "100200300499"}, nil); err != nil {
		t.Fatal(err)
	}
	others := b1Perm(t, st, 2)
	if _, err := st.DB.C("employee_gov_id").Upsert(context.Background(), bson.M{"employee_id": others[0]}, bson.M{"uan": nil}, nil); err != nil {
		t.Fatal(err)
	}
	head := "Employee ID,Mobile,Personal Email,Date of Birth,Gender,Address,Father or Spouse Name,1st Child,2nd Child,Marital Status,Emergency Contact Name,Emergency Contact Relation,Emergency Contact Mobile,Existing UAN?\n"
	pre := ",,,,,,"
	post := ",,,,"
	file := head + "EMP-1020" + pre + ",Aarav Khan,Anya Khan" + post + ",Yes\n" + others[0] + pre + ",," + post + ",Yes\n" + others[1] + pre + ",," + post + ",Maybe\n"
	s, r := c.sendFile("/api/bulk/pers/check", "EMP-1002", "HR Admin", "p.csv", []byte(file))
	if s != 200 || r["ok"] != 1.0 {
		t.Fatal(s, r)
	}
	if !strings.Contains(b1Errs(r, 1), "Existing UAN? Yes needs the UAN") || !strings.Contains(b1Errs(r, 2), "Existing UAN? is Yes or No") {
		t.Fatal(r)
	}
	c.sendFile("/api/bulk/pers/apply", "EMP-1002", "HR Admin", "p.csv", []byte(file))
	p := apiDoc(st, "employee_personal", bson.M{"employee_id": "EMP-1020"})
	if p["child1"] != "Aarav Khan" || p["child2"] != "Anya Khan" || apiDoc(st, "employee", bson.M{"_id": "EMP-1020"})["existing_uan"] != true {
		t.Fatalf("applied: %v", p)
	}
	// Blank keeps the current value.
	c.sendFile("/api/bulk/pers/apply", "EMP-1002", "HR Admin", "p.csv", []byte(head+"EMP-1020"+pre+",,Anya K"+post+",\n"))
	if p = apiDoc(st, "employee_personal", bson.M{"employee_id": "EMP-1020"}); p["child1"] != "Aarav Khan" || p["child2"] != "Anya K" {
		t.Fatalf("blank keeps: %v", p)
	}
	_, e := c.do("GET", "/api/employees/EMP-1020", "EMP-1002", "HR Admin", nil)
	if e["personal"].(map[string]any)["child1"] != "Aarav Khan" || e["existingUan"] != "Yes" {
		t.Fatalf("record: %v %v", e["personal"], e["existingUan"])
	}
	// The download: current data has the columns; the employee download has the children and Existing UAN?.
	if s, b := c.getRaw("/api/employees/export?sections=personal,stat", "EMP-1002", "HR Admin"); s != 200 || !bytes.Contains(b, []byte("1st child name")) ||
		!bytes.Contains(b, []byte("Aarav Khan")) || !bytes.Contains(b, []byte("Existing UAN?")) {
		t.Fatalf("export %d %.300s", s, b)
	}
	// The joining form has the optional family names next to Marital status.
	_, setup := c.do("GET", "/api/onboarding/setup", "EMP-1002", "HR Admin", nil)
	order := []string{}
	for _, x := range setup["fields"].([]any) {
		if f := x.(map[string]any); f["section"] == "Personal" {
			order = append(order, f["key"].(string))
		}
	}
	if o := strings.Join(order, " "); !strings.Contains(o, "fatherSpouse marital child1 child2") {
		t.Fatalf("order: %s", o)
	}
}

// b1Perm is n active permanent employees other than EMP-1020, in code order.
func b1Perm(t *testing.T, st *store.Store, n int) []string {
	t.Helper()
	out := []string{}
	rows, _ := st.DB.C("employee").Find(context.Background(), bson.M{"status": "Active", "type": "Permanent", "_id": bson.M{"$gt": "EMP-1020"}})
	for _, r := range rows {
		if len(out) < n {
			out = append(out, r["id"].(string))
		}
	}
	if len(out) < n {
		t.Fatal("not enough permanent staff in the sample")
	}
	return out
}
