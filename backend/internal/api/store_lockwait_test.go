package api_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// holdLock locks one document in a transaction of its own and keeps it for d before committing, as a concurrent
// request holding SELECT … FOR UPDATE would. It returns once the lock is held; the channel gives the commit result.
func holdLock(t *testing.T, st *store.Store, coll string, filter bson.M, d time.Duration) <-chan error {
	t.Helper()
	locked, done := make(chan struct{}), make(chan error, 1)
	go func() {
		first := true
		done <- st.Tx(context.Background(), func(tx store.Q) error {
			row, err := tx.C(coll).Lock(context.Background(), filter)
			if err != nil {
				return err
			}
			if row == nil {
				return fmt.Errorf("nothing to lock in %s", coll)
			}
			if first {
				first = false
				close(locked)
			}
			time.Sleep(d)
			return nil
		})
	}()
	select {
	case <-locked:
	case err := <-done:
		t.Fatalf("holding the lock: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the lock was never taken")
	}
	return done
}

// A request that finds its row locked by another transaction waits for it (Tx runs again on the write conflict)
// instead of answering 404 / 400, as FOR UPDATE waited in PostgreSQL.
func TestStoreLockWaits(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1011", "EMP-1020")
	const hold = 400 * time.Millisecond
	committed := func(t *testing.T, done <-chan error) {
		t.Helper()
		if err := <-done; err != nil {
			t.Fatalf("the holding transaction failed: %v", err)
		}
	}

	t.Run("asset issue, return and lost", func(t *testing.T) {
		_, inv := c.do("GET", "/api/inventory", "EMP-1002", "HR Admin", nil)
		var laptop float64
		for _, i := range inv["items"].([]any) {
			if m := i.(map[string]any); m["name"] == "Laptop" {
				laptop = apiFloat(m["id"])
			}
		}
		if s, r := c.do("POST", "/api/inventory/stock/add", "EMP-1002", "HR Admin", map[string]any{"to": "central",
			"lines": []any{map[string]any{"itemId": laptop, "serials": []string{"LW-1", "LW-2"}}}}); s != 200 {
			t.Fatal(r)
		}
		a1, a2 := ui2InvAssetID(st, "LW-1"), ui2InvAssetID(st, "LW-2")
		issue := func(aid int64) (int, map[string]any) {
			return c.do("POST", "/api/inventory/issue", "EMP-1002", "HR Admin", map[string]any{"store": "central", "employeeId": "EMP-1020", "assetId": aid})
		}

		done := holdLock(t, st, "asset", bson.M{"_id": a1}, hold)
		if s, r := issue(a1); s != 200 {
			t.Fatalf("issue behind a lock: %d %v", s, r)
		}
		committed(t, done)

		done = holdLock(t, st, "asset", bson.M{"_id": a1}, hold)
		if s, r := c.do("POST", "/api/inventory/return", "EMP-1002", "HR Admin", map[string]any{"store": "central", "assetId": a1}); s != 200 {
			t.Fatalf("return behind a lock: %d %v", s, r)
		}
		committed(t, done)

		if s, r := issue(a2); s != 200 {
			t.Fatal(r)
		}
		done = holdLock(t, st, "asset", bson.M{"_id": a2}, hold)
		if s, r := c.do("POST", fmt.Sprintf("/api/inventory/assets/%d/lost", a2), "EMP-1002", "HR Admin", map[string]any{"note": "Left on the bus"}); s != 200 {
			t.Fatalf("lost behind a lock: %d %v", s, r)
		}
		committed(t, done)
		if a := apiDoc(st, "asset", bson.M{"_id": a2}); a["status"] != "Lost" {
			t.Fatal(a)
		}
	})

	t.Run("deduction waive", func(t *testing.T) {
		_, meta := c.do("GET", "/api/pay-items/deductions", "EMP-1011", "Payroll", nil)
		types := meta["dedTypes"].([]any)
		s, f := c.upload("EMP-1011", "Payroll", "approval", "EMP-1020/ded-1", "Deduction proof")
		if s != 200 {
			t.Fatal(f)
		}
		s, r := c.do("POST", "/api/approvals", "EMP-1011", "Payroll", map[string]any{"wf": "p_ded", "employeeId": "EMP-1020", "payload": map[string]any{
			"dedType": types[0], "amount": 3000, "instalments": 3, "heard": "Yes", "note": "Cash short by 3,000 on 14 Oct, counted by two staff", "doc": f["id"]}})
		if s != 200 {
			t.Fatal(r)
		}
		ded := r["id"].(string)
		c.approveAll(t, ded)
		done := holdLock(t, st, "approval_request", bson.M{"_id": ded}, hold)
		s, w := c.do("POST", "/api/pay-items/deductions/"+ded+"/waive", "EMP-1011", "Payroll", map[string]string{"note": "Cash found in the safe"})
		if s != 200 || w["waived"] != 3000.0 {
			t.Fatalf("waive behind a lock: %d %v", s, w)
		}
		committed(t, done)
		if s, w := c.do("POST", "/api/pay-items/deductions/"+ded+"/waive", "EMP-1011", "Payroll", map[string]string{"note": "Again"}); s != 400 || w["error"] != "Nothing is left to deduct" {
			t.Fatalf("second waive: %d %v", s, w)
		}
	})
}

// The uniforms an employee holds sort by issue date, then by the 'DD Mon YYYY' text of when they were recorded:
// the old ORDER BY's `at` named the to_char output column, not the timestamp.
func TestStoreUniformsOrder(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1020")
	ctx := context.Background()
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, store.Loc())
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, u := range []struct {
		id, on string
		at     time.Time
	}{
		{"UNI-T3", "2026-09-01", at("2026-10-09 10:00")}, // '09 Oct 2026'
		{"UNI-T4", "2026-09-01", at("2026-09-10 10:00")}, // '10 Sep 2026' sorts after it as text, descending first
		{"UNI-T5", "2026-09-02", at("2026-09-02 10:00")},
	} {
		if _, err := st.DB.C("uniform_issue").Insert(ctx, bson.M{"_id": u.id, "employee_id": "EMP-1020", "store_id": "central", "lines": bson.A{},
			"amount": 0, "recover": false, "by_name": "test", "at": u.at, "issued_on": u.on}); err != nil {
			t.Fatal(err)
		}
	}
	s, d := c.do("GET", "/api/inventory/employee/EMP-1020", "EMP-1002", "HR Admin", nil)
	if s != 200 {
		t.Fatal(d)
	}
	var got []string
	for _, x := range d["uniforms"].([]any) {
		got = append(got, x.(map[string]any)["id"].(string))
	}
	if fmt.Sprint(got) != "[UNI-T5 UNI-T4 UNI-T3]" {
		t.Fatalf("uniform order %v, want [UNI-T5 UNI-T4 UNI-T3]", got)
	}
}
