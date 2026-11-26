package store

// The Shortlisting stage in Form setup (owner, 26 Nov 2026): every field and document is asked at three stages,
// Shortlisting, Offer release and Joining, each Mandatory, Optional or Not needed.
//
//   - onb_field.shortlist_state and onb_document.shortlist_state: 0 Not needed, 1 Optional, 2 Mandatory on the
//     shortlist form (Onboarding › Shortlist), like offer_state and join_state. Null on a row written before the stage
//     existed (an import, or a row a later migration adds): the app then reads the default (ShortlistDefault), which
//     is what the shortlist form asked before: first and last name, mobile, personal email, date of birth, gender,
//     department, designation, work city and reporting location Mandatory, current address Optional, everything
//     else (and every document) Not needed.
//   - Locked (ShortlistFixed): the names, department, designation, work city and reporting location are always
//     Mandatory at shortlisting (the shortlist's approval route and the pipeline need them); the approval flow and
//     the work policy are worked out at the offer, so they are never asked at shortlisting.
//   - Values given at shortlisting are kept in candidate.data, documents with the candidate (document owner_kind
//     "candidate", owner_id SI-0001) like the offer's and joining's: they prefill the offer and the joining form.
//
// Migration 031 writes the default on every Form setup row that has none, with one audit_log row saying how many got
// which; Reset does the same after the reference data. Nothing else changes.

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The stage values (as offer_state and join_state).
const (
	StateNotNeeded = 0
	StateOptional  = 1
	StateMandatory = 2
)

// shortlistAlways are the fields the shortlist always asks, Mandatory and locked.
var shortlistAlways = map[string]bool{"first": true, "last": true, "dept": true, "desig": true, "city": true, "loc": true}

// shortlistBefore are the other fields the shortlist form asked before the stage existed, with their requirement.
var shortlistBefore = map[string]int{"phone": StateMandatory, "email": StateMandatory, "dob": StateMandatory, "gender": StateMandatory, "addr": StateOptional}

// ShortlistAlways: the shortlist always asks this field (Mandatory, locked).
func ShortlistAlways(key string) bool { return shortlistAlways[key] }

// ShortlistFixed: the field's Shortlisting cannot be changed in Form setup (ShortlistDefault is its value).
func ShortlistFixed(key, kind string) bool {
	return shortlistAlways[key] || kind == "flow" || kind == "policy"
}

// ShortlistDefault is a field's Shortlisting when Form setup has not said: what the shortlist form asked before.
func ShortlistDefault(key, kind string) int {
	switch {
	case shortlistAlways[key]:
		return StateMandatory
	case kind == "flow" || kind == "policy":
		return StateNotNeeded
	}
	return shortlistBefore[key]
}

var shortlistStateCheck = []Cond{{Col: "shortlist_state", Op: ">=", Val: float64(0)}, {Col: "shortlist_state", Op: "<=", Val: float64(2)}}

var _ = registerShortlist()

func registerShortlist() bool {
	for _, t := range schemaTables {
		if (t.Name == "onb_field" || t.Name == "onb_document") && t.cols == nil {
			t.Columns = append(t.Columns, Column{Name: "shortlist_state", Type: TInt2})
			t.Checks = append(t.Checks, Check{Name: t.Name + "_shortlist_state_check", Expr: "CHECK (((shortlist_state >= 0) AND (shortlist_state <= 2)))", Conds: shortlistStateCheck})
		}
	}
	migrations = append(migrations, migration{"031_shortlist_stage", false, func(ctx context.Context, s *Store) error {
		return s.Tx(ctx, func(tx Q) error {
			got, err := FillShortlistDefaults(ctx, tx)
			if err != nil || got.Total() == 0 {
				return err
			}
			_, err = tx.C("audit_log").Insert(ctx, bson.M{"actor": "System (migration 031)", "action": "Form setup: Shortlisting stage added. " + got.String(),
				"entity": "onb_field", "entity_id": "shortlist_state", "before": nil, "after": got.Map()})
			return err
		})
	}})
	resetHooks = append(resetHooks, func(ctx context.Context, tx Q) error { _, err := FillShortlistDefaults(ctx, tx); return err })
	return true
}

// ShortlistCounts are how many Form setup rows got each Shortlisting default: [0] Not needed, [1] Optional,
// [2] Mandatory.
type ShortlistCounts struct{ Fields, Docs [3]int }

// Total is the rows set.
func (c ShortlistCounts) Total() int {
	n := 0
	for i := range 3 {
		n += c.Fields[i] + c.Docs[i]
	}
	return n
}

// String reads "fields: 10 Mandatory, 1 Optional, 51 Not needed; documents: 15 Not needed".
func (c ShortlistCounts) String() string {
	part := func(l [3]int) string {
		s := ""
		for _, i := range []int{StateMandatory, StateOptional, StateNotNeeded} {
			if l[i] == 0 {
				continue
			}
			if s != "" {
				s += ", "
			}
			s += fmt.Sprintf("%d %s", l[i], map[int]string{0: "Not needed", 1: "Optional", 2: "Mandatory"}[i])
		}
		if s == "" {
			return "none"
		}
		return s
	}
	return "Fields: " + part(c.Fields) + "; documents: " + part(c.Docs)
}

// Map is the counts for an audit row.
func (c ShortlistCounts) Map() bson.M {
	m := func(l [3]int) bson.M { return bson.M{"mandatory": l[2], "optional": l[1], "notNeeded": l[0]} }
	return bson.M{"fields": m(c.Fields), "docs": m(c.Docs)}
}

// FillShortlistDefaults sets shortlist_state on every onb_field and onb_document row that has none: fields by
// ShortlistDefault, documents Not needed (the shortlist asked none). Rows already set are left alone; safe to run
// again. It returns how many rows got which value.
func FillShortlistDefaults(ctx context.Context, q Q) (ShortlistCounts, error) {
	var got ShortlistCounts
	missing := bson.M{"shortlist_state": nil}
	rows, err := q.C("onb_field").Find(ctx, missing)
	if err != nil {
		return got, err
	}
	for _, r := range rows {
		key, _ := r["key"].(string)
		kind, _ := r["kind"].(string)
		v := ShortlistDefault(key, kind)
		n, err := q.C("onb_field").Update(ctx, bson.M{"key": key, "shortlist_state": nil}, bson.M{"$set": bson.M{"shortlist_state": int32(v)}})
		if err != nil {
			return got, err
		}
		got.Fields[v] += int(n)
	}
	n, err := q.C("onb_document").UpdateMany(ctx, missing, bson.M{"$set": bson.M{"shortlist_state": int32(StateNotNeeded)}})
	got.Docs[StateNotNeeded] += int(n)
	return got, err
}
