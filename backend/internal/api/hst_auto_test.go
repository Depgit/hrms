package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Company accommodation tagged on the offer (user, 19 Nov 2026): Mark joined books a bed in the tagged cluster from
// the joining day; with no free bed the joiner waits (list + bell) and gets the bed the sweep finds once one frees up.
func TestJoinAccommodation(t *testing.T) {
	c, app, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	ctx := context.Background()
	hub, _ := hubPlace(st)
	cluster := fmt.Sprint(apiDoc(st, "location", bson.M{"_id": hub})["cluster_id"])
	unit := hstUnit(t, c, "Joiner Flat", cluster, "Any", 1, hub)

	join := func(first, last, phone string, data map[string]any) (string, int, map[string]any) {
		si := selectCandidate(t, c, shortlistBody(st, first, last, phone))
		if s, r := c.upload("EMP-1004", "HRBP", "candidate", si, "aadhaar"); s != 200 {
			t.Fatalf("aadhaar: %v", r)
		}
		d := offerData(st, first, last, phone)
		for k, v := range data {
			d[k] = v
		}
		s, r := c.do("POST", "/api/offers", "EMP-1004", "HRBP", map[string]any{"data": d, "candidate": si})
		if s != 200 {
			return "", s, r
		}
		oi := r["id"].(string)
		ui3ApproveAll(t, c, r["request"].(map[string]any)["id"].(string))
		for _, step := range []string{"release", "accept"} {
			if s, r := c.do("POST", "/api/offers/"+oi+"/"+step, "EMP-1004", "HRBP", nil); s != 200 {
				t.Fatalf("%s: %v", step, r)
			}
		}
		if s, r := c.do("POST", "/api/candidates/"+si+"/joined", "EMP-1004", "HRBP", map[string]any{"date": today()}); s != 200 {
			t.Fatalf("joined: %v", r)
		}
		return fmt.Sprint(apiDoc(st, "candidate", bson.M{"_id": si})["employee_id"]), 200, nil
	}

	// Yes needs a cluster with accommodation.
	if _, s, r := join("Asha", "Nair", "9876504001", map[string]any{"accom": "Yes"}); s != 400 || !strings.Contains(fmt.Sprint(r), "cluster") {
		t.Fatalf("Yes without a cluster: %d %v", s, r)
	}

	// Tagged: a bed in that cluster from the joining date, by itself.
	a, _, _ := join("Bala", "Iyer", "9876504002", map[string]any{"accom": "Yes", "accomCluster": cluster})
	stay := apiDoc(st, "hst_stay", bson.M{"employee_id": a})
	if stay == nil || fmt.Sprint(stay["unit_id"]) != unit || stay["check_in"] != today() || stay["note"] != "Allocated automatically from the offer" {
		t.Fatalf("allocated: %v", stay)
	}

	// No free bed: on the waiting list, HR told.
	b, _, _ := join("Charu", "Das", "9876504003", map[string]any{"accom": "Yes", "accomCluster": cluster})
	if apiCount(st, "hst_stay", bson.M{"employee_id": b}) != 0 || apiDoc(st, "employee", bson.M{"_id": b})["hst_wait"] == nil {
		t.Fatalf("waiting: %v", apiDoc(st, "employee", bson.M{"_id": b})["hst_wait"])
	}
	if r := hstOK(t, c, "GET", "/api/hst/waiting", nil); len(r["rows"].([]any)) != 1 {
		t.Fatalf("waiting list: %v", r)
	}
	if n := notices(c, "EMP-1002", "HR Admin"); !strings.Contains(n, "no bed is free") {
		t.Fatalf("bell: %s", n)
	}
	// The bed frees up tomorrow: the sweep books it from then.
	tomorrow := time.Now().AddDate(0, 0, 1)
	hstOK(t, c, "POST", "/api/hst/stays/"+fmt.Sprint(stay["id"])+"/checkout", map[string]any{"date": tomorrow.Format("2006-01-02"), "reason": "Moved out"})
	if n, err := app.HstWaitSweep(ctx, tomorrow); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	if s := apiDoc(st, "hst_stay", bson.M{"employee_id": b}); s == nil || s["check_in"] != tomorrow.Format("2006-01-02") || apiDoc(st, "employee", bson.M{"_id": b})["hst_wait"] != nil {
		t.Fatalf("after the sweep: %v", s)
	}
}
