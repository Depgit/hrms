package store

// Variable pay as a target (decided 17 Nov 2026): every salary structure, Version 1 included, pays variable pay as a
// monthly target per person that HR pays by entering the amount when a payout is due; nothing is paid automatically.
// The target is entered either as a % of the fixed monthly gross or as ₹ a month (var_target.mode "pct" or "amount").
//
// Migration 017 converts each percentage structure in place (same id, so structure_map is untouched): rates.comps
// becomes calc.TargetComponents (the same fixed components, the variable a manual "Variable pay" component on the
// structure's payout cycle), rates.designed and rates.converted are set and rates.varPct becomes 0. Nothing else in the
// rates changes, so fixed pay (Basic, HRA, Special allowance, PF, ESI and the rest) is unchanged to the paisa
// (`hrms pay-compare -dump` before and `-against` after proves it on a copy). Each person with a variable % (their own,
// else the structure's standard) gets a var_target row in mode "pct" with the same %, from the month it applied
// (joining, or the month it started or changed), and employee_pay.var_pct / var_cycle are cleared; employee_change
// records the move.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/calc"
)

var _ = registerVarTarget()

func registerVarTarget() bool {
	varTargetTable.Columns = append(varTargetTable.Columns,
		Column{Name: "mode", Type: TText},                      // "pct" or "amount" (null: amount)
		Column{Name: "pct", Type: TNumeric, Prec: 5, Scale: 2}, // mode pct: % of the fixed monthly gross
	)
	for i := range varTargetTable.Columns {
		if varTargetTable.cols != nil {
			varTargetTable.cols[varTargetTable.Columns[i].Name] = &varTargetTable.Columns[i]
		}
	}
	migrations = append(migrations, migration{"017_variable_target", false, func(ctx context.Context, s *Store) error {
		return s.Tx(ctx, func(tx Q) error { _, _, err := ConvertToVarTargets(ctx, tx); return err })
	}})
	return true
}

// VarTargetMove is one person's variable % moved to a target by migration 017.
type VarTargetMove struct {
	EmployeeID string
	From       string
	Pct        float64
}

// ConvertToVarTargets is migration 017 (see the file comment): it returns the structures converted and the targets
// written. Safe to run again: a structure already designed is skipped, and so are its people.
func ConvertToVarTargets(ctx context.Context, q Q) (int, []VarTargetMove, error) {
	list, err := q.C("salary_structure").Find(ctx, bson.M{})
	if err != nil {
		return 0, nil, err
	}
	conv := map[string]calc.Structure{} // converted structure id → its rates before
	def := ""
	for _, x := range list {
		var st calc.Structure
		if err := payJSON(x["rates"], &st); err != nil {
			return 0, nil, err
		}
		if x["is_default"] == true {
			def = fmt.Sprint(x["id"])
		}
		if st.Designed {
			continue
		}
		comps, err := toDocs(calc.TargetComponents(st))
		if err != nil {
			return 0, nil, err
		}
		if _, err := q.C("salary_structure").Update(ctx, bson.M{"_id": x["id"]}, bson.M{"$set": bson.M{"rates.comps": comps, "rates.designed": true,
			"rates.converted": true, "rates.varPct": 0.0}}); err != nil {
			return 0, nil, err
		}
		conv[fmt.Sprint(x["id"])] = st
	}
	if len(conv) == 0 {
		return 0, nil, nil
	}
	today := time.Now().Format("2006-01-02")
	emps, err := q.C("employee").Find(ctx, bson.M{"status": bson.M{"$ne": "Inactive"}})
	if err != nil {
		return 0, nil, err
	}
	var moves []VarTargetMove
	for _, e := range emps {
		id, doj := fmt.Sprint(e["id"]), fmt.Sprint(e["doj"])
		if len(doj) < 7 {
			continue
		}
		// The structure the person is on today: the latest mapping that has started, else the default.
		sid := def
		if m, _ := q.C("structure_map").FindOne(ctx, bson.M{"employee_id": id, "from_date": bson.M{"$lte": today}},
			options.FindOne().SetSort(bson.D{{Key: "from_date", Value: -1}})); m != nil {
			sid = fmt.Sprint(m["structure_id"])
		}
		st, ok := conv[sid]
		if !ok {
			continue
		}
		pay, _ := q.C("employee_pay").FindOne(ctx, bson.M{"employee_id": id})
		num := func(k string) *float64 {
			if pay == nil {
				return nil
			}
			switch v := pay[k].(type) {
			case float64:
				return &v
			case int32:
				f := float64(v)
				return &f
			case int64:
				f := float64(v)
				return &f
			}
			return nil
		}
		pct := 0.0
		if p := num("var_pct"); p != nil {
			pct = *p
		} else if calc.RulesFor(calc.EmploymentType(fmt.Sprint(e["type"]))).Variable {
			pct = st.VarPct
		}
		if pct <= 0 {
			continue
		}
		from := doj[:7]
		str := func(k string) string {
			if pay == nil || pay[k] == nil {
				return ""
			}
			return fmt.Sprint(pay[k])
		}
		if s := str("var_start"); len(s) >= 7 && s[:7] > from {
			from = s[:7]
		}
		type pt struct {
			from string
			pct  float64
		}
		pts := []pt{{from, pct}}
		if vf := str("var_from"); len(vf) >= 7 && vf[:7] > from {
			if prev := num("var_prev_pct"); prev != nil && *prev > 0 {
				pts = []pt{{from, *prev}, {vf[:7], pct}}
			} else {
				pts = []pt{{vf[:7], pct}}
			}
		}
		was := strconv.FormatFloat(pct, 'f', -1, 64) + "% of gross, paid automatically " + cycleText(st.VarCycle, num("var_cycle"))
		for _, p := range pts {
			total := 0.0
			if g, _ := q.C("gross_history").FindOne(ctx, bson.M{"employee_id": id, "from_month": bson.M{"$lte": p.from}},
				options.FindOne().SetSort(bson.D{{Key: "from_month", Value: -1}})); g != nil {
				if f, ok := g["gross"].(float64); ok {
					total = calc.Round2(f * p.pct / 100)
				}
			}
			if _, err := q.C(VarTargetColl).Upsert(ctx, bson.M{"employee_id": id, "from_month": p.from}, bson.M{"mode": "pct", "pct": p.pct, "total": total,
				"by_comp": nil, "reason": "Variable pay % moved to a target (migration 017)", "created_by": "System"}, nil); err != nil {
				return 0, nil, err
			}
			moves = append(moves, VarTargetMove{EmployeeID: id, From: p.from, Pct: p.pct})
		}
		if pay != nil {
			if _, err := q.C("employee_pay").Update(ctx, bson.M{"employee_id": id}, bson.M{"$set": bson.M{"var_pct": nil, "var_cycle": nil}}); err != nil {
				return 0, nil, err
			}
		}
		pp := pts[len(pts)-1]
		if _, err := q.C("employee_change").Insert(ctx, bson.M{"employee_id": id, "field": "Variable pay target (a month)", "old_value": was,
			"new_value": strconv.FormatFloat(pp.pct, 'f', -1, 64) + "% of fixed gross, paid as HR enters it when due", "reason": "Variable pay as a target (17 Nov 2026)",
			"effective_from": pp.from + "-01", "source": "Migration 017", "changed_by": "System"}); err != nil {
			return 0, nil, err
		}
	}
	return len(conv), moves, nil
}

func cycleText(std int, own *float64) string {
	c := std
	if own != nil {
		c = int(*own)
	}
	switch c {
	case 3, 6, 12:
		return "every " + strconv.Itoa(c) + " months"
	}
	return "every month"
}

// payJSON decodes stored rates (maps from the jsonb) into v.
func payJSON(src any, v any) error {
	b, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// HealVarTargets converts any structure still on percentage rates (one seeded after the migrations ran, on a new
// installation) to the variable target model, at every start, so HR can always give a target in % or ₹.
func (s *Store) HealVarTargets(ctx context.Context) (int, error) {
	n := 0
	err := s.Tx(ctx, func(tx Q) error {
		var err error
		n, _, err = ConvertToVarTargets(ctx, tx)
		return err
	})
	return n, err
}
