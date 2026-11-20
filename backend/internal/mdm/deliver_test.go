package mdm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/jobs"
)

// hook is a consumer's webhook endpoint that checks every request's signature and can fail on purpose.
type hook struct {
	mu      sync.Mutex
	secrets map[string]string // path → secret
	fail    map[int64]int     // pos → how many more attempts to answer 500
	tries   []string          // "path pos ok|fail" in arrival order
	ok      map[string][]int64
	bad     []string
}

func newHook() (*hook, *httptest.Server) {
	h := &hook{secrets: map[string]string{}, fail: map[int64]int{}, ok: map[string][]int64{}}
	return h, httptest.NewServer(h)
}

func (h *hook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	defer h.mu.Unlock()
	ts, _ := strconv.ParseInt(r.Header.Get("X-HRMS-Timestamp"), 10, 64)
	var ev Event
	_ = json.Unmarshal(body, &ev)
	switch {
	case !signatureOK(h.secrets[r.URL.Path], ts, body, r.Header.Get("X-HRMS-Signature")):
		h.bad = append(h.bad, "signature")
	case time.Since(time.Unix(ts, 0)).Abs() > 5*time.Minute:
		h.bad = append(h.bad, "timestamp")
	case r.Header.Get("X-HRMS-Event-Id") != ev.EventID || r.Header.Get("X-HRMS-Event-Type") != ev.EventType:
		h.bad = append(h.bad, "headers")
	case r.Header.Get("Content-Type") != "application/json" || r.Method != http.MethodPost:
		h.bad = append(h.bad, "request")
	}
	if h.fail[ev.Pos] > 0 {
		h.fail[ev.Pos]--
		h.tries = append(h.tries, r.URL.Path+" "+strconv.FormatInt(ev.Pos, 10)+" fail")
		http.Error(w, "temporarily down", http.StatusInternalServerError)
		return
	}
	h.tries = append(h.tries, r.URL.Path+" "+strconv.FormatInt(ev.Pos, 10)+" ok")
	h.ok[r.URL.Path] = append(h.ok[r.URL.Path], ev.Pos)
	w.WriteHeader(http.StatusNoContent)
}

func (h *hook) snapshot() ([]string, map[string][]int64, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ok := map[string][]int64{}
	for k, v := range h.ok {
		ok[k] = append([]int64(nil), v...)
	}
	return append([]string(nil), h.tries...), ok, append([]string(nil), h.bad...)
}

func ptr[T any](v T) *T { return &v }

// Push: signed, in pos order, head-of-line retry with backoff, never skipping; type filters pass events over.
func TestPushDelivery(t *testing.T) {
	svc, st, ctx := testEnv(t, Options{BackoffBase: 300 * time.Millisecond, BackoffMax: 2 * time.Second})
	o := seedOrg(t, ctx, st)
	for _, c := range []string{"EMP-5001", "EMP-5002", "EMP-5003"} {
		addEmployee(t, ctx, st, o, c, "Person "+c, nil)
	}
	if _, err := svc.Publish(ctx, []string{"EMP-5001", "EMP-5002", "EMP-5003"}, nil); err != nil {
		t.Fatal(err)
	}
	h, srv := newHook()
	defer srv.Close()
	pd, sec, err := svc.AddSubscriber(ctx, "Asha (EMP-1)", SubscriberInput{Name: ptr("Purchase Desk"), URL: ptr(srv.URL + "/pd")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sec, prefixWebhookSecret) || pd["secretLast4"] != last4(sec) || pd["behind"] != int64(3) {
		t.Fatalf("subscriber %v (secret %q)", pd, sec)
	}
	mb, sec2, err := svc.AddSubscriber(ctx, "", SubscriberInput{Name: ptr("Moneyball"), URL: ptr(srv.URL + "/mb"), EventTypes: []string{EmployeeSeparated}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.AddSubscriber(ctx, "", SubscriberInput{Name: ptr("moneyball"), URL: ptr(srv.URL)}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("a duplicate name: %v", err)
	}
	h.mu.Lock()
	h.secrets["/pd"], h.secrets["/mb"] = sec, sec2
	h.fail[2] = 2
	h.mu.Unlock()

	sub := func(id any) bson.M {
		r, err := st.DB.C(collSub).FindOne(ctx, bson.M{"_id": id})
		if err != nil || r == nil {
			t.Fatal(err)
		}
		return bson.M(r)
	}
	n, err := svc.DeliverDue(ctx)
	if err != nil || n != 1 {
		t.Fatalf("first run delivered %d (%v)", n, err)
	}
	p := sub(pd["id"])
	if i64(p["cursor"]) != 1 || i64(p["attempts"]) != 1 || p["failing_since"] == nil || !strings.HasPrefix(str(p["last_error"]), "HTTP 500") || i64(p["last_status"]) != 500 {
		t.Fatalf("after a failure: %v", p)
	}
	if m := sub(mb["id"]); i64(m["cursor"]) != 3 || i64(m["attempts"]) != 0 {
		t.Fatalf("a filtered subscriber should pass over the events: %v", m)
	}
	// Waiting out the backoff: nothing is tried.
	tries, _, _ := h.snapshot()
	if _, err := svc.DeliverDue(ctx); err != nil {
		t.Fatal(err)
	}
	if again, _, _ := h.snapshot(); len(again) != len(tries) {
		t.Fatalf("tried again during the backoff: %v", again)
	}
	time.Sleep(350 * time.Millisecond)
	if n, _ := svc.DeliverDue(ctx); n != 0 {
		t.Fatalf("second try delivered %d", n)
	}
	if p := sub(pd["id"]); i64(p["attempts"]) != 2 || !p["next_attempt_at"].(time.Time).After(time.Now().Add(400*time.Millisecond)) {
		t.Fatalf("the second failure should double the wait: %v", p)
	}
	time.Sleep(650 * time.Millisecond)
	if n, err := svc.DeliverDue(ctx); err != nil || n != 2 {
		t.Fatalf("third try delivered %d (%v)", n, err)
	}
	p = sub(pd["id"])
	if i64(p["cursor"]) != 3 || i64(p["attempts"]) != 0 || p["failing_since"] != nil || p["last_error"] != nil || p["last_delivered_at"] == nil {
		t.Fatalf("after recovering: %v", p)
	}
	tries, ok, bad := h.snapshot()
	if want := []string{"/pd 1 ok", "/pd 2 fail", "/pd 2 fail", "/pd 2 ok", "/pd 3 ok"}; !reflect.DeepEqual(tries, want) {
		t.Fatalf("attempts %v, want %v", tries, want)
	}
	if len(bad) > 0 {
		t.Fatalf("bad requests: %v", bad)
	}
	if !reflect.DeepEqual(ok["/pd"], []int64{1, 2, 3}) || len(ok["/mb"]) != 0 {
		t.Fatalf("delivered %v", ok)
	}

	// A separation reaches both; an update only Purchase Desk.
	setEmp(t, ctx, st, "EMP-5002", bson.M{"status": "Inactive"})
	setEmp(t, ctx, st, "EMP-5003", bson.M{"name": "Renamed"})
	if _, err := svc.Publish(ctx, []string{"EMP-5002", "EMP-5003"}, nil); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.DeliverDue(ctx); err != nil || n != 3 {
		t.Fatalf("delivered %d (%v)", n, err)
	}
	_, ok, bad = h.snapshot()
	if !reflect.DeepEqual(ok["/pd"], []int64{1, 2, 3, 4, 5}) || !reflect.DeepEqual(ok["/mb"], []int64{4}) || len(bad) > 0 {
		t.Fatalf("delivered %v, bad %v", ok, bad)
	}

	// Paused: nothing goes; a test event goes now, signed; rotating the secret signs with the new one.
	if _, err := svc.UpdateSubscriber(ctx, str(pd["id"]), SubscriberInput{Active: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	setEmp(t, ctx, st, "EMP-5001", bson.M{"name": "Paused Change"})
	_, _ = svc.Publish(ctx, []string{"EMP-5001"}, nil)
	if n, _ := svc.DeliverDue(ctx); n != 0 {
		t.Fatalf("a paused subscriber got %d events", n)
	}
	_, sec3, err := svc.RotateSubscriberSecret(ctx, str(pd["id"]))
	if err != nil || sec3 == sec {
		t.Fatal("rotate", err)
	}
	h.mu.Lock()
	h.secrets["/pd"] = sec3
	h.mu.Unlock()
	if res, err := svc.SendTest(ctx, str(pd["id"])); err != nil || !res.OK || res.Status != 204 {
		t.Fatalf("test event: %+v %v", res, err)
	}
	if _, err := svc.UpdateSubscriber(ctx, str(pd["id"]), SubscriberInput{Active: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if n, _ := svc.DeliverDue(ctx); n != 1 {
		t.Fatalf("resumed subscriber got %d events", n)
	}
	if _, _, bad := h.snapshot(); len(bad) > 0 {
		t.Fatalf("bad requests after rotating: %v", bad)
	}
	if err := svc.DeleteSubscriber(ctx, str(mb["id"])); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSubscriber(ctx, str(mb["id"])); err == nil {
		t.Fatal("deleted twice")
	}
}

// End to end through the job queue: the watcher sees an insert, a delivery job is queued at once, the webhook
// gets the event. Stopping the queue stops the watcher, which gives its lease up.
func TestJobQueueDelivery(t *testing.T) {
	svc, st, ctx := testEnv(t, Options{})
	o := seedOrg(t, ctx, st)
	h, srv := newHook()
	defer srv.Close()
	_, sec, err := svc.AddSubscriber(ctx, "", SubscriberInput{Name: ptr("PMS"), URL: ptr(srv.URL + "/pms")})
	if err != nil {
		t.Fatal(err)
	}
	h.secrets["/pms"] = sec
	w := jobs.NewWorkers()
	svc.AddWorkers(w)
	c, err := jobs.NewClient(st.DB.Database(), &jobs.Config{Queues: map[string]jobs.QueueConfig{jobs.QueueDefault: {MaxWorkers: 2}}, Workers: w,
		PeriodicJobs: []*jobs.PeriodicJob{DeliverPeriodicJob()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = c.Stop(context.Background())
		}
	}()
	wt := svc.Start(ctx, c, c.ID())
	waitFor(t, "the watcher to lead", 10*time.Second, wt.IsLeader)
	addEmployee(t, ctx, st, o, "EMP-6001", "Queued", nil)
	waitFor(t, "the webhook", 15*time.Second, func() bool { _, ok, _ := h.snapshot(); return len(ok["/pms"]) == 1 })
	if _, _, bad := h.snapshot(); len(bad) > 0 {
		t.Fatalf("bad requests: %v", bad)
	}
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	stopped = true
	waitFor(t, "the watcher to stop", 10*time.Second, func() bool { return !wt.IsLeader() })
	waitFor(t, "the lease to be given up", 10*time.Second, func() bool {
		r, _ := st.DB.C(collState).FindOne(ctx, bson.M{"_id": stateWatcher})
		exp, _ := r["expires_at"].(time.Time)
		return !exp.After(time.Now())
	})
}
