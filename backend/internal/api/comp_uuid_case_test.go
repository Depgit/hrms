package api_test

import (
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// A city or state id in another spelling (upper case, braces, no hyphens) still names the same row in a pay-band
// save, a band copy and an LWF save: PostgreSQL cast it with ::uuid, so INSERT … ON CONFLICT updated the row
// instead of failing on the unique key.
func TestCompUpsertUUIDSpelling(t *testing.T) {
	c, _, st := setupFull(t)
	c.signIn("EMP-1002")
	_, comp := c.do("GET", "/api/comp", "EMP-1002", "HR Admin", nil)
	band := comp["bands"].([]any)[0].(map[string]any)
	city, grade := band["city_id"].(string), band["grade"].(string)
	var other string
	for _, b := range comp["bands"].([]any) {
		if m := b.(map[string]any); m["city_id"] != city {
			other = m["city_id"].(string)
		}
	}
	if other == "" {
		t.Fatal("need bands in two cities")
	}
	brace := "{" + strings.ToUpper(strings.ReplaceAll(city, "-", "")) + "}"

	for i, id := range []string{city, strings.ToUpper(city), brace} {
		lo, hi := 15000+1000*i, 30000+1000*i
		if s, r := c.do("PUT", "/api/comp/bands", "EMP-1002", "HR Admin", map[string]any{"cityId": id, "grade": grade, "min": lo, "max": hi}); s != 200 {
			t.Fatalf("band save with %s: %d %v", id, s, r)
		}
		if n := apiCount(st, "pay_band", bson.M{"city_id": city, "grade": grade}); n != 1 {
			t.Fatalf("one band row, got %d", n)
		}
		got := apiDoc(st, "pay_band", bson.M{"city_id": city, "grade": grade})
		if apiFloat(got["min_gross"]) != float64(lo) || apiFloat(got["max_gross"]) != float64(hi) {
			t.Fatalf("band updated through %s: %v", id, got)
		}
	}
	// The audit's before matched city_id::text exactly, as it did: none for the other spellings.
	if a := apiDoc(st, "audit_log", bson.M{"action": "Pay band changed", "entity_id": strings.ToUpper(city) + "/" + grade}); a == nil || a["before"] != nil {
		t.Fatalf("upper-case save audited with no before: %v", a)
	}
	if a := apiDoc(st, "audit_log", bson.M{"action": "Pay band changed", "entity_id": city + "/" + grade}); a == nil || a["before"] == nil {
		t.Fatalf("lower-case save audited with its before: %v", a)
	}

	// Copying onto the other city twice, the second time naming it in upper case, updates the same rows.
	before := apiCount(st, "pay_band", bson.M{"city_id": other})
	for _, to := range []string{other, strings.ToUpper(other)} {
		if s, r := c.do("POST", "/api/comp/bands/copy", "EMP-1002", "HR Admin", map[string]any{"from": city, "to": to, "factor": 1}); s != 200 || apiFloat(r["copied"]) == 0 {
			t.Fatalf("copy to %s: %d %v", to, s, r)
		}
	}
	after := apiCount(st, "pay_band", bson.M{"city_id": other})
	if after < before || after > before+apiCount(st, "pay_band", bson.M{"city_id": city}) {
		t.Fatalf("copy rows %d → %d", before, after)
	}
	if n := apiCount(st, "pay_band", bson.M{"city_id": strings.ToUpper(other)}); n != 0 {
		t.Fatalf("no band stored under the upper-case spelling, got %d", n)
	}

	state := comp["lwf"].([]any)[0].(map[string]any)["state_id"].(string)
	for i, id := range []string{state, strings.ToUpper(state)} {
		rule := map[string]any{"effectiveFrom": "2026-04-01", "rule": map[string]any{"on": true, "ee": map[string]any{"mode": "fixed", "val": 10 + i}, "er": map[string]any{"mode": "fixed", "val": 20}, "freq": "monthly"}}
		if s, r := c.do("PUT", "/api/comp/lwf/"+id, "EMP-1002", "HR Admin", rule); s != 200 {
			t.Fatalf("lwf save with %s: %d %v", id, s, r)
		}
		if n := apiCount(st, "lwf_rule", bson.M{"state_id": state, "effective_from": "2026-04-01"}); n != 1 {
			t.Fatalf("one lwf row, got %d", n)
		}
		got := apiDoc(st, "lwf_rule", bson.M{"state_id": state, "effective_from": "2026-04-01"})
		if !strings.Contains(fmt.Sprint(got["rule"]), fmt.Sprintf("val:%d", 10+i)) {
			t.Fatalf("lwf rule updated through %s: %v", id, got["rule"])
		}
	}
}
