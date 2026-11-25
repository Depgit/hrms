package api_test

// PF on the offer (pf_offer.go, owner 26 Nov 2026): "Does the employee want PF?" on the offer. No above the
// threshold is all Basic with no PF on the offer and on the employee after joining; No up to the threshold is refused
// (a Retainer); Yes is the normal structure; the joining form's Yes wins over the offer's No.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestPfOnOffer(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009", "EMP-1012")
	num := func(m map[string]any, k string) float64 { v, _ := m[k].(float64); return v }
	pay := func(emp string) map[string]any {
		p, _ := st.DB.C("employee_pay").FindOne(ctx, bson.M{"employee_id": emp})
		return p
	}

	si := selectCandidate(t, c, shortlistBody(st, "Asha", "Rao", "9876504001"))
	if s, r := c.upload("EMP-1004", "HRBP", "candidate", si, "aadhaar"); s != 200 {
		t.Fatalf("aadhaar: %v", r)
	}
	od := offerData(st, "Asha", "Rao", "9876504001")
	od["uanAvail"], od["gross"] = "No", 30000
	check := func(d map[string]any) map[string]any {
		t.Helper()
		s, r := c.do("POST", "/api/offers/check", "EMP-1004", "HRBP", map[string]any{"data": d, "candidate": si})
		if s != 200 {
			t.Fatalf("check: %d %v", s, r)
		}
		return r
	}

	// Required, no default, for a type PF applies to; not asked for a Retainer.
	delete(od, "pfWant")
	if r := check(od); !strings.Contains(fmt.Sprint(r["errors"]), "Does the employee want PF? Choose Yes or No") {
		t.Fatalf("required: %v", r["errors"])
	}
	ret := map[string]any{}
	for k, v := range od {
		ret[k] = v
	}
	ret["type"], ret["pfWant"], ret["gross"] = "Retainer", "No", 20000
	if r := check(ret); strings.Contains(fmt.Sprint(r["errors"]), "PF") {
		t.Fatalf("a Retainer is not asked: %v", r["errors"])
	}

	// Yes: the normal structure with PF.
	od["pfWant"] = "Yes"
	if b := check(od)["breakup"].(map[string]any); num(b, "hra") <= 0 || num(b, "eePF") <= 0 || num(b, "basic") >= 30000 || b["pfExempt"] == true {
		t.Fatalf("Yes: normal structure: %v", b)
	}

	// No, at or below the threshold (₹25,000): refused, the type must be Retainer.
	od["pfWant"], od["gross"] = "No", 25000
	want := "Gross is up to ₹25,000: an employee who does not want PF must be a Retainer. Change the employment type to Retainer."
	if r := check(od); !strings.Contains(fmt.Sprint(r["errors"]), want) {
		t.Fatalf("refused at the threshold: %v", r["errors"])
	}
	if s, r := c.do("POST", "/api/offers", "EMP-1004", "HRBP", map[string]any{"data": od, "candidate": si}); s != 400 || !strings.Contains(fmt.Sprint(r), want) {
		t.Fatalf("raise refused: %d %v", s, r)
	}

	// No above the threshold: all Basic, no PF from either side, on the preview and the letter.
	od["gross"] = 30000
	if b := check(od)["breakup"].(map[string]any); num(b, "basic") != 30000 || num(b, "hra") != 0 || num(b, "sa") != 0 || num(b, "eePF") != 0 || num(b, "erPF") != 0 {
		t.Fatalf("No: all Basic, no PF: %v", b)
	}
	s, r := c.do("POST", "/api/offers", "EMP-1004", "HRBP", map[string]any{"data": od, "candidate": si})
	if s != 200 {
		t.Fatalf("offer: %v", r)
	}
	oi := r["id"].(string)
	ui3ApproveAll(t, c, r["request"].(map[string]any)["id"].(string))
	if _, o := c.do("GET", "/api/offers/"+oi, "EMP-1004", "HRBP", nil); num(o["breakup"].(map[string]any), "eePF") != 0 || o["data"].(map[string]any)["pfChoice"] != "No" {
		t.Fatalf("offer view: %v %v", o["breakup"], o["data"])
	}
	// The threshold raised meanwhile: the release is refused with the same message.
	if s, r := c.do("PUT", "/api/payroll/pf-settings", "EMP-1012", "Payroll Manager", map[string]any{"exemptGross": 30000}); s != 200 {
		t.Fatalf("threshold: %v", r)
	}
	if s, r := c.do("POST", "/api/offers/"+oi+"/release", "EMP-1004", "HRBP", nil); s != 400 || !strings.Contains(fmt.Sprint(r), "Gross is up to ₹30,000") {
		t.Fatalf("release refused: %d %v", s, r)
	}
	c.do("PUT", "/api/payroll/pf-settings", "EMP-1012", "Payroll Manager", map[string]any{"exemptGross": 25000})
	if s, r := c.do("POST", "/api/offers/"+oi+"/release", "EMP-1004", "HRBP", nil); s != 200 {
		t.Fatalf("release: %v", r)
	}
	if s, r := c.do("POST", "/api/offers/"+oi+"/accept", "EMP-1004", "HRBP", nil); s != 200 {
		t.Fatalf("accept: %v", r)
	}
	s, r = c.do("POST", "/api/candidates/"+si+"/joined", "EMP-1004", "HRBP", map[string]any{"date": today()})
	if s != 200 {
		t.Fatalf("joined: %v", r)
	}
	emp := fmt.Sprint(r["id"])
	if p := pay(emp); p["pf_choice"] != "optout" || p["all_basic"] != true || p["pf_exempt"] != true || p["pf_choice_by"] != "Offer" {
		t.Fatalf("joined on the offer's No: %v", p)
	}
	if acts := hireActions(hireCand(t, c, si)); !strings.Contains(acts, "PF choice on the offer") {
		t.Fatalf("timeline: %s", acts)
	}

	// The joining form's Yes wins over the offer's No: PF applies, normal structure.
	si2 := selectCandidate(t, c, shortlistBody(st, "Kiran", "Das", "9876504002"))
	if s, r := c.upload("EMP-1004", "HRBP", "candidate", si2, "aadhaar"); s != 200 {
		t.Fatalf("aadhaar: %v", r)
	}
	od2 := offerData(st, "Kiran", "Das", "9876504002")
	od2["uanAvail"], od2["gross"], od2["pfWant"] = "No", 30000, "No"
	s, r = c.do("POST", "/api/offers", "EMP-1004", "HRBP", map[string]any{"data": od2, "candidate": si2})
	if s != 200 {
		t.Fatalf("offer 2: %v", r)
	}
	oi2 := r["id"].(string)
	ui3ApproveAll(t, c, r["request"].(map[string]any)["id"].(string))
	c.do("POST", "/api/offers/"+oi2+"/release", "EMP-1004", "HRBP", nil)
	c.do("POST", "/api/offers/"+oi2+"/accept", "EMP-1004", "HRBP", nil)
	if s, r := c.do("POST", "/api/offers/"+oi2+"/joining", "EMP-1004", "HRBP", map[string]any{"data": map[string]any{"pfChoice": "Yes"}}); s != 200 {
		t.Fatalf("joining details: %v", r)
	}
	if _, o := c.do("GET", "/api/offers/"+oi2, "EMP-1004", "HRBP", nil); num(o["breakup"].(map[string]any), "eePF") <= 0 || num(o["breakup"].(map[string]any), "hra") <= 0 {
		t.Fatalf("the joining form's Yes: normal structure: %v", o["breakup"])
	}
	s, r = c.do("POST", "/api/candidates/"+si2+"/joined", "EMP-1004", "HRBP", map[string]any{"date": today()})
	if s != 200 {
		t.Fatalf("joined 2: %v", r)
	}
	emp2 := fmt.Sprint(r["id"])
	if p := pay(emp2); p["pf_choice"] != "member" || p["all_basic"] == true || p["pf_exempt"] == true || p["pf_choice_by"] != "Joining form" {
		t.Fatalf("the joining form's Yes wins: %v", p)
	}
	if acts := hireActions(hireCand(t, c, si2)); !strings.Contains(acts, "PF choice changed on the joining details") {
		t.Fatalf("timeline 2: %s", acts)
	}
	if _, x := c.do("GET", "/api/employees/"+emp2+"/pf-exempt", "EMP-1002", "HR Admin", nil); x["exempt"] != false {
		t.Fatalf("PF member: %v", x)
	}
}
