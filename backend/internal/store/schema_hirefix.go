package store

// Registry additions of 18 Nov 2026 (owner's test sheet #22 and #26):
//
//   - employee.hire_no: the one number of the candidate series (candidate_seq) behind the person's Shortlisting,
//     Offer and Joining IDs (SI-0042, OI-0042, JI-0042). A hire through Onboarding has the candidate's number; an
//     employee made by Import employees, or one from before stages (no candidate record), takes the next number of
//     the same series. The Employee ID never changes.
//   - offer.letter_at ("joining" when the offer letter is held until joining, by the rule in force at release) and
//     offer.letter_released_at (when Mark joined released it).
//   - onb_setting.letter_join_depts / letter_join_types: departments and employment types whose offer letter is
//     released at joining instead of at offer release.
//
// Migration 018 fills hire_no for every employee without one: the candidate's number when they came through a
// candidate, else the next number of candidate_seq (in joining-date order), with a status-timeline entry. It only
// adds fields and timeline rows, and runs again safely (employees that have a hire_no are skipped).

import (
	"context"
	"fmt"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var _ = registerHireFix()

func registerHireFix() bool {
	for _, t := range schemaTables {
		if t.cols != nil {
			continue
		}
		switch t.Name {
		case "employee":
			t.Columns = append(t.Columns, Column{Name: "hire_no", Type: TInt4})
		case "offer":
			t.Columns = append(t.Columns, Column{Name: "letter_at", Type: TText}, Column{Name: "letter_released_at", Type: TTimestamptz})
		case "onb_setting":
			t.Columns = append(t.Columns, Column{Name: "letter_join_depts", Type: TText, Array: true}, Column{Name: "letter_join_types", Type: TText, Array: true})
		}
	}
	migrations = append(migrations, migration{"018_hire_ids", false, func(ctx context.Context, s *Store) error {
		_, err := BackfillHireNos(ctx, s.DB)
		return err
	}})
	sort.SliceStable(migrations, func(i, j int) bool { return migrations[i].name < migrations[j].name })
	return true
}

// HireIDs are the Shortlisting, Offer and Joining IDs of a hire number.
func HireIDs(no int) (si, oi, ji string) {
	return fmt.Sprintf("SI-%04d", no), fmt.Sprintf("OI-%04d", no), fmt.Sprintf("JI-%04d", no)
}

// unsetHireNo matches an employee without a hire number.
var unsetHireNo = bson.D{{Key: "$or", Value: bson.A{bson.D{{Key: "hire_no", Value: nil}}, bson.D{{Key: "hire_no", Value: bson.D{{Key: "$exists", Value: false}}}}}}}

// BackfillHireNos is migration 018 (see the file comment). It returns how many employees took a new number.
func BackfillHireNos(ctx context.Context, q Q) (int, error) {
	// 1. Hires through Onboarding: the candidate's own number.
	cands, err := q.C("candidate").Find(ctx, bson.D{{Key: "employee_id", Value: bson.D{{Key: "$ne", Value: nil}}}}, options.Find().SetProjection(bson.D{{Key: "no", Value: 1}, {Key: "employee_id", Value: 1}}))
	if err != nil {
		return 0, err
	}
	for _, c := range cands {
		no := rowInt(c["no"])
		emp, _ := c["employee_id"].(string)
		if no <= 0 || emp == "" {
			continue
		}
		if _, err := q.C("employee").Update(ctx, append(bson.D{{Key: "_id", Value: emp}}, unsetHireNo...), bson.D{{Key: "$set", Value: bson.D{{Key: "hire_no", Value: int32(no)}}}}); err != nil {
			return 0, err
		}
	}
	// 2. Everyone else (imported, or hired before stages): the next numbers of the candidate series.
	rest, err := q.C("employee").Find(ctx, unsetHireNo, options.Find().SetProjection(bson.D{{Key: "doj", Value: 1}}).SetSort(bson.D{{Key: "doj", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil || len(rest) == 0 {
		return 0, err
	}
	first, err := nextSeqBlock(ctx, q, "candidate_seq", int64(len(rest)))
	if err != nil {
		return 0, err
	}
	n := 0
	for i, e := range rest {
		id, _ := e["id"].(string)
		no := int(first) + i
		set, err := q.C("employee").Update(ctx, append(bson.D{{Key: "_id", Value: id}}, unsetHireNo...), bson.D{{Key: "$set", Value: bson.D{{Key: "hire_no", Value: int32(no)}}}})
		if err != nil {
			return n, err
		}
		if set == 0 {
			continue
		}
		doj, _ := e["doj"].(string)
		if err := HireIDsEvent(ctx, q, id, no, doj, "Given by migration 018: the employee was imported or joined before stages", "System", ""); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// HireIDsEvent records on the employee's status timeline the SI-/OI-/JI- IDs given to an employee who did not come
// through a candidate (dated the joining date).
func HireIDsEvent(ctx context.Context, q Q, emp string, no int, doj, reason, by, byID string) error {
	si, oi, ji := HireIDs(no)
	at := time.Now()
	if t, err := time.Parse("2006-01-02", doj); err == nil && t.Before(at) {
		at = t
	}
	var onDate, byIDv any
	if doj != "" {
		onDate = doj
	}
	if byID != "" {
		byIDv = byID
	}
	_, err := q.C(LifecycleEventColl).Insert(ctx, bson.D{{Key: "subject", Value: "employee"}, {Key: "employee_id", Value: emp},
		{Key: "action", Value: fmt.Sprintf("Hiring IDs: %s, %s, %s", si, oi, ji)}, {Key: "to_state", Value: ji}, {Key: "reason", Value: reason},
		{Key: "on_date", Value: onDate}, {Key: "by_name", Value: by}, {Key: "by_id", Value: byIDv}, {Key: "at", Value: at}})
	return err
}

func rowInt(v any) int {
	switch x := v.(type) {
	case int32:
		return int(x)
	case int64:
		return int(x)
	case int:
		return x
	case float64:
		return int(x)
	}
	return 0
}
