package seed

// The first-admin command on a fresh database: it needs the reference data, makes one HR Admin who can sign in,
// and refuses to run a second time. HRMS_TEST_MONGO_URL as for the other seed tests; the database is
// hrms_test_firstadmin (dropped first). Skipped otherwise.

import (
	"context"
	"os"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/store"
)

func TestFirstAdmin(t *testing.T) {
	url := os.Getenv("HRMS_TEST_MONGO_URL")
	if url == "" {
		t.Skip("HRMS_TEST_MONGO_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, url, "hrms_test_firstadmin")
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
	in := FirstAdminInput{Name: "Asha Rao", Mobile: "+91 98765 43210", Email: "asha@example.com", Password: "Str0ng!Passw0rd"}

	// Before the reference data is loaded it must say what to do.
	if _, err := FirstAdmin(ctx, st, in); err == nil || !strings.Contains(err.Error(), "hrms seed") {
		t.Fatalf("without reference data: %v", err)
	}
	if err := Reference(ctx, st); err != nil {
		t.Fatal(err)
	}
	// Bad input is refused before anything is written.
	for _, bad := range []FirstAdminInput{{Name: "", Mobile: "9876543210"}, {Name: "A", Mobile: "12345"}, {Name: "A", Mobile: "9876543210", Password: "weak"}} {
		if _, err := FirstAdmin(ctx, st, bad); err == nil {
			t.Fatalf("%+v should be refused", bad)
		}
	}
	if n, _ := st.DB.C("employee").Count(ctx, nil); n != 0 {
		t.Fatalf("a refused call must write nothing, found %d employees", n)
	}

	id, err := FirstAdmin(ctx, st, in)
	if err != nil {
		t.Fatal(err)
	}
	if id != "EMP-0001" {
		t.Fatalf("first Employee ID = %q", id)
	}
	if ok, _ := st.DB.C("user_role_map").Exists(ctx, bson.M{"employee_id": id, "role_name": "HR Admin", "access": "all"}); !ok {
		t.Fatal("no HR Admin role")
	}
	p, _ := st.DB.C("employee_personal").FindOne(ctx, bson.M{"employee_id": id})
	if p == nil || p["mobile"] != "9876543210" {
		t.Fatalf("mobile must be stored as 10 digits: %v", p)
	}
	c, _ := st.DB.C("auth_credential").FindOne(ctx, bson.M{"employee_id": id})
	if c == nil || c["hash"] != access.HashPassword(in.Password, c["salt"].(string)) {
		t.Fatal("the password must work")
	}
	// The next code moves on, and a second admin is refused.
	if _, err := FirstAdmin(ctx, st, in); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("a second run must be refused: %v", err)
	}
	if s, _ := st.DB.C("employee_code_series").FindOne(ctx, bson.M{"key": "All"}); s == nil || (s["next_no"] != int32(2) && s["next_no"] != int64(2)) {
		t.Fatalf("series did not advance: %v", s)
	}
}
