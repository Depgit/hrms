package mdm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// Push delivery (SOP §5, signed HTTP webhooks): each active subscriber gets the outbox events in pos order, one
// POST per event, from its cursor (the last pos delivered). A 2xx answer moves the cursor on; anything else (or no
// answer) keeps it where it is and the subscriber waits base·2^(n-1) (at most max) before the same event is tried
// again: head-of-line blocking per subscriber, so an event is never skipped and never overtaken. Events of types a
// subscriber did not ask for are passed over. Delivery is at least once: consumers drop repeats by event_id.
//
// Every request carries
//
//	X-HRMS-Event-Id    the event_id
//	X-HRMS-Event-Type  the event_type
//	X-HRMS-Timestamp   the time of this attempt, Unix seconds
//	X-HRMS-Signature   sha256=hex(HMAC-SHA256(secret, timestamp + "." + body))

// Event is one published employee event as consumers receive it.
type Event struct {
	EventID       string         `json:"event_id"`
	EventType     string         `json:"event_type"`
	SchemaVersion string         `json:"schema_version"`
	Timestamp     string         `json:"timestamp"`
	Source        string         `json:"source"`
	Sequence      int64          `json:"sequence"`
	Pos           int64          `json:"pos"`
	Data          map[string]any `json:"data"`
}

func eventOf(r store.Row) Event {
	d, _ := r["data"].(map[string]any)
	return Event{EventID: str(r["event_id"]), EventType: str(r["event_type"]), SchemaVersion: str(r["schema_version"]),
		Timestamp: str(r["timestamp"]), Source: str(r["source"]), Sequence: i64(r["sequence"]), Pos: i64(r["pos"]), Data: d}
}

// Body is the event's JSON as sent.
func (e Event) Body() []byte { return Canonical(e) }

// deliveryBudget bounds one subscriber's turn in one delivery run.
const (
	deliveryMaxEvents = 1000
	deliveryMaxTime   = 2 * time.Minute
	deliveryClaim     = 5 * time.Minute
)

// DeliverDue delivers to every active subscriber that is not waiting out a backoff, and goes round again while
// new events keep arriving (a few rounds at most; the schedule covers the rest). It returns how many events were
// delivered. One subscriber's failure does not stop the others.
func (s *Service) DeliverDue(ctx context.Context) (int, error) {
	total := 0
	for round := 0; round < 5; round++ {
		n, err := s.deliverRound(ctx)
		total += n
		if err != nil || n == 0 {
			return total, err
		}
	}
	return total, nil
}

func (s *Service) deliverRound(ctx context.Context) (int, error) {
	now := s.now()
	subs, err := s.st.DB.C(collSub).Find(ctx, bson.M{"active": true,
		"$or": bson.A{bson.M{"next_attempt_at": nil}, bson.M{"next_attempt_at": bson.M{"$lte": now}}}},
		findSortLimit(bson.D{{Key: "created_at", Value: 1}, {Key: "_id", Value: 1}}, 0))
	if err != nil {
		return 0, err
	}
	head, err := s.Head(ctx, s.st.DB)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, sub := range subs {
		if i64(sub["cursor"]) >= head {
			continue
		}
		n, err := s.deliverOne(ctx, str(sub["id"]))
		total += n
		if err != nil {
			s.logf("delivery to %s: %v", str(sub["name"]), err)
		}
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
	}
	return total, nil
}

// deliverOne claims a subscriber (so two servers never deliver to it at once) and delivers what it can.
func (s *Service) deliverOne(ctx context.Context, id string) (int, error) {
	now := s.now()
	claim := now.Add(deliveryClaim)
	sub, err := s.st.DB.C(collSub).FindOneAndUpdate(ctx,
		bson.M{"_id": id, "active": true, "$or": bson.A{bson.M{"claim_until": nil}, bson.M{"claim_until": bson.M{"$lt": now}}}},
		bson.M{"$set": bson.M{"claim_until": claim}})
	if err != nil || sub == nil {
		return 0, err
	}
	defer func() {
		_, _ = s.st.DB.C(collSub).Update(context.WithoutCancel(ctx), bson.M{"_id": id, "claim_until": claim}, bson.M{"$set": bson.M{"claim_until": nil}})
	}()
	secret, err := s.unseal(aadSubscriberSecret, bytesOf(sub["secret"]))
	if err != nil {
		return 0, s.failed(ctx, sub, 0, fmt.Errorf("the signing secret cannot be read (was the app's secret key changed?): rotate it"))
	}
	types := strs(sub["event_types"])
	cursor := i64(sub["cursor"])
	start := time.Now()
	sent := 0
	for sent < deliveryMaxEvents && time.Since(start) < deliveryMaxTime {
		rows, err := s.st.DB.C(collOutbox).Find(ctx, bson.M{"pos": bson.M{"$gt": cursor}}, findSortLimit(bson.D{{Key: "pos", Value: 1}}, 100))
		if err != nil {
			return sent, err
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			ev := eventOf(r)
			if len(types) > 0 && !slices.Contains(types, ev.EventType) {
				cursor = ev.Pos // not asked for: passed over
				continue
			}
			status, err := s.post(ctx, str(sub["url"]), secret, ev)
			if err != nil {
				if cursor > i64(sub["cursor"]) {
					_ = s.advance(ctx, id, cursor, nil)
				}
				return sent, s.failed(ctx, sub, status, err)
			}
			cursor = ev.Pos
			sent++
			if err := s.advance(ctx, id, cursor, &status); err != nil {
				return sent, err
			}
			sub["cursor"] = cursor
		}
	}
	if cursor > i64(sub["cursor"]) {
		if err := s.advance(ctx, id, cursor, nil); err != nil {
			return sent, err
		}
	}
	return sent, nil
}

func bytesOf(v any) []byte {
	switch x := v.(type) {
	case []byte:
		return x
	case string:
		return []byte(x)
	}
	return nil
}

// advance moves the cursor forward (never back) and, after a delivery (status set), clears the failure state.
func (s *Service) advance(ctx context.Context, id string, cursor int64, status *int) error {
	upd := bson.M{"$max": bson.M{"cursor": cursor}}
	if status != nil {
		upd["$set"] = bson.M{"attempts": 0, "next_attempt_at": nil, "last_error": nil, "failing_since": nil,
			"last_status": *status, "last_delivered_at": s.now()}
	}
	_, err := s.st.DB.C(collSub).Update(ctx, bson.M{"_id": id}, upd)
	return err
}

// failed records a failed attempt and when to try again.
func (s *Service) failed(ctx context.Context, sub store.Row, status int, cause error) error {
	n := int(i64(sub["attempts"])) + 1
	now := s.now()
	set := bson.M{"attempts": n, "next_attempt_at": now.Add(s.backoff(n)), "last_error": trimErr(cause.Error())}
	if status > 0 {
		set["last_status"] = status
	} else {
		set["last_status"] = nil
	}
	if sub["failing_since"] == nil {
		set["failing_since"] = now
	}
	if _, err := s.st.DB.C(collSub).Update(ctx, bson.M{"_id": sub["id"]}, bson.M{"$set": set}); err != nil {
		return err
	}
	return cause
}

// backoff is the wait before attempt n+1: base·2^(n-1), at most max.
func (s *Service) backoff(n int) time.Duration {
	d := s.o.BackoffBase
	for i := 1; i < n && d < s.o.BackoffMax; i++ {
		d *= 2
	}
	return min(d, s.o.BackoffMax)
}

func trimErr(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 300 {
		return string(r[:300]) + "…"
	}
	return s
}

// post sends one signed event; it returns the HTTP status and an error unless the answer was 2xx.
func (s *Service) post(ctx context.Context, url, secret string, ev Event) (int, error) {
	body := ev.Body()
	ts := s.o.Now().Unix()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Saltstayz-HRMS-MDM/1")
	req.Header.Set("X-HRMS-Event-Id", ev.EventID)
	req.Header.Set("X-HRMS-Event-Type", ev.EventType)
	req.Header.Set("X-HRMS-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-HRMS-Signature", Sign(secret, ts, body))
	res, err := s.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(res.Body, 300))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		msg := fmt.Sprintf("HTTP %d", res.StatusCode)
		if t := strings.TrimSpace(string(snippet)); t != "" {
			msg += ": " + t
		}
		return res.StatusCode, fmt.Errorf("%s", msg)
	}
	return res.StatusCode, nil
}

// TestResult is what sending a test event to a subscriber gave.
type TestResult struct {
	OK     bool   `json:"ok"`
	Status int    `json:"status"`
	Error  string `json:"error"`
}

// SendTest posts a signed TEST event (not stored, not counted) to a subscriber, now.
func (s *Service) SendTest(ctx context.Context, id string) (TestResult, error) {
	sub, err := s.subscriber(ctx, id)
	if err != nil {
		return TestResult{}, err
	}
	secret, err := s.unseal(aadSubscriberSecret, bytesOf(sub["secret"]))
	if err != nil {
		return TestResult{}, userErr(409, "The signing secret cannot be read (the app's secret key changed?). Rotate it first")
	}
	ev := Event{EventID: store.NewID(), EventType: "TEST", SchemaVersion: SchemaVersion, Timestamp: utc(s.now()), Source: Source,
		Data: map[string]any{"message": "Test event from HRMS. Verify the signature, answer 2xx, and ignore it."}}
	status, err := s.post(ctx, str(sub["url"]), secret, ev)
	if err != nil {
		return TestResult{Status: status, Error: trimErr(err.Error())}, nil
	}
	return TestResult{OK: true, Status: status}, nil
}
