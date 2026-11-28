package api

// One mapping, one policy (owner, 24 Nov 2026): a mapping (a level and its value, a Department + place pair
// included) is applied to one work policy at a time. Mapping it again on another policy, switching on a switched-off
// copy while another policy has it on, or editing a mapping into one another policy has, is refused with "Already
// applied to <policy>. Switch it off there or edit that mapping." — never a silent move. The same mapping on the
// same policy is one mapping (nothing changes). Mappings of different levels that cover the same people (Department
// on one policy, Department + Location on another) are not duplicates: the order of the levels decides, and the
// conflict check (policy_conflict.go) asks HR first.
//
// Edit (PUT /api/policies/{id}/maps/{mapId}, preview at …/preview) changes a switched-on mapping's value and/or moves
// it to another policy: the old mapping is switched off (kept as history), the result is switched on on the target,
// both policies' histories say so and the change is audited. The duplicate rule applies to the result and the
// people-moving conflict check runs as on Add.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// mapOwner is a policy (other than the mapping row skip on policy skipPol) with level|ref switched on, or nil.
// Inactive policies count: their mappings come back when the policy is made active again.
func (t *timeSet) mapOwner(level, ref, skipPol string, skipRow int64) *workPolicy {
	for _, p := range t.all {
		for _, m := range p.Maps {
			if m.On && m.Level == level && m.Ref == ref && !(p.ID == skipPol && m.ID == skipRow) {
				return p
			}
		}
	}
	return nil
}

// dupRefusal is the error of a mapping that another policy already has.
func dupRefusal(p *workPolicy) error {
	return &apiError{Status: 409, Message: fmt.Sprintf("Already applied to %s. Switch it off there or edit that mapping.", p.Name),
		Details: map[string]any{"duplicate": true, "policyId": p.ID, "policy": p.Name}}
}

// mapDupCheck refuses a save of policy id's mappings that switches on (or adds) a mapping another policy has on.
func (t *timeSet) mapDupCheck(id string, maps []polMap) error {
	var target *workPolicy
	for _, p := range t.all {
		if p.ID == id {
			target = p
		}
	}
	if target == nil {
		return notFound("Policy not found")
	}
	on := map[string]bool{}
	for _, m := range target.Maps {
		if m.On {
			on[m.Level+"|"+m.Ref] = true
		}
	}
	keys := []polMap{}
	seen := map[string]bool{}
	for _, m := range maps {
		k := m.Level + "|" + m.Ref
		if m.On && !on[k] && !seen[k] {
			seen[k] = true
			keys = append(keys, m)
		}
	}
	sort.SliceStable(keys, func(i, j int) bool { return keys[i].Level+keys[i].Ref < keys[j].Level+keys[j].Ref })
	for _, m := range keys {
		if p := t.mapOwner(m.Level, m.Ref, id, 0); p != nil && p.ID != id {
			return dupRefusal(p)
		}
	}
	return nil
}

type mapEditBody struct {
	Ref        string            `json:"ref"`      // the new value (blank keeps it)
	PolicyID   string            `json:"policyId"` // the policy it ends on (blank keeps it)
	Resolution *policyResolution `json:"resolution"`
}

// mapEdit is a planned edit of one mapping.
type mapEdit struct {
	rowID                 int64
	level, oldRef, newRef string
	from, to              *workPolicy
	exception             bool
	note                  string
	same                  bool // nothing changes
	cf                    *conflictResult
}

// planMapEdit checks an edit of mapping mapID of policy id and works out the people it moves.
func planMapEdit(ctx context.Context, q store.Q, ts *timeSet, id, mapID string, b mapEditBody) (*mapEdit, error) {
	mid, ok := policiesSerial(mapID)
	if !ok {
		return nil, notFound("Mapping not found")
	}
	row, err := q.C("work_policy_map").FindOne(ctx, bson.M{"_id": mid, "policy_id": id})
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, notFound("Mapping not found")
	}
	if row["on_map"] != true {
		return nil, bad("This mapping is switched off. Switch it on, or add a new mapping")
	}
	e := &mapEdit{rowID: mid, level: str(row["level"]), oldRef: str(row["ref"]), exception: row["exception"] == true, note: str(row["note"])}
	target := strings.TrimSpace(b.PolicyID)
	if target == "" {
		target = id
	}
	for _, p := range ts.all {
		if p.ID == id {
			e.from = p
		}
		if p.ID == target {
			e.to = p
		}
	}
	if e.from == nil || e.to == nil {
		return nil, notFound("Work policy not found")
	}
	if e.newRef = strings.TrimSpace(b.Ref); e.newRef == "" {
		e.newRef = e.oldRef
	}
	if e.level == "employee" && e.newRef != e.oldRef {
		return nil, bad("A mapping for one employee can only be moved to another policy")
	}
	if !policyRefExists(ctx, q, e.level, e.newRef) {
		if _, ok := comboPlace[e.level]; ok {
			return nil, bad("%s: pick a department and a %s that exist", levelNames[e.level], strings.ToLower(levelNames[comboPlace[e.level]]))
		}
		return nil, bad("%s “%s” does not exist", levelNames[e.level], e.newRef)
	}
	if e.newRef == e.oldRef && e.to.ID == e.from.ID {
		e.same = true
		e.cf = &conflictResult{Conflicts: []conflictGroup{}, Moves: []conflictGroup{}, Policy: e.to.Name, ApplyFrom: today()}
		return e, nil
	}
	// The result may not be on anywhere else (the mapping being edited aside).
	if p := ts.mapOwner(e.level, e.newRef, e.from.ID, e.rowID); p != nil {
		return nil, dupRefusal(p)
	}
	if e.cf, err = policyConflicts(ctx, q, ts, e.to.ID, []polMap{{Level: e.level, Ref: e.newRef, On: true}}); err != nil {
		return nil, err
	}
	return e, nil
}

// policyMapEditPreview: POST /api/policies/{id}/maps/{mapId}/preview {ref, policyId}: what the edit would do.
func (s *Server) policyMapEditPreview(r *http.Request, c *Ctx) (any, error) {
	if err := canPolicies(c, true); err != nil {
		return nil, err
	}
	var b mapEditBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	e, err := planMapEdit(ctx, s.st.DB, ts, r.PathValue("id"), r.PathValue("mapId"), b)
	if err != nil {
		return nil, err
	}
	return struct {
		*conflictResult
		From      string `json:"from"`
		To        string `json:"to"`
		Move      bool   `json:"move"`
		Unchanged bool   `json:"unchanged"`
	}{e.cf, e.from.Name, e.to.Name, e.from.ID != e.to.ID, e.same}, nil
}

// policyMapEdit: PUT /api/policies/{id}/maps/{mapId} {ref, policyId, resolution}: changes a mapping's value and/or
// moves it to another policy. Refused (409) while people it moves already get another policy and HR has not chosen.
func (s *Server) policyMapEdit(r *http.Request, c *Ctx) (any, error) {
	if err := canPolicies(c, true); err != nil {
		return nil, err
	}
	var b mapEditBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		ts, err := loadTimeSet(ctx, tx)
		if err != nil {
			return err
		}
		e, err := planMapEdit(ctx, tx, ts, id, r.PathValue("mapId"), b)
		if err != nil || e.same {
			return err
		}
		if len(e.cf.Conflicts) > 0 && b.Resolution == nil {
			return conflictRefusal(e.cf)
		}
		wm := tx.C("work_policy_map")
		if _, err := wm.Update(ctx, bson.M{"_id": e.rowID}, bson.M{"$set": bson.M{"on_map": false}}); err != nil {
			return err
		}
		set := bson.M{"on_map": true}
		if e.exception {
			set["exception"], set["note"] = true, e.note
		}
		x, err := wm.FindOne(ctx, bson.M{"policy_id": e.to.ID, "level": e.level, "ref": e.newRef})
		if err != nil {
			return err
		}
		if x != nil {
			if _, err := wm.Update(ctx, bson.M{"_id": x["id"]}, bson.M{"$set": set}); err != nil {
				return err
			}
		} else {
			doc := bson.M{"policy_id": e.to.ID, "level": e.level, "ref": e.newRef, "created_by": c.Name}
			for k, v := range set {
				if k != "on_map" {
					doc[k] = v
				}
			}
			if _, err := wm.Insert(ctx, doc); err != nil {
				return err
			}
		}
		names := newRefNames(ctx, tx)
		oldL, newL := names.label(polMap{Level: e.level, Ref: e.oldRef}), names.label(polMap{Level: e.level, Ref: e.newRef})
		hist := map[string][]map[string]any{}
		hist[e.from.ID] = append(hist[e.from.ID], map[string]any{"field": "applies to " + e.level + "|" + e.oldRef, "from": "on", "to": "off",
			"note": fmt.Sprintf("Edited: now %s on %s", newL, e.to.Name)})
		hist[e.to.ID] = append(hist[e.to.ID], map[string]any{"field": "applies to " + e.level + "|" + e.newRef, "from": "off", "to": "on",
			"note": fmt.Sprintf("Edited from %s on %s", oldL, e.from.Name)})
		if e.from.ID != e.to.ID {
			mv := map[string]any{"field": "mapping moved", "from": e.from.Name, "to": e.to.Name, "note": newL}
			hist[e.from.ID] = append(hist[e.from.ID], mv)
			hist[e.to.ID] = append(hist[e.to.ID], mv)
		}
		if len(e.cf.Conflicts) > 0 { // HR's choice, as employee-level exceptions
			byPol, err := applyResolution(ctx, tx, c, e.to.ID, e.cf, b.Resolution)
			if err != nil {
				return err
			}
			for pid, ch := range byPol {
				hist[pid] = append(hist[pid], ch...)
			}
			if err := audit(ctx, tx, c, "Policy conflict resolved: "+b.Resolution.Mode, "work_policy", e.to.ID, e.cf.Conflicts, b.Resolution); err != nil {
				return err
			}
		}
		pids := []string{}
		for pid := range hist {
			pids = append(pids, pid)
		}
		sort.Strings(pids)
		for _, pid := range pids {
			if err := configHistory(ctx, tx, c, "work_policy", pid, hist[pid]); err != nil {
				return err
			}
		}
		return audit(ctx, tx, c, "Work policy mapping edited", "work_policy", e.to.ID,
			map[string]any{"policy": e.from.Name, "level": e.level, "ref": e.oldRef, "mapping": oldL},
			map[string]any{"policy": e.to.Name, "level": e.level, "ref": e.newRef, "mapping": newL})
	})
}
