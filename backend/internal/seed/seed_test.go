package seed

// Seeds a fresh MongoDB database the way `hrms migrate && hrms seed -sample` does and checks what lands.
// Set HRMS_TEST_MONGO_URL; HRMS_TEST_MONGO_SEED_DB names the database (default hrms_test_seed), which is dropped
// first and left in place afterwards for inspection. Skipped otherwise.

import (
	"context"
	"os"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/store"
)

func TestSeedSample(t *testing.T) {
	url := os.Getenv("HRMS_TEST_MONGO_URL")
	if url == "" {
		t.Skip("HRMS_TEST_MONGO_URL not set")
	}
	name := os.Getenv("HRMS_TEST_MONGO_SEED_DB")
	if name == "" {
		name = "hrms_test_seed"
	}
	ctx := context.Background()
	st, err := store.Open(ctx, url, name)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.DB.Database().Drop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if err := Reference(ctx, st); err != nil {
		t.Fatal(err)
	}
	users, err := SampleOrg(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if users["HR Admin"] != StartingAdmin {
		t.Fatalf("users: %v", users)
	}
	if err := LinkCalendars(ctx, st); err != nil {
		t.Fatal(err)
	}
	days, err := SampleAttendance(ctx, st)
	if err != nil || days == 0 {
		t.Fatal(days, err)
	}
	set, err := StartingLogins(ctx, st)
	if err != nil || len(set) != 2 {
		t.Fatal(set, err)
	}
	// Running everything again changes nothing.
	if err := Reference(ctx, st); err != nil {
		t.Fatal(err)
	}
	if again, err := SampleOrg(ctx, st); err != nil || again != nil {
		t.Fatal(again, err)
	}
	if n, err := SampleAttendance(ctx, st); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if set, err := StartingLogins(ctx, st); err != nil || len(set) != 0 {
		t.Fatal(set, err)
	}
	// simple_master: 46 plus 8 job roles and 8 skills (owner, 14 Nov 2026).
	want := map[string]int64{"employee": 53, "employee_personal": 53, "employee_bank": 53, "gross_history": 53, "structure_map": 53,
		"region": 3, "state": 7, "city": 5, "cluster": 5, "location": 6, "department": 9, "designation": 16, "shift": 5, "shift_version": 5,
		"pay_band": 30, "user_role_map": 12, "work_policy": 5, "leave_plan": 3, "grade": 6, "salary_structure": 1, "auth_credential": 2,
		"simple_master": 62, "approval_flow": 34, "approval_matrix": 8, "employee_code_series": 4, "employee_code_settings": 1, "role": int64(len(access.Roles))}
	for coll, n := range want {
		if got, _ := st.DB.C(coll).Count(ctx, nil); got != n {
			t.Errorf("%s: %d documents, want %d", coll, got, n)
		}
	}
	for _, k := range store.SysKeys {
		if ok, _ := st.DB.C("simple_master").Exists(ctx, bson.M{"sys_key": k.Key}); !ok {
			t.Errorf("sys key %s not assigned", k.Key)
		}
	}
	if s, _ := st.DB.C("employee_code_series").FindOne(ctx, bson.M{"key": "All"}); s["next_no"] != int32(1054) {
		t.Errorf("next_no %v", s["next_no"])
	}
	wp, _ := st.DB.C("work_policy").FindOne(ctx, bson.M{"name": "Operations – 6 day"})
	if m, _ := wp["cal_map"].(map[string]any); len(m) != 5 {
		t.Errorf("cal_map %v", wp["cal_map"])
	}
	if store.SysName(ctx, st.DB, "mode.default") != "Biometric" {
		t.Error("SysName")
	}
}
