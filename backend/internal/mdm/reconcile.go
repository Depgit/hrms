package mdm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// Nightly differential reconciliation (SOP §6), once a day after 02:00 business time (or on demand):
//
//   - as the source of truth for employee_code: every employee's snapshot is recomputed and compared with the last
//     one published; any difference (a change the watcher missed) is published now, through the same outbox;
//   - as a consumer of property_id: when a CMS address is set, GET {base}/properties (paged by next_cursor) is
//     compared with the local copy, and drift is fixed in mdm_property and in the linked locations.
//
// Each run is recorded in mdm_reconcile_run {at, checked, fixed, errors}.

// NightlyAfterHour is the business-time hour after which the nightly run may start.
const NightlyAfterHour = 2

// CMSProperty is one property in the CMS's /properties answer.
type CMSProperty struct {
	PropertyID string `json:"property_id"`
	Name       string `json:"name"`
	City       string `json:"city"`
	State      string `json:"state"`
	Status     string `json:"status"`
	UpdatedAt  string `json:"updated_at"`
}

// cmsPage is the CMS's /properties answer.
type cmsPage struct {
	Properties []CMSProperty `json:"properties"`
	NextCursor *string       `json:"next_cursor"`
}

// ReconcileIfDue runs the nightly reconciliation when it is due: after 02:00 business time and not yet run for
// today's business date. One server runs it (the day is claimed first). It reports whether it ran.
func (s *Service) ReconcileIfDue(ctx context.Context) (bool, error) {
	local := s.o.Now().In(store.Loc())
	if local.Hour() < NightlyAfterHour {
		return false, nil
	}
	day := local.Format("2006-01-02")
	if err := s.ensureState(ctx, stateReconcile); err != nil {
		return false, err
	}
	got, err := s.st.DB.C(collState).FindOneAndUpdate(ctx, bson.M{"_id": stateReconcile, "day": bson.M{"$ne": day}},
		bson.M{"$set": bson.M{"day": day, "updated_at": s.now()}})
	if err != nil || got == nil {
		return false, err
	}
	_, err = s.Reconcile(ctx, "nightly")
	return true, err
}

// Reconcile runs a reconciliation now and returns its record.
func (s *Service) Reconcile(ctx context.Context, trigger string) (map[string]any, error) {
	id, err := s.st.DB.C(collRun).Insert(ctx, bson.M{"trigger": trigger, "at": s.now()})
	if err != nil {
		return nil, err
	}
	var errs []string
	var empChecked, empFixed, propChecked, propFixed int64

	codes, err := s.AllEmployeeCodes(ctx)
	if err != nil {
		errs = append(errs, "Employees: "+err.Error())
	} else {
		empChecked = int64(len(codes))
		n, err := s.Publish(ctx, codes, nil)
		empFixed = int64(n)
		if err != nil {
			errs = append(errs, "Employees: "+err.Error())
		}
	}

	var problems []string
	propChecked, propFixed, problems = s.reconcileProperties(ctx)
	for _, e := range problems {
		errs = append(errs, "CMS: "+e)
	}

	if errs == nil {
		errs = []string{}
	}
	_, uerr := s.st.DB.C(collRun).Update(context.WithoutCancel(ctx), bson.M{"_id": id}, bson.M{"$set": bson.M{
		"finished_at": s.now(), "checked": empChecked + propChecked, "fixed": empFixed + propFixed, "errors": errs,
		"employees_checked": empChecked, "employees_fixed": empFixed, "properties_checked": propChecked, "properties_fixed": propFixed}})
	if uerr != nil {
		return nil, uerr
	}
	r, err := s.st.DB.C(collRun).FindOne(ctx, bson.M{"_id": id})
	if err != nil || r == nil {
		return nil, err
	}
	return runView(r), nil
}

// reconcileProperties compares the CMS's property list with the local copy and fixes the drift. It returns how
// many properties it checked and fixed, and the problems found.
func (s *Service) reconcileProperties(ctx context.Context) (int64, int64, []string) {
	set, err := s.settingRow(ctx, s.st.DB)
	if err != nil {
		return 0, 0, []string{err.Error()}
	}
	base := ""
	if set != nil {
		base = str(set["cms_base_url"])
	}
	if base == "" {
		return 0, 0, nil // no CMS address: HRMS only receives the CMS's events
	}
	key := ""
	if b := bytesOf(set["cms_api_key"]); len(b) > 0 {
		if key, err = s.unseal(aadCMSAPIKey, b); err != nil {
			return 0, 0, []string{"the CMS API key cannot be read (the app's secret key changed?); enter it again"}
		}
	}
	props, err := s.fetchCMSProperties(ctx, base, key)
	if err != nil {
		return 0, 0, []string{err.Error()}
	}
	var checked, fixed int64
	var problems []string
	seen := map[string]bool{}
	for _, cp := range props {
		p := Property{PropertyID: strings.TrimSpace(cp.PropertyID), Name: strings.TrimSpace(cp.Name), City: strings.TrimSpace(cp.City),
			State: strings.TrimSpace(cp.State), Status: strings.TrimSpace(cp.Status)}
		if p.PropertyID == "" || p.Name == "" || (p.Status != "Active" && p.Status != "Inactive") {
			problems = append(problems, fmt.Sprintf("property %q: property_id, name and status (Active or Inactive) are required", cp.PropertyID))
			continue
		}
		if seen[p.PropertyID] {
			problems = append(problems, fmt.Sprintf("property %s is listed twice", p.PropertyID))
			continue
		}
		seen[p.PropertyID] = true
		checked++
		var at time.Time
		if cp.UpdatedAt != "" {
			if at, err = time.Parse(time.RFC3339Nano, cp.UpdatedAt); err != nil {
				problems = append(problems, fmt.Sprintf("property %s: updated_at is not RFC 3339", p.PropertyID))
				continue
			}
		}
		changed, notes, err := s.reconcileProperty(ctx, p, at)
		if err != nil {
			problems = append(problems, fmt.Sprintf("property %s: %v", p.PropertyID, err))
			continue
		}
		problems = append(problems, notes...)
		if changed {
			fixed++
		}
	}
	// Properties HRMS knows that the CMS no longer lists: reported, not changed (the CMS deactivates, never deletes).
	local, err := s.st.DB.C(collProperty).Distinct(ctx, "property_id", nil)
	if err != nil {
		problems = append(problems, err.Error())
	}
	for _, v := range local {
		if !seen[str(v)] {
			problems = append(problems, fmt.Sprintf("property %s is in HRMS but not in the CMS's list", str(v)))
		}
	}
	return checked, fixed, problems
}

// reconcileProperty makes the local copy (and a linked location) agree with the CMS's state of one property.
func (s *Service) reconcileProperty(ctx context.Context, p Property, at time.Time) (bool, []string, error) {
	var changed bool
	var notes []string
	err := s.st.Tx(ctx, func(tx store.Q) error {
		changed, notes = false, nil
		if err := store.AdvisoryLock(ctx, tx, "mdm_property:"+p.PropertyID); err != nil {
			return err
		}
		cur, err := tx.C(collProperty).FindOne(ctx, bson.M{"property_id": p.PropertyID})
		if err != nil {
			return err
		}
		if cur != nil {
			if last, ok := cur["last_event_at"].(time.Time); ok && !at.IsZero() && last.After(at) {
				return nil // an event newer than the list was applied meanwhile
			}
		}
		same := cur != nil && str(cur["name"]) == p.Name && str(cur["city"]) == p.City && str(cur["state"]) == p.State && str(cur["status"]) == p.Status
		if !same {
			set := bson.M{"name": p.Name, "city": nilIfEmpty(p.City), "state": nilIfEmpty(p.State), "status": p.Status, "source": "reconcile", "updated_at": tx.Now()}
			if !at.IsZero() {
				set["last_event_at"] = at
			}
			if _, err := tx.C(collProperty).Upsert(ctx, bson.M{"property_id": p.PropertyID}, set, nil); err != nil {
				return err
			}
			changed = true
		}
		loc, err := tx.C("location").FindOne(ctx, bson.M{"property_id": p.PropertyID})
		if err != nil || loc == nil {
			return err
		}
		before := fmt.Sprint(loc["name"], loc["status"], loc["inactive_since"], loc["inactive_reason"])
		if notes, err = s.follow(ctx, tx, loc, p); err != nil {
			return err
		}
		after, err := tx.C("location").FindOne(ctx, bson.M{"_id": loc["id"]})
		if err != nil {
			return err
		}
		if after != nil && fmt.Sprint(after["name"], after["status"], after["inactive_since"], after["inactive_reason"]) != before {
			changed = true
		}
		return nil
	})
	return changed, notes, err
}

// fetchCMSProperties reads every page of GET {base}/properties.
func (s *Service) fetchCMSProperties(ctx context.Context, base, key string) ([]CMSProperty, error) {
	var out []CMSProperty
	cursor := ""
	for page := 0; page < 1000; page++ {
		u := strings.TrimRight(base, "/") + "/properties?limit=500"
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "Saltstayz-HRMS-MDM/1")
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		res, err := s.hc.Do(req)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", u, err)
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
		res.Body.Close()
		if err != nil {
			return nil, err
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: HTTP %d: %s", u, res.StatusCode, trimErr(string(body)))
		}
		var p cmsPage
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, fmt.Errorf("GET %s: the answer is not the expected JSON: %w", u, err)
		}
		out = append(out, p.Properties...)
		if p.NextCursor == nil || *p.NextCursor == "" {
			return out, nil
		}
		if *p.NextCursor == cursor {
			return nil, errors.New("the CMS returned the same next_cursor twice")
		}
		cursor = *p.NextCursor
	}
	return nil, errors.New("the CMS's property list has more than 1000 pages")
}
