package api

// The all-inclusive gross (owner, 26 Nov 2026; the rule is calc/allin.go). From the month an admin picks
// (company_setting.allin_from, Compensation › Salary structure, pay.setup at Edit), the monthly gross G on the gross
// history and on offers also pays the employer's PF (with EPS), ESI, EPFO admin + EDLI and LWF, with the statutory
// bonus inside G when G is at most the bonus ceiling; variable pay is then paid every month (target × days paid ÷
// days) and HR enters no payouts for those months. Every payroll month from it on, for everyone; earlier months and
// approved or published payrolls never change: a change of the month that would alter one is refused.

import (
	"context"
	"net/http"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

func (s *Server) allInRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/comp/all-in", s.a(s.allInGet))
	m.HandleFunc("PUT /api/comp/all-in", s.a(s.allInPut))
}

// allInFrom is the first month of the all-inclusive gross ("" when off).
func allInFrom(ctx context.Context, q store.Q) string {
	set, _ := q.C("company_setting").FindOne(ctx, bson.M{"_id": 1}, options.FindOne().SetProjection(bson.M{"allin_from": 1}))
	if v := str(set["allin_from"]); isYm(v) {
		return v
	}
	return ""
}

// allInAt says whether month ym is paid on the all-inclusive gross, the rule starting in month from ("" = off).
func allInAt(from, ym string) bool { return from != "" && isYm(ym) && ym >= from }

// allInOn is allInAt with the setting read.
func allInOn(ctx context.Context, q store.Q, ym string) bool { return allInAt(allInFrom(ctx, q), ym) }

// allInPreview says whether a preview with no month of its own (the salary calculator, an offer without a joining
// date) uses the rule: whenever it is set, as every month from it on does.
func allInPreview(ctx context.Context, q store.Q) bool { return allInFrom(ctx, q) != "" }

// offerAllIn says whether an offer is priced on the all-inclusive gross: the joining month's rule, else (no joining
// date yet) whenever the rule is set.
func offerAllIn(ctx context.Context, q store.Q, j offerJob) bool {
	if isDate(j.DOJ) {
		return allInOn(ctx, q, j.DOJ[:7])
	}
	return allInPreview(ctx, q)
}

// allInView is the setting as the Compensation page shows it.
func allInView(ctx context.Context, q store.Q, c *Ctx) map[string]any {
	from := allInFrom(ctx, q)
	out := map[string]any{"from": from, "open": openYm(ctx, q), "can": c.Level("pay.setup") >= 2}
	if from != "" {
		out["label"] = ymLabel(from)
	}
	return out
}

// allInGet is the setting for any signed-in user: screens that take a gross say whether it is the all-inclusive G.
func (s *Server) allInGet(r *http.Request, c *Ctx) (any, error) {
	return allInView(r.Context(), s.st.DB, c), nil
}

// allInChangeRange is the months a change of the setting from old to next alters: [lo, hi) ("" hi: no end).
func allInChangeRange(old, next string) (lo, hi string) {
	switch {
	case old == "":
		return next, ""
	case next == "":
		return old, ""
	case old < next:
		return old, next
	}
	return next, old
}

func (s *Server) allInPut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.setup", 2); err != nil {
		return nil, err
	}
	var b struct {
		From string `json:"from"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	from := strings.TrimSpace(b.From)
	if from != "" && !isYm(from) {
		return nil, bad("Give the first month as YYYY-MM, or leave it empty to switch the all-inclusive gross off")
	}
	ctx := r.Context()
	err := s.st.Tx(ctx, func(tx store.Q) error {
		old := allInFrom(ctx, tx)
		if old == from {
			return nil
		}
		lo, hi := allInChangeRange(old, from)
		ym := bson.M{"$gte": lo}
		if hi != "" {
			ym["$lt"] = hi
		}
		paid, err := tx.C("payroll_month").FindOne(ctx, bson.M{"ym": ym, "status": bson.M{"$in": bson.A{"Approved", "Published"}}},
			options.FindOne().SetSort(bson.D{{Key: "ym", Value: 1}}))
		if err != nil {
			return err
		}
		if paid != nil {
			return bad("%s is already approved or published and this change would alter its pay. The all-inclusive gross can start from %s or later",
				ymLabel(str(paid["ym"])), ymLabel(openYm(ctx, tx)))
		}
		if _, err := tx.C("company_setting").Update(ctx, bson.M{"_id": 1}, bson.M{"$set": bson.M{"allin_from": nullIfEmpty(from)}}); err != nil {
			return err
		}
		action := "All-inclusive gross from " + ymLabel(from)
		if from == "" {
			action = "All-inclusive gross switched off"
		}
		return audit(ctx, tx, c, action, "company_setting", "1", map[string]any{"allinFrom": old}, map[string]any{"allinFrom": from})
	})
	if err != nil {
		return nil, err
	}
	return allInView(ctx, s.st.DB, c), nil
}

// allInVarRefusal is the message refusing a variable payout entry for month ym under the rule ("" when allowed).
func allInVarRefusal(ctx context.Context, q store.Q, ym string) string {
	if !allInOn(ctx, q, ym) {
		return ""
	}
	return "From " + ymLabel(allInFrom(ctx, q)) + " variable pay is paid automatically every month (target × days paid ÷ days, all-inclusive gross): no amounts are entered for " + ymLabel(ym)
}
