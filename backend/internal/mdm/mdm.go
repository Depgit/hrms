// Package mdm implements the client's Cross-Product Master Data SOP (SOP-ARCH-MDM-01, approved 10 Nov 2026) in
// HRMS:
//
//   - HRMS is the source of truth (SoT) for employee_code. A change-stream watcher (the "transactional database
//     hook") sees every committed change to employees, and to the designation, department, location and grade
//     names that appear in the published snapshot. For each affected employee it computes the published snapshot
//     (schema v1, no personal contact data, salary, bank or government ids), and when it differs from the last
//     one published, writes an outbox event and the new snapshot in ONE transaction (snapshot.go, publish.go,
//     watcher.go).
//   - The outbox is transport-agnostic. Two transports read it: signed HTTP webhooks pushed in order to each
//     subscriber, never skipping (deliver.go), and an authenticated pull feed plus snapshot and summary endpoints
//     for the consumers' nightly reconciliation (feed.go). A broker publisher (Kafka, NATS) can be added later as
//     another reader of the outbox.
//   - CMS is the SoT for property_id; HRMS keeps a read-only copy (mdm_property) fed by signed, idempotent,
//     versioned property events (cms.go), and locations linked to a property follow its name and status.
//   - A nightly reconciliation (SOP §6) recomputes every employee's snapshot and, as a consumer, compares the
//     CMS's property list with the local copy (reconcile.go).
//
// Stack rule: Go + MongoDB, the app's own job queue (internal/jobs); no broker, no Redis.
package mdm

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// SchemaVersion is the version of the employee event payload HRMS publishes.
const SchemaVersion = "v1"

// Source names HRMS in every event it publishes.
const Source = "HRMS"

// Employee event types (SOP §4).
const (
	EmployeeCreated     = "EMPLOYEE_CREATED"
	EmployeeUpdated     = "EMPLOYEE_UPDATED"
	EmployeeSeparated   = "EMPLOYEE_SEPARATED"   // status became Inactive
	EmployeeReactivated = "EMPLOYEE_REACTIVATED" // status left Inactive
)

// EventTypes lists the employee event types, in the order the settings screen shows them.
var EventTypes = []string{EmployeeCreated, EmployeeUpdated, EmployeeSeparated, EmployeeReactivated}

// CMSRetireReason is the reason a location retired because its CMS property was deactivated carries.
const CMSRetireReason = "Deactivated in the CMS"

// Collections.
const (
	collOutbox    = "mdm_outbox"
	collPublished = "mdm_published"
	collSub       = "mdm_subscriber"
	collKey       = "mdm_feed_key"
	collInbox     = "mdm_inbox"
	collProperty  = "mdm_property"
	collSetting   = "mdm_setting"
	collRun       = "mdm_reconcile_run"
	collState     = "mdm_state"
)

// LocationHooks change a location the way the Masters screen does. The app (internal/api) plugs in its own master
// rules (saveMaster, retireMaster, reactivateMaster); when a hook is nil the built-in equivalent in locations.go is
// used. Each runs inside the caller's transaction.
type LocationHooks struct {
	Rename     func(ctx context.Context, tx store.Q, id, name string) error
	Retire     func(ctx context.Context, tx store.Q, id, since, reason string) error
	Reactivate func(ctx context.Context, tx store.Q, id string) error
}

// Options configure a Service. Zero values take the defaults.
type Options struct {
	// Key (32 bytes) seals the stored secrets (webhook secrets, the CMS secrets): the app's secret key, the one
	// that seals the Google Drive token. Without one a key for this process only is made (tests).
	Key []byte
	// HTTPClient sends webhooks and calls the CMS (default: 15s timeout, no redirects followed).
	HTTPClient *http.Client
	// Now is the clock (tests).
	Now func() time.Time
	// Logf logs (default log.Printf).
	Logf func(format string, a ...any)
	// BackoffBase and BackoffMax shape a failing subscriber's retries: base·2^(n-1), at most max (10s, 1h).
	BackoffBase, BackoffMax time.Duration
	// Tolerance is how far an inbound CMS request's timestamp may be from now (5 minutes).
	Tolerance time.Duration
	// Hooks change locations by the app's master rules (see LocationHooks).
	Hooks LocationHooks
	// ChunkSize is how many employees one publishing transaction handles (200).
	ChunkSize int
}

// Service is the master data sync of one HRMS server.
type Service struct {
	st   *store.Store
	key  []byte
	hc   *http.Client
	o    Options
	kick func() // wakes the delivery (set by the job queue glue)
	mu   sync.Mutex
}

// New returns the service over st.
func New(st *store.Store, o Options) *Service {
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}
	if o.BackoffBase <= 0 {
		o.BackoffBase = 10 * time.Second
	}
	if o.BackoffMax <= 0 {
		o.BackoffMax = time.Hour
	}
	if o.Tolerance <= 0 {
		o.Tolerance = 5 * time.Minute
	}
	if o.ChunkSize <= 0 {
		o.ChunkSize = 200
	}
	return &Service{st: st, key: sealKey(o.Key), hc: o.HTTPClient, o: o}
}

// SetKick sets what wakes the push delivery after events are written (the job queue inserts a delivery job).
func (s *Service) SetKick(fn func()) {
	s.mu.Lock()
	s.kick = fn
	s.mu.Unlock()
}

func (s *Service) wake() {
	s.mu.Lock()
	fn := s.kick
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (s *Service) now() time.Time { return s.o.Now().Truncate(time.Millisecond) }

func (s *Service) logf(format string, a ...any) { s.o.Logf("mdm: "+format, a...) }

// Error is a refusal the API shows as is: Status is the HTTP status, Message the text.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

func userErr(status int, msg string, a ...any) error {
	return &Error{Status: status, Message: fmt.Sprintf(msg, a...)}
}

// AsError returns the *Error inside err, if any.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// ---------- small helpers ----------

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// strOrNil is a text value as published: a string, or nil.
func strOrNil(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return x
	case fmt.Stringer:
		return x.String()
	}
	return fmt.Sprint(v)
}

func i64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int32:
		return int64(x)
	case int:
		return int64(x)
	case float64:
		return int64(x)
	}
	return 0
}

func timeOrNil(v any) any {
	if t, ok := v.(time.Time); ok {
		return t
	}
	return nil
}

func strs(v any) []string {
	out := []string{}
	if l, ok := v.([]any); ok {
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func findSortLimit(sort bson.D, limit int64) *options.FindOptionsBuilder {
	o := options.Find().SetSort(sort)
	if limit > 0 {
		o.SetLimit(limit)
	}
	return o
}

// utc formats an instant as published: UTC RFC 3339 with milliseconds.
func utc(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z07:00") }

// ensureState makes sure a state document exists (outside any transaction), so transactions only ever update it.
func (s *Service) ensureState(ctx context.Context, id string) error {
	ok, err := s.st.DB.C(collState).Exists(ctx, bson.M{"_id": id})
	if err != nil || ok {
		return err
	}
	_, err = s.st.DB.C(collState).Upsert(ctx, bson.M{"_id": id}, nil, bson.M{"updated_at": s.now()})
	if code, _ := store.ErrCode(err); code == "23505" {
		return nil
	}
	return err
}

// validURL accepts an absolute http(s) URL with a host.
func validURL(u string) bool {
	u = strings.TrimSpace(u)
	return (strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")) && len(u) > len("https://") && !strings.ContainsAny(u, " \t\r\n\"<>")
}
