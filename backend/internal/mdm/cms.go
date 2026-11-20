package mdm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// HRMS as a consumer of the CMS's property events (CMS is the source of truth for property_id):
//
//	POST /api/mdm/cms/events
//	X-CMS-Timestamp: <Unix seconds>
//	X-CMS-Signature: sha256=hex(HMAC-SHA256(shared secret, timestamp + "." + raw body))
//
// A request is refused unless the signature matches and the timestamp is within the tolerance (5 minutes) of
// HRMS's clock, so a captured request cannot be replayed later; inside the window a replay carries the same
// event_id and is recognised as a duplicate (the inbox, mdm_inbox, keeps every event_id). The payload's
// schema_version picks a translator (v1 today); an unknown version is refused (422) and kept in the inbox as
// rejected. Per property, an event older than the last one applied (by sequence when both carry one, else by
// timestamp) is ignored. The local read-only copy is mdm_property; a location linked to the property follows its
// name, and retires when the property is deactivated (inactive_since today, reason "Deactivated in the CMS").

// Property event types the CMS sends.
const (
	PropertyCreated     = "PROPERTY_CREATED"
	PropertyUpdated     = "PROPERTY_UPDATED"
	PropertyDeactivated = "PROPERTY_DEACTIVATED"
)

// Envelope is the outer shape of every CMS event (all versions).
type Envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	SchemaVersion string          `json:"schema_version"`
	Timestamp     string          `json:"timestamp"`
	Source        string          `json:"source"`
	Sequence      *int64          `json:"sequence"`
	Data          json.RawMessage `json:"data"`
}

// Property is one CMS property in HRMS's internal form (what every translator produces).
type Property struct {
	PropertyID string
	Name       string
	City       string
	State      string
	Status     string // Active | Inactive
}

// PropertyEvent is a translated CMS event.
type PropertyEvent struct {
	EventID   string
	EventType string
	Version   string
	Time      time.Time
	Sequence  *int64
	Property  Property
}

// A Translator turns an envelope of its schema version into a PropertyEvent (backward translation: a newer
// payload shape gets its own translator, older ones stay).
type Translator func(env Envelope) (PropertyEvent, error)

// Translators by schema_version.
var Translators = map[string]Translator{"v1": propertyV1}

// SupportedVersions lists the schema versions HRMS accepts, sorted.
func SupportedVersions() []string {
	out := make([]string, 0, len(Translators))
	for v := range Translators {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// propertyV1 reads data {property_id, name, city, state, status}.
func propertyV1(env Envelope) (PropertyEvent, error) {
	var d struct {
		PropertyID string `json:"property_id"`
		Name       string `json:"name"`
		City       string `json:"city"`
		State      string `json:"state"`
		Status     string `json:"status"`
	}
	if len(env.Data) == 0 || json.Unmarshal(env.Data, &d) != nil {
		return PropertyEvent{}, errors.New("data must be an object")
	}
	p := Property{PropertyID: strings.TrimSpace(d.PropertyID), Name: strings.TrimSpace(d.Name), City: strings.TrimSpace(d.City),
		State: strings.TrimSpace(d.State), Status: strings.TrimSpace(d.Status)}
	if p.PropertyID == "" || len(p.PropertyID) > 64 {
		return PropertyEvent{}, errors.New("data.property_id is required (at most 64 characters)")
	}
	if env.EventType != PropertyDeactivated && p.Name == "" {
		return PropertyEvent{}, errors.New("data.name is required")
	}
	if len([]rune(p.Name)) > 120 {
		return PropertyEvent{}, errors.New("data.name is too long: 120 characters at most")
	}
	switch {
	case env.EventType == PropertyDeactivated:
		p.Status = "Inactive"
	case p.Status == "":
		p.Status = "Active"
	case p.Status != "Active" && p.Status != "Inactive":
		return PropertyEvent{}, errors.New(`data.status must be "Active" or "Inactive"`)
	}
	return PropertyEvent{EventID: env.EventID, EventType: env.EventType, Version: "v1", Sequence: env.Sequence, Property: p}, nil
}

// Receipt is what HRMS did with a CMS event.
type Receipt struct {
	EventID string `json:"event_id"`
	Status  string `json:"status"` // applied | ignored | duplicate
	Reason  string `json:"reason,omitempty"`
}

// CMSHandler serves POST /api/mdm/cms/events.
func (s *Service) CMSHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, "The body could not be read")
			return
		}
		if len(body) > 1<<20 {
			writeError(w, http.StatusRequestEntityTooLarge, "One event per request, at most 1 MB")
			return
		}
		rc, err := s.ReceiveCMS(r.Context(), r.Header.Get("X-CMS-Timestamp"), r.Header.Get("X-CMS-Signature"), body)
		if err != nil {
			if e, ok := AsError(err); ok {
				writeError(w, e.Status, e.Message)
				return
			}
			s.logf("cms event: %v", err)
			writeError(w, http.StatusInternalServerError, "Something went wrong; send the event again")
			return
		}
		writeJSON(w, http.StatusOK, rc)
	})
}

// ReceiveCMS verifies and applies one CMS event (the handler's work, without HTTP).
func (s *Service) ReceiveCMS(ctx context.Context, tsHeader, sigHeader string, body []byte) (Receipt, error) {
	set, err := s.settingRow(ctx, s.st.DB)
	if err != nil {
		return Receipt{}, err
	}
	if set == nil || len(bytesOf(set["inbound_secret"])) == 0 {
		return Receipt{}, userErr(http.StatusServiceUnavailable, "The CMS connection is not set up in HRMS yet")
	}
	secret, err := s.unseal(aadInboundSecret, bytesOf(set["inbound_secret"]))
	if err != nil {
		return Receipt{}, userErr(http.StatusServiceUnavailable, "HRMS cannot read its CMS secret; an HR Admin must generate a new one")
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(tsHeader), 10, 64)
	if err != nil {
		return Receipt{}, userErr(http.StatusUnauthorized, "X-CMS-Timestamp must be the time in Unix seconds")
	}
	if d := s.o.Now().Sub(time.Unix(ts, 0)); d > s.o.Tolerance || d < -s.o.Tolerance {
		return Receipt{}, userErr(http.StatusUnauthorized, "X-CMS-Timestamp is more than %d minutes from HRMS's clock: stale or replayed request", int(s.o.Tolerance/time.Minute))
	}
	if !signatureOK(secret, ts, body, strings.TrimSpace(sigHeader)) {
		return Receipt{}, userErr(http.StatusUnauthorized, "X-CMS-Signature does not match")
	}
	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return Receipt{}, userErr(http.StatusBadRequest, "The body must be one JSON event")
	}
	env.EventID = strings.TrimSpace(env.EventID)
	if env.EventID == "" || len(env.EventID) > 100 {
		return Receipt{}, userErr(http.StatusBadRequest, "event_id is required (at most 100 characters)")
	}
	switch env.EventType {
	case PropertyCreated, PropertyUpdated, PropertyDeactivated:
	default:
		return Receipt{}, userErr(http.StatusBadRequest, "event_type must be PROPERTY_CREATED, PROPERTY_UPDATED or PROPERTY_DEACTIVATED")
	}
	at, err := time.Parse(time.RFC3339Nano, env.Timestamp)
	if err != nil {
		return Receipt{}, userErr(http.StatusBadRequest, "timestamp must be RFC 3339, for example 2026-11-11T09:30:00Z")
	}
	tr, ok := Translators[env.SchemaVersion]
	if !ok {
		reason := fmt.Sprintf("Schema version %q is not supported (HRMS accepts %s)", env.SchemaVersion, strings.Join(SupportedVersions(), ", "))
		if err := s.reject(ctx, env, at, body, reason); err != nil {
			return Receipt{}, err
		}
		return Receipt{}, userErr(http.StatusUnprocessableEntity, "%s", reason)
	}
	ev, err := tr(env)
	if err != nil {
		if err2 := s.reject(ctx, env, at, body, err.Error()); err2 != nil {
			return Receipt{}, err2
		}
		return Receipt{}, userErr(http.StatusUnprocessableEntity, "%s", err.Error())
	}
	ev.Time = at
	return s.applyEvent(ctx, ev, body)
}

// reject keeps a refused event in the inbox (a later delivery of the same event_id is evaluated again).
func (s *Service) reject(ctx context.Context, env Envelope, at time.Time, body []byte, reason string) error {
	var pid any
	var d struct {
		PropertyID string `json:"property_id"`
	}
	if json.Unmarshal(env.Data, &d) == nil && d.PropertyID != "" {
		pid = d.PropertyID
	}
	_, err := s.st.DB.C(collInbox).Upsert(ctx, bson.M{"event_id": env.EventID, "status": "rejected"},
		bson.M{"event_type": env.EventType, "schema_version": env.SchemaVersion, "property_id": pid, "event_time": at, "sequence": env.Sequence,
			"reason": reason, "payload": json.RawMessage(body), "received_at": s.now()}, nil)
	if code, _ := store.ErrCode(err); code == "23505" {
		return nil // already applied or ignored under this event_id
	}
	return err
}

// older says whether ev is older than the last state applied to the property.
func older(ev PropertyEvent, cur store.Row) (bool, string) {
	if cur == nil {
		return false, ""
	}
	if ev.Sequence != nil && cur["last_sequence"] != nil {
		if last := i64(cur["last_sequence"]); *ev.Sequence <= last {
			return true, fmt.Sprintf("Older than the last applied event (sequence %d, last %d)", *ev.Sequence, last)
		}
		return false, ""
	}
	if t, ok := cur["last_event_at"].(time.Time); ok && ev.Time.Before(t) {
		return true, fmt.Sprintf("Older than the last applied event (%s, last %s)", utc(ev.Time), utc(t))
	}
	return false, ""
}

// applyEvent applies a translated event once, in one transaction with its inbox entry.
func (s *Service) applyEvent(ctx context.Context, ev PropertyEvent, body []byte) (Receipt, error) {
	var rc Receipt
	err := s.st.Tx(ctx, func(tx store.Q) error {
		rc = Receipt{EventID: ev.EventID}
		// Events of one property, and copies of one event, take turns.
		if err := store.AdvisoryLock(ctx, tx, "mdm_property:"+ev.Property.PropertyID); err != nil {
			return err
		}
		if err := store.AdvisoryLock(ctx, tx, "mdm_inbox:"+ev.EventID); err != nil {
			return err
		}
		seen, err := tx.C(collInbox).FindOne(ctx, bson.M{"event_id": ev.EventID})
		if err != nil {
			return err
		}
		if seen != nil && str(seen["status"]) != "rejected" {
			rc.Status, rc.Reason = "duplicate", "Already received ("+str(seen["status"])+")"
			return nil
		}
		cur, err := tx.C(collProperty).FindOne(ctx, bson.M{"property_id": ev.Property.PropertyID})
		if err != nil {
			return err
		}
		status, reason := "applied", ""
		if old, why := older(ev, cur); old {
			status, reason = "ignored", why
		} else {
			notes, err := s.applyProperty(ctx, tx, ev.Property, "event", &ev)
			if err != nil {
				return err
			}
			reason = strings.Join(notes, "; ")
		}
		var seq any
		if ev.Sequence != nil {
			seq = *ev.Sequence
		}
		if _, err := tx.C(collInbox).Upsert(ctx, bson.M{"event_id": ev.EventID}, bson.M{"event_type": ev.EventType, "schema_version": ev.Version,
			"property_id": ev.Property.PropertyID, "event_time": ev.Time, "sequence": seq, "status": status, "reason": nilIfEmpty(reason),
			"payload": json.RawMessage(body), "received_at": tx.Now()}, nil); err != nil {
			return err
		}
		rc.Status, rc.Reason = status, reason
		return nil
	})
	if code, _ := store.ErrCode(err); code == "23505" {
		return Receipt{EventID: ev.EventID, Status: "duplicate", Reason: "Already received"}, nil
	}
	return rc, err
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// applyProperty writes a property's state to the local copy and makes its linked location follow. ev is the
// event it comes from (nil from a reconciliation). It returns notes about what could not follow. Inside tx.
func (s *Service) applyProperty(ctx context.Context, tx store.Q, p Property, source string, ev *PropertyEvent) ([]string, error) {
	set := bson.M{"name": p.Name, "city": nilIfEmpty(p.City), "state": nilIfEmpty(p.State), "status": p.Status, "source": source, "updated_at": tx.Now()}
	if ev != nil {
		set["last_event_at"], set["last_event_id"] = ev.Time, ev.EventID
		if ev.Sequence != nil {
			set["last_sequence"] = *ev.Sequence
		}
	}
	if p.Name == "" { // a deactivation may come without the name: keep the one known
		cur, err := tx.C(collProperty).FindOne(ctx, bson.M{"property_id": p.PropertyID})
		if err != nil {
			return nil, err
		}
		if cur == nil {
			set["name"] = p.PropertyID
		} else {
			set["name"] = cur["name"]
		}
		p.Name = str(set["name"])
	}
	if _, err := tx.C(collProperty).Upsert(ctx, bson.M{"property_id": p.PropertyID}, set, nil); err != nil {
		return nil, err
	}
	loc, err := tx.C("location").FindOne(ctx, bson.M{"property_id": p.PropertyID})
	if err != nil || loc == nil {
		return nil, err
	}
	return s.follow(ctx, tx, loc, p)
}

// follow makes a linked location agree with its property: the CMS name, retired when the property is inactive,
// active again when the CMS reactivates a property it had deactivated.
func (s *Service) follow(ctx context.Context, tx store.Q, loc store.Row, p Property) ([]string, error) {
	var notes []string
	id := str(loc["id"])
	if p.Name != "" && str(loc["name"]) != p.Name {
		clash, err := tx.C("location").FindOne(ctx, bson.M{"name": noCase(p.Name), "_id": bson.M{"$ne": id}})
		if err != nil {
			return nil, err
		}
		if clash != nil {
			notes = append(notes, fmt.Sprintf("Location “%s” was not renamed to “%s”: location “%s” already has that name", str(loc["name"]), p.Name, str(clash["name"])))
		} else if err := s.renameLocation(ctx, tx, id, p.Name); err != nil {
			return nil, err
		}
	}
	today := store.DateOf(tx.Now())
	since := str(loc["inactive_since"])
	switch {
	case p.Status == "Inactive" && str(loc["status"]) == "Active" && (since == "" || since > today):
		if err := s.retireLocation(ctx, tx, id, today, CMSRetireReason); err != nil {
			return nil, err
		}
	case p.Status == "Active" && str(loc["inactive_reason"]) == CMSRetireReason:
		if err := s.reactivateLocation(ctx, tx, id); err != nil {
			return nil, err
		}
	}
	return notes, nil
}
