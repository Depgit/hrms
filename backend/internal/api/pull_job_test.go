package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The scheduled pull stores the office server's punches like pushed ones, asks for punches since the last pull,
// and keeps the result on the settings row.
func TestPunchPull(t *testing.T) {
	t.Setenv("PULL_ALLOW_LOCAL", "1")
	c, srv, st := setupFull(t)
	signAll(c, "EMP-1002")
	ctx := context.Background()
	day := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	var asked []string
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "k-123" {
			w.WriteHeader(401)
			return
		}
		asked = append(asked, r.URL.Query().Get("from"))
		_, _ = w.Write([]byte(`{"data":[{"emp_code":"EMP-1020","punch_time":"` + day + ` 06:02","punch_type":"IN"},` +
			`{"emp_code":"EMP-1020","punch_time":"` + day + ` 14:05","punch_type":"OUT"},{"emp_code":"EMP-9999","punch_time":"` + day + ` 07:00","punch_type":"IN"}]}`))
	}))
	defer vendor.Close()

	if s, r := c.do("POST", "/api/attendance/pull-config/pull", "EMP-1002", "HR Admin", nil); s != 400 {
		t.Fatal("a pull needs a saved address", r)
	}
	cfg := map[string]any{"url": vendor.URL + "/punches", "method": "GET", "auth": "API key", "key": "k-123", "since": "from", "every": 15,
		"empF": "emp_code", "timeF": "punch_time", "dirF": "punch_type", "inV": "IN", "outV": "OUT"}
	if s, r := c.do("PUT", "/api/attendance/pull-config", "EMP-1002", "HR Admin", cfg); s != 200 {
		t.Fatal(r)
	}
	s, r := c.do("POST", "/api/attendance/pull-config/pull", "EMP-1002", "HR Admin", nil)
	if s != 200 || r["stored"].(float64) != 2 || r["last_pull_ok"] != true || !strings.Contains(r["last_pull_note"].(string), "1 not stored") {
		t.Fatal("two punches stored, the unknown employee reported", r)
	}
	var in, out, src string
	if d, _ := st.DB.C("attendance_day").FindOne(ctx, bson.D{{Key: "employee_id", Value: "EMP-1020"}, {Key: "date", Value: day}}); d != nil {
		in, _ = d["in_time"].(string)
		out, _ = d["out_time"].(string)
		src, _ = d["source"].(string)
	}
	if in != "06:02" || out != "14:05" {
		t.Fatalf("the day is rebuilt from the pulled punches, got %q-%q (%s)", in, out, src)
	}
	// Not due yet: the scheduled check does nothing inside the interval.
	if n, err := srv.PullDue(ctx, false); err != nil || n != 0 || len(asked) != 1 {
		t.Fatal("not due inside the interval", n, err, asked)
	}
	// The next pull asks from the last one (less the overlap) and stores nothing twice.
	n, err := srv.PullDue(ctx, true)
	if err != nil || n != 0 || len(asked) != 2 || asked[1] <= asked[0] {
		t.Fatal("resent punches count once and the window moves forward", n, err, asked)
	}
	_, g := c.do("GET", "/api/attendance/pull-config", "EMP-1002", "HR Admin", nil)
	if g["last_pull_at"] == nil || g["key"] != nil {
		t.Fatal("the status shows; the key never does", g)
	}
}
