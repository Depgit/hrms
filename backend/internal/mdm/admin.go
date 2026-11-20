package mdm

import (
	"context"
	"slices"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// The settings behind Masters › Company › Integrations (HR Admin only; the API checks the role): push
// subscribers, pull-feed keys, the CMS connection, recent events and reconciliations. Secrets are shown once, in
// the answer to the call that makes them, and afterwards only by their last four characters.

func (s *Service) settingRow(ctx context.Context, q store.Q) (store.Row, error) {
	return q.C(collSetting).FindOne(ctx, bson.M{"_id": 1})
}

// SettingsInput changes the CMS connection; nil fields stay as they are.
type SettingsInput struct {
	CMSBaseURL        *string `json:"cmsBaseUrl"`
	CMSAPIKey         *string `json:"cmsApiKey"` // "" removes it
	PropertiesFromCMS *bool   `json:"propertiesFromCms"`
}

func settingsView(r store.Row) map[string]any {
	out := map[string]any{"inboundSecretSet": false, "inboundSecretLast4": "", "cmsBaseUrl": "", "cmsApiKeySet": false, "cmsApiKeyLast4": "",
		"propertiesFromCms": false, "updatedAt": nil, "updatedBy": "", "inboundPath": "/api/mdm/cms/events", "supportedVersions": SupportedVersions()}
	if r == nil {
		return out
	}
	out["inboundSecretSet"] = len(bytesOf(r["inbound_secret"])) > 0
	out["inboundSecretLast4"] = str(r["inbound_secret_last4"])
	out["cmsBaseUrl"] = str(r["cms_base_url"])
	out["cmsApiKeySet"] = len(bytesOf(r["cms_api_key"])) > 0
	out["cmsApiKeyLast4"] = str(r["cms_api_key_last4"])
	out["propertiesFromCms"] = r["properties_from_cms"] == true
	out["updatedAt"] = timeOrNil(r["updated_at"])
	out["updatedBy"] = str(r["updated_by"])
	return out
}

// SaveSettings changes the CMS connection.
func (s *Service) SaveSettings(ctx context.Context, by string, in SettingsInput) (map[string]any, error) {
	set := bson.M{"updated_at": s.now(), "updated_by": by}
	if in.CMSBaseURL != nil {
		u := strings.TrimRight(strings.TrimSpace(*in.CMSBaseURL), "/")
		if u != "" && (!validURL(u) || len(u) > 500) {
			return nil, userErr(400, "CMS address must be a web address starting with https:// (or http:// on a private network)")
		}
		set["cms_base_url"] = nilIfEmpty(u)
	}
	if in.CMSAPIKey != nil {
		k := strings.TrimSpace(*in.CMSAPIKey)
		if k == "" {
			set["cms_api_key"], set["cms_api_key_last4"] = nil, nil
		} else {
			if len(k) > 500 {
				return nil, userErr(400, "The CMS API key is too long")
			}
			sealed, err := s.seal(aadCMSAPIKey, k)
			if err != nil {
				return nil, err
			}
			set["cms_api_key"], set["cms_api_key_last4"] = sealed, last4(k)
		}
	}
	if in.PropertiesFromCMS != nil {
		set["properties_from_cms"] = *in.PropertiesFromCMS
	}
	if _, err := s.st.DB.C(collSetting).Upsert(ctx, bson.M{"_id": 1}, set, nil); err != nil {
		return nil, err
	}
	r, err := s.settingRow(ctx, s.st.DB)
	return settingsView(r), err
}

// RotateInboundSecret makes a new shared secret for the CMS's signed events and returns it (shown once). The old
// one stops working at once.
func (s *Service) RotateInboundSecret(ctx context.Context, by string) (string, map[string]any, error) {
	sec := newSecret(prefixInboundSecret)
	sealed, err := s.seal(aadInboundSecret, sec)
	if err != nil {
		return "", nil, err
	}
	if _, err := s.st.DB.C(collSetting).Upsert(ctx, bson.M{"_id": 1},
		bson.M{"inbound_secret": sealed, "inbound_secret_last4": last4(sec), "updated_at": s.now(), "updated_by": by}, nil); err != nil {
		return "", nil, err
	}
	r, err := s.settingRow(ctx, s.st.DB)
	return sec, settingsView(r), err
}

// ---------- push subscribers ----------

// SubscriberInput adds or changes a subscriber; nil fields stay as they are.
type SubscriberInput struct {
	Name       *string  `json:"name"`
	URL        *string  `json:"url"`
	EventTypes []string `json:"eventTypes"` // empty: every type
	Active     *bool    `json:"active"`
	Start      string   `json:"start"` // on add: "beginning" (default, every event kept) or "now"
}

func (s *Service) subscriber(ctx context.Context, id string) (store.Row, error) {
	r, err := s.st.DB.C(collSub).FindOne(ctx, bson.M{"_id": id})
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, userErr(404, "Subscriber not found")
	}
	return r, nil
}

func subscriberView(r store.Row, head int64) map[string]any {
	types := strs(r["event_types"])
	cur := i64(r["cursor"])
	state := "Up to date"
	switch {
	case r["active"] != true:
		state = "Paused"
	case r["failing_since"] != nil:
		state = "Failing"
	case cur < head:
		state = "Delivering"
	}
	return map[string]any{"id": r["id"], "name": r["name"], "url": r["url"], "secretLast4": r["secret_last4"], "eventTypes": types,
		"active": r["active"] == true, "cursor": cur, "behind": max(head-cur, 0), "attempts": i64(r["attempts"]), "nextAttemptAt": timeOrNil(r["next_attempt_at"]),
		"lastError": r["last_error"], "failingSince": timeOrNil(r["failing_since"]), "lastStatus": r["last_status"],
		"lastDeliveredAt": timeOrNil(r["last_delivered_at"]), "createdAt": timeOrNil(r["created_at"]), "createdBy": r["created_by"], "state": state}
}

func cleanTypes(l []string) ([]string, error) {
	out := []string{}
	for _, t := range l {
		t = strings.TrimSpace(t)
		if !slices.Contains(EventTypes, t) {
			return nil, userErr(400, "Unknown event type %s", t)
		}
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out, nil
}

func cleanName(p *string, what string) (string, error) {
	n := strings.TrimSpace(*p)
	if n == "" {
		return "", userErr(400, "Give the %s a name", what)
	}
	if len([]rune(n)) > 60 {
		return "", userErr(400, "The name is too long: 60 characters at most")
	}
	return n, nil
}

func dupName(err error, what, name string) error {
	if code, _ := store.ErrCode(err); code == "23505" {
		return userErr(409, "A %s named “%s” already exists", what, name)
	}
	return err
}

// AddSubscriber adds a push subscriber and returns it with its signing secret (shown once).
func (s *Service) AddSubscriber(ctx context.Context, by string, in SubscriberInput) (map[string]any, string, error) {
	if in.Name == nil || in.URL == nil {
		return nil, "", userErr(400, "Give the subscriber a name and a URL")
	}
	name, err := cleanName(in.Name, "subscriber")
	if err != nil {
		return nil, "", err
	}
	url := strings.TrimSpace(*in.URL)
	if !validURL(url) || len(url) > 500 {
		return nil, "", userErr(400, "The URL must be a web address starting with https:// (or http:// on a private network)")
	}
	types, err := cleanTypes(in.EventTypes)
	if err != nil {
		return nil, "", err
	}
	var cursor int64
	switch in.Start {
	case "", "beginning":
	case "now":
		if cursor, err = s.Head(ctx, s.st.DB); err != nil {
			return nil, "", err
		}
	default:
		return nil, "", userErr(400, `start must be "beginning" or "now"`)
	}
	if dup, err := s.st.DB.C(collSub).Exists(ctx, bson.M{"name": noCase(name)}); err != nil || dup {
		if err == nil {
			err = userErr(409, "A subscriber named “%s” already exists", name)
		}
		return nil, "", err
	}
	sec := newSecret(prefixWebhookSecret)
	sealed, err := s.seal(aadSubscriberSecret, sec)
	if err != nil {
		return nil, "", err
	}
	id, err := s.st.DB.C(collSub).Insert(ctx, bson.M{"name": name, "url": url, "secret": sealed, "secret_last4": last4(sec), "event_types": types,
		"cursor": cursor, "created_by": nilIfEmpty(by)})
	if err != nil {
		return nil, "", dupName(err, "subscriber", name)
	}
	r, err := s.subscriber(ctx, str(id))
	if err != nil {
		return nil, "", err
	}
	head, err := s.Head(ctx, s.st.DB)
	if err != nil {
		return nil, "", err
	}
	s.wake()
	return subscriberView(r, head), sec, nil
}

// UpdateSubscriber renames, re-points, re-filters, pauses or resumes a subscriber. Resuming or changing it clears
// the wait before the next attempt.
func (s *Service) UpdateSubscriber(ctx context.Context, id string, in SubscriberInput) (map[string]any, error) {
	r, err := s.subscriber(ctx, id)
	if err != nil {
		return nil, err
	}
	set := bson.M{"next_attempt_at": nil}
	if in.Name != nil {
		name, err := cleanName(in.Name, "subscriber")
		if err != nil {
			return nil, err
		}
		if dup, err := s.st.DB.C(collSub).Exists(ctx, bson.M{"name": noCase(name), "_id": bson.M{"$ne": id}}); err != nil || dup {
			if err == nil {
				err = userErr(409, "A subscriber named “%s” already exists", name)
			}
			return nil, err
		}
		set["name"] = name
	}
	if in.URL != nil {
		url := strings.TrimSpace(*in.URL)
		if !validURL(url) || len(url) > 500 {
			return nil, userErr(400, "The URL must be a web address starting with https:// (or http:// on a private network)")
		}
		set["url"] = url
	}
	if in.EventTypes != nil {
		types, err := cleanTypes(in.EventTypes)
		if err != nil {
			return nil, err
		}
		set["event_types"] = types
	}
	if in.Active != nil {
		set["active"] = *in.Active
	}
	if _, err := s.st.DB.C(collSub).Update(ctx, bson.M{"_id": id}, bson.M{"$set": set}); err != nil {
		return nil, dupName(err, "subscriber", str(set["name"]))
	}
	if r, err = s.subscriber(ctx, id); err != nil {
		return nil, err
	}
	head, err := s.Head(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	s.wake()
	return subscriberView(r, head), nil
}

// DeleteSubscriber removes a subscriber (its deliveries stop; the outbox stays).
func (s *Service) DeleteSubscriber(ctx context.Context, id string) error {
	n, err := s.st.DB.C(collSub).Delete(ctx, bson.M{"_id": id})
	if err == nil && n == 0 {
		err = userErr(404, "Subscriber not found")
	}
	return err
}

// RotateSubscriberSecret gives a subscriber a new signing secret and returns it (shown once). Deliveries from now
// on are signed with it.
func (s *Service) RotateSubscriberSecret(ctx context.Context, id string) (map[string]any, string, error) {
	if _, err := s.subscriber(ctx, id); err != nil {
		return nil, "", err
	}
	sec := newSecret(prefixWebhookSecret)
	sealed, err := s.seal(aadSubscriberSecret, sec)
	if err != nil {
		return nil, "", err
	}
	if _, err := s.st.DB.C(collSub).Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"secret": sealed, "secret_last4": last4(sec), "next_attempt_at": nil}}); err != nil {
		return nil, "", err
	}
	r, err := s.subscriber(ctx, id)
	if err != nil {
		return nil, "", err
	}
	head, err := s.Head(ctx, s.st.DB)
	return subscriberView(r, head), sec, err
}

// ---------- pull-feed keys ----------

func keyView(r store.Row) map[string]any {
	return map[string]any{"id": r["id"], "name": r["name"], "keyLast4": r["key_last4"], "active": r["active"] == true,
		"createdAt": timeOrNil(r["created_at"]), "createdBy": r["created_by"], "lastUsedAt": timeOrNil(r["last_used_at"])}
}

func (s *Service) feedKey(ctx context.Context, id string) (store.Row, error) {
	r, err := s.st.DB.C(collKey).FindOne(ctx, bson.M{"_id": id})
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, userErr(404, "API key not found")
	}
	return r, nil
}

// AddKey makes a pull-feed API key for a consumer and returns it (shown once; HRMS keeps only its hash).
func (s *Service) AddKey(ctx context.Context, by, name string) (map[string]any, string, error) {
	n, err := cleanName(&name, "key")
	if err != nil {
		return nil, "", err
	}
	if dup, err := s.st.DB.C(collKey).Exists(ctx, bson.M{"name": noCase(n)}); err != nil || dup {
		if err == nil {
			err = userErr(409, "A key named “%s” already exists", n)
		}
		return nil, "", err
	}
	k := newSecret(prefixFeedKey)
	id, err := s.st.DB.C(collKey).Insert(ctx, bson.M{"name": n, "key_hash": keyHash(k), "key_last4": last4(k), "created_by": nilIfEmpty(by)})
	if err != nil {
		return nil, "", dupName(err, "key", n)
	}
	r, err := s.feedKey(ctx, str(id))
	if err != nil {
		return nil, "", err
	}
	return keyView(r), k, nil
}

// SetKeyActive pauses or resumes a key.
func (s *Service) SetKeyActive(ctx context.Context, id string, active bool) (map[string]any, error) {
	if _, err := s.feedKey(ctx, id); err != nil {
		return nil, err
	}
	if _, err := s.st.DB.C(collKey).Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"active": active}}); err != nil {
		return nil, err
	}
	r, err := s.feedKey(ctx, id)
	if err != nil {
		return nil, err
	}
	return keyView(r), nil
}

// RotateKey replaces a key's value and returns the new one (shown once); the old value stops working at once.
func (s *Service) RotateKey(ctx context.Context, id string) (map[string]any, string, error) {
	if _, err := s.feedKey(ctx, id); err != nil {
		return nil, "", err
	}
	k := newSecret(prefixFeedKey)
	if _, err := s.st.DB.C(collKey).Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"key_hash": keyHash(k), "key_last4": last4(k)}}); err != nil {
		return nil, "", err
	}
	r, err := s.feedKey(ctx, id)
	if err != nil {
		return nil, "", err
	}
	return keyView(r), k, nil
}

// DeleteKey removes a key.
func (s *Service) DeleteKey(ctx context.Context, id string) error {
	n, err := s.st.DB.C(collKey).Delete(ctx, bson.M{"_id": id})
	if err == nil && n == 0 {
		err = userErr(404, "API key not found")
	}
	return err
}

// ---------- overview ----------

func eventView(r store.Row) map[string]any {
	d, _ := r["data"].(map[string]any)
	return map[string]any{"pos": i64(r["pos"]), "eventId": r["event_id"], "eventType": r["event_type"], "employeeCode": r["employee_code"],
		"name": d["name"], "sequence": i64(r["sequence"]), "timestamp": r["timestamp"]}
}

// RecentEvents lists the last n outbox events, newest first.
func (s *Service) RecentEvents(ctx context.Context, n int64) ([]map[string]any, error) {
	rows, err := s.st.DB.C(collOutbox).Find(ctx, nil, findSortLimit(bson.D{{Key: "pos", Value: -1}}, n))
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, eventView(r))
	}
	return out, nil
}

func runView(r store.Row) map[string]any {
	errs, _ := r["errors"].([]any)
	if errs == nil {
		errs = []any{}
	}
	return map[string]any{"id": r["id"], "at": timeOrNil(r["at"]), "finishedAt": timeOrNil(r["finished_at"]), "trigger": r["trigger"],
		"checked": i64(r["checked"]), "fixed": i64(r["fixed"]), "errors": errs,
		"employeesChecked": i64(r["employees_checked"]), "employeesFixed": i64(r["employees_fixed"]),
		"propertiesChecked": i64(r["properties_checked"]), "propertiesFixed": i64(r["properties_fixed"])}
}

// Runs lists the last n reconciliations, newest first.
func (s *Service) Runs(ctx context.Context, n int64) ([]map[string]any, error) {
	rows, err := s.st.DB.C(collRun).Find(ctx, nil, findSortLimit(bson.D{{Key: "at", Value: -1}, {Key: "_id", Value: -1}}, n))
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, runView(r))
	}
	return out, nil
}

func inboxView(r store.Row) map[string]any {
	return map[string]any{"eventId": r["event_id"], "eventType": r["event_type"], "schemaVersion": r["schema_version"], "propertyId": r["property_id"],
		"eventTime": timeOrNil(r["event_time"]), "status": r["status"], "reason": r["reason"], "receivedAt": timeOrNil(r["received_at"])}
}

// Overview is everything the Integrations card shows.
func (s *Service) Overview(ctx context.Context) (map[string]any, error) {
	db := s.st.DB
	set, err := s.settingRow(ctx, db)
	if err != nil {
		return nil, err
	}
	head, err := s.Head(ctx, db)
	if err != nil {
		return nil, err
	}
	subs, err := db.C(collSub).Find(ctx, nil, options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	sv := make([]map[string]any, 0, len(subs))
	for _, r := range subs {
		sv = append(sv, subscriberView(r, head))
	}
	keys, err := db.C(collKey).Find(ctx, nil, options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	kv := make([]map[string]any, 0, len(keys))
	for _, r := range keys {
		kv = append(kv, keyView(r))
	}
	events, err := s.RecentEvents(ctx, 10)
	if err != nil {
		return nil, err
	}
	runs, err := s.Runs(ctx, 5)
	if err != nil {
		return nil, err
	}
	inbox, err := db.C(collInbox).Find(ctx, nil, findSortLimit(bson.D{{Key: "received_at", Value: -1}}, 10))
	if err != nil {
		return nil, err
	}
	iv := make([]map[string]any, 0, len(inbox))
	for _, r := range inbox {
		iv = append(iv, inboxView(r))
	}
	published, err := db.C(collPublished).Count(ctx, nil)
	if err != nil {
		return nil, err
	}
	props, err := db.C(collProperty).Count(ctx, nil)
	if err != nil {
		return nil, err
	}
	linked, err := db.C("location").Count(ctx, bson.M{"property_id": bson.M{"$type": "string"}})
	if err != nil {
		return nil, err
	}
	unlinked, err := db.C("location").Count(ctx, bson.M{"property_id": nil, "status": "Active"})
	if err != nil {
		return nil, err
	}
	w, err := db.C(collState).FindOne(ctx, bson.M{"_id": stateWatcher})
	if err != nil {
		return nil, err
	}
	watcher := map[string]any{"running": false, "server": "", "lastSeenAt": nil, "resumable": false}
	if w != nil {
		exp, _ := w["expires_at"].(time.Time)
		watcher["running"] = !exp.IsZero() && exp.After(s.now()) && str(w["owner"]) != ""
		watcher["server"] = str(w["owner"])
		watcher["lastSeenAt"] = timeOrNil(w["updated_at"])
		watcher["resumable"] = str(w["token"]) != ""
	}
	return map[string]any{"settings": settingsView(set), "subscribers": sv, "keys": kv, "head": head, "published": published,
		"events": events, "runs": runs, "inbox": iv, "properties": props, "linkedLocations": linked, "unlinkedLocations": unlinked,
		"watcher": watcher, "eventTypes": EventTypes, "schemaVersion": SchemaVersion}, nil
}
