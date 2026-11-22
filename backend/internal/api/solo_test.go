package api_test

// An installation that has only its first admin (what `hrms create-admin` leaves) must open every dashboard tab:
// there are no colleagues, so the lists of ids the queries build are empty, and MongoDB refuses an empty "$in".
// HRMS_TEST_MONGO_URL as for the other tests; the database is hrms_test_solo_<name> (dropped first).

import (
	"context"
	"os"
	"testing"

	"github.com/saltstayz/hrms/internal/api"
	"github.com/saltstayz/hrms/internal/seed"
	"github.com/saltstayz/hrms/internal/storage"
	"github.com/saltstayz/hrms/internal/store"

	"net/http/httptest"
)

func TestFirstAdminOnlyDashboard(t *testing.T) {
	url := os.Getenv("HRMS_TEST_MONGO_URL")
	if url == "" {
		t.Skip("HRMS_TEST_MONGO_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, url, "hrms_test_solo_admin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.DB.Database().Drop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if err := seed.Reference(ctx, st); err != nil {
		t.Fatal(err)
	}
	const pw = "Str0ng!Passw0rd"
	id, err := seed.FirstAdmin(ctx, st, seed.FirstAdminInput{Name: "Asha Rao", Mobile: "9876543210", Password: pw})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.New(st, api.Options{Files: &storage.Memory{}}).Routes())
	t.Cleanup(srv.Close)
	c := &client{t: t, base: srv.URL, tokens: map[string]string{}, st: st}
	code, b := c.do("POST", "/api/auth/login", "", "", map[string]string{"employeeId": id, "password": pw})
	if code != 200 {
		t.Fatalf("the first admin must be able to sign in: %d %v", code, b)
	}
	c.tokens[id], _ = b["token"].(string)
	for _, tab := range []string{"ov", "pay", "att", "cmp"} {
		if code, b := c.do("GET", "/api/me/dashboard?tab="+tab, id, "Employee", nil); code != 200 {
			t.Fatalf("My view tab %s: %d %v", tab, code, b)
		}
	}
	for _, p := range []string{"/api/dashboard", "/api/me/directory", "/api/notices"} {
		if code, b := c.do("GET", p, id, "HR Admin", nil); code >= 500 {
			t.Fatalf("%s: %d %v", p, code, b)
		}
	}
}
