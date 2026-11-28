package api_test

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestTeamTwoLevels: a manager's Line Manager view covers their direct reports and their reports' reports; the
// "Direct employees" filter (?team=direct / X-Team) narrows it to the first level (owner, 27 Nov 2026).
func TestTeamTwoLevels(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	// A chain G → M → X among active employees.
	emps, _ := st.DB.C("employee").Find(ctx, bson.M{"status": "Active", "manager_id": bson.M{"$ne": nil}})
	mgr := map[string]string{}
	for _, e := range emps {
		mgr[e["id"].(string)], _ = e["manager_id"].(string)
	}
	var g, m, x string
	for id, mid := range mgr {
		if gid := mgr[mid]; mid != "" && gid != "" && gid != id && gid != mid {
			g, m, x = gid, mid, id
			break
		}
	}
	if g == "" {
		t.Skip("no two-level reporting chain in the seed")
	}
	signAll(c, g)
	ids := func(path string) map[string]bool {
		s, r := c.do("GET", path, g, "Line Manager", nil)
		if s != 200 {
			t.Fatalf("%s: %d %v", path, s, r)
		}
		out := map[string]bool{}
		for _, e := range r["rows"].([]any) {
			out[e.(map[string]any)["id"].(string)] = true
		}
		return out
	}
	all := ids("/api/employees?size=500")
	if !all[m] || !all[x] {
		t.Fatalf("%s sees direct %s and second level %s: %v", g, m, x, all)
	}
	direct := ids("/api/employees?size=500&team=direct")
	if !direct[m] || direct[x] {
		t.Fatalf("direct filter: %s yes, %s no: %v", m, x, direct)
	}
	for id := range all {
		if mgr[id] != g && mgr[mgr[id]] != g {
			t.Fatalf("%s is not under %s", id, g)
		}
	}
}
