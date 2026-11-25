package api

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

// Compensation (spec 13): one standard salary structure with versions, mapped to people from
// a date; pay bands per city and grade; labour welfare fund by state; budgets per property.

// ---------- small helpers shared by the compensation, tax and statutory files ----------

// compUUIDKey is the uuid as the stored column holds it (what `$1::uuid` made of it: braces and hyphens optional,
// any case), so an INSERT … ON CONFLICT key written as an Upsert filter finds the existing row. A value that is not
// a uuid is returned unchanged, and the insert path rejects it as before.
func compUUIDKey(s string) string {
	x := strings.TrimSpace(s)
	if strings.HasPrefix(x, "{") && strings.HasSuffix(x, "}") {
		x = x[1 : len(x)-1]
	}
	x = strings.ToLower(strings.ReplaceAll(x, "-", ""))
	if len(x) != 32 || strings.Trim(x, "0123456789abcdef") != "" {
		return s
	}
	return x[0:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
}

// compOrder is a sort order for Find and $sort: field names in order, "-name" for descending.
func compOrder(fields ...string) bson.D {
	d := make(bson.D, 0, len(fields))
	for _, f := range fields {
		dir := 1
		if strings.HasPrefix(f, "-") {
			f, dir = f[1:], -1
		}
		d = append(d, bson.E{Key: f, Value: dir})
	}
	return d
}

// compFields is a projection of exactly these columns (no id unless named), like a SELECT list.
func compFields(cols ...string) bson.D {
	d := bson.D{}
	hasID := false
	for _, c := range cols {
		if c == "id" || c == "_id" {
			hasID = true
		}
		d = append(d, bson.E{Key: c, Value: 1})
	}
	if !hasID {
		d = append(bson.D{{Key: "_id", Value: 0}}, d...)
	}
	return d
}

// compJSONInto decodes a JSON value as the store returns it (a map or a slice) into out with encoding/json and the
// json tags, over what out already holds (fields the stored value lacks keep theirs).
func compJSONInto(v any, out any) error {
	if v == nil {
		return errors.New("jsonb value is null")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// compDay is to_char(ts,'DD Mon YYYY') in the business zone (nil for NULL).
func compDay(v any) any {
	if t, ok := v.(time.Time); ok {
		return t.In(store.Loc()).Format("02 Jan 2006")
	}
	return nil
}

// compStrs turns a text[] read from the store into a []string (nil for NULL).
func compStrs(v any) []string {
	l, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(l))
	for _, x := range l {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// compRowsByID keys rows by their id.
func compRowsByID(rows []store.Row) map[string]store.Row {
	m := make(map[string]store.Row, len(rows))
	for _, r := range rows {
		m[str(r["id"])] = r
	}
	return m
}

// compNonNil is a slice that is never nil, so a $in filter always gets an array (a nil list matches nothing).
func compNonNil(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

// compRound2 rounds a sum of numeric(…,2) amounts back to cents.
func compRound2(f float64) float64 { return math.Round(f*100) / 100 }

// compIntID reads a numeric id given in a URL: only its plain decimal form names a record.
func compIntID(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || strconv.FormatInt(n, 10) != s {
		return 0, false
	}
	return n, true
}

// ---------- the Compensation page ----------

func (s *Server) compGet(r *http.Request, c *Ctx) (any, error) {
	if c.Level("comp.structure") < 1 && c.Level("comp.bands") < 1 && c.Level("comp.budget") < 1 && c.Level("pay.setup") < 1 {
		return nil, forbidden("")
	}
	ctx := r.Context()
	out := map[string]any{"types": calc.TypeRulesNow(),
		"can": map[string]bool{"structure": c.Level("comp.structure") >= 2, "bands": c.Level("comp.bands") >= 2, "budget": c.Level("comp.budget") >= 2, "lwf": c.Level("pay.setup") >= 2}}
	if c.Level("comp.structure") >= 1 || c.Level("pay.setup") >= 1 {
		st, err := compStructures(ctx, s.st.DB)
		if err != nil {
			return nil, err
		}
		out["structures"] = st
		out["lwf"], _ = compLwfLatest(ctx, s.st.DB)
		// Active leave type names for the "Leave" rule by employment type (names only, so no leave-plan right is needed).
		names := []string{}
		if rows, err := s.st.DB.C("leave_type").Find(ctx, bson.M{"status": "Active"}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}, {Key: "name", Value: 1}}).SetProjection(bson.M{"name": 1})); err == nil {
			for _, x := range rows {
				if n, ok := x["name"].(string); ok {
					names = append(names, n)
				}
			}
		}
		out["leaveTypes"] = names
	}
	out["allIn"] = allInView(ctx, s.st.DB, c) // the all-inclusive gross from a month (allin.go)
	if c.Level("comp.bands") >= 1 {
		out["bands"], _ = compBands(ctx, s.st.DB)
	}
	if c.Level("comp.budget") >= 1 {
		// HRBP sees only the budgets of mapped properties (spec 13).
		out["budgets"], _ = compBudgets(ctx, s.st.DB, c)
		if c.Scope.Access == "all" {
			out["cityBudgets"], _ = compCityBudgets(ctx, s.st.DB)
		}
	}
	return out, nil
}

// compStructures lists the versions oldest first, with how many people were ever mapped to each and how many
// active employees are on it today.
func compStructures(ctx context.Context, q store.Q) ([]store.Row, error) {
	list, err := q.C("salary_structure").Find(ctx, bson.M{}, options.Find().SetSort(compOrder("created_at", "name")))
	if err != nil {
		return nil, err
	}
	mapped := map[string]int64{}
	counts, err := q.C("structure_map").Aggregate(ctx, bson.A{
		bson.M{"$group": bson.M{"_id": bson.M{"s": "$structure_id", "e": "$employee_id"}}},
		bson.M{"$group": bson.M{"_id": "$_id.s", "n": bson.M{"$sum": int64(1)}}},
	})
	if err != nil {
		return nil, err
	}
	for _, x := range counts {
		mapped[str(x["id"])], _ = x["n"].(int64)
	}
	active, err := activeOnVersion(ctx, q, today())
	if err != nil {
		return nil, err
	}
	out := make([]store.Row, 0, len(list))
	for _, x := range list {
		id := str(x["id"])
		out = append(out, store.Row{"id": x["id"], "name": x["name"], "rates": x["rates"], "is_default": x["is_default"], "status": x["status"],
			"created": compDay(x["created_at"]), "mapped": mapped[id], "active": active[id]})
	}
	return out, nil
}

// compLwfLatest is each active state's latest labour welfare fund rule (rule and date null when none).
func compLwfLatest(ctx context.Context, q store.Q) ([]store.Row, error) {
	states, err := q.C("state").Find(ctx, bson.M{"status": "Active"}, options.Find().SetSort(compOrder("name", "_id")))
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(states))
	for i, st := range states {
		ids[i] = str(st["id"])
	}
	rules, err := q.C("lwf_rule").Find(ctx, bson.M{"state_id": bson.M{"$in": ids}}, options.Find().SetSort(compOrder("state_id", "-effective_from")))
	if err != nil {
		return nil, err
	}
	// The rule in force today, and the next one scheduled after it (SET-076): a rule saved from a future date does
	// not hide the one deducted now.
	now := today()
	latest, next := map[string]store.Row{}, map[string]store.Row{}
	for _, w := range rules { // newest first per state
		sid := str(w["state_id"])
		if str(w["effective_from"]) > now {
			next[sid] = w // ends as the earliest future rule
			continue
		}
		if _, seen := latest[sid]; !seen {
			latest[sid] = w
		}
	}
	out := make([]store.Row, 0, len(states))
	for _, st := range states {
		row := store.Row{"state_id": st["id"], "state": st["name"], "rule": nil, "effective_from": nil, "next_rule": nil, "next_from": nil}
		if w := latest[str(st["id"])]; w != nil {
			row["rule"], row["effective_from"] = w["rule"], w["effective_from"]
		}
		if w := next[str(st["id"])]; w != nil {
			row["next_rule"], row["next_from"] = w["rule"], w["effective_from"]
		}
		out = append(out, row)
	}
	return out, nil
}

// compBands is every active city × active grade with its band (min and max null when not set).
func compBands(ctx context.Context, q store.Q) ([]store.Row, error) {
	cities, err := q.C("city").Find(ctx, bson.M{"status": "Active"}, options.Find().SetSort(compOrder("name", "_id")))
	if err != nil {
		return nil, err
	}
	grades, err := q.C("grade").Find(ctx, bson.M{"status": "Active"}, options.Find().SetSort(compOrder("name")))
	if err != nil {
		return nil, err
	}
	bands, err := q.C("pay_band").Find(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	band := map[string]store.Row{}
	for _, b := range bands {
		band[str(b["city_id"])+"|"+str(b["grade"])] = b
	}
	out := make([]store.Row, 0, len(cities)*len(grades))
	for _, ct := range cities {
		for _, g := range grades {
			row := store.Row{"city_id": ct["id"], "city": ct["name"], "grade": g["name"], "min": nil, "max": nil}
			if b := band[str(ct["id"])+"|"+str(g["name"])]; b != nil {
				row["min"], row["max"] = b["min_gross"], b["max_gross"]
			}
			out = append(out, row)
		}
	}
	// By city name, then grade: cities with the same name (in different states) interleave by grade.
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := str(out[i]["city"]), str(out[j]["city"]); a != b {
			return a < b
		}
		return str(out[i]["grade"]) < str(out[j]["grade"])
	})
	return out, nil
}

// compPlaces maps every location to its city (location → cluster → city), and names the cities.
func compPlaces(ctx context.Context, q store.Q) (locCity map[string]string, cityName map[string]any, err error) {
	clusters, err := q.C("cluster").Find(ctx, bson.M{}, options.Find().SetProjection(compFields("id", "city_id")))
	if err != nil {
		return nil, nil, err
	}
	clusterCity := map[string]string{}
	for _, k := range clusters {
		clusterCity[str(k["id"])] = str(k["city_id"])
	}
	locs, err := q.C("location").Find(ctx, bson.M{}, options.Find().SetProjection(compFields("id", "cluster_id")))
	if err != nil {
		return nil, nil, err
	}
	locCity = map[string]string{}
	for _, l := range locs {
		if ct, ok := clusterCity[str(l["cluster_id"])]; ok {
			locCity[str(l["id"])] = ct
		}
	}
	cities, err := q.C("city").Find(ctx, bson.M{}, options.Find().SetProjection(compFields("id", "name")))
	if err != nil {
		return nil, nil, err
	}
	cityName = map[string]any{}
	for _, ct := range cities {
		cityName[str(ct["id"])] = ct["name"]
	}
	return locCity, cityName, nil
}

// compHeadcounts counts the employees who are not inactive, per location.
func compHeadcounts(ctx context.Context, q store.Q) (map[string]int64, error) {
	rows, err := q.C("employee").Aggregate(ctx, bson.A{
		bson.M{"$match": bson.M{"status": bson.M{"$ne": "Inactive"}}},
		bson.M{"$group": bson.M{"_id": "$location_id", "n": bson.M{"$sum": int64(1)}}},
	})
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, x := range rows {
		out[str(x["id"])], _ = x["n"].(int64)
	}
	return out, nil
}

// compBudgets is each active location's budget beside its headcount, offers waiting for approval and the
// total monthly cost to the company of its active employees (budget_cost.go locMonthlyCost).
func compBudgets(ctx context.Context, q store.Q, c *Ctx) ([]store.Row, error) {
	filter := bson.M{"status": "Active"}
	if c.Scope.Access == "locs" {
		filter["_id"] = bson.M{"$in": compNonNil(c.Scope.Locations)}
	}
	locs, err := q.C("location").Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	locCity, cityName, err := compPlaces(ctx, q)
	if err != nil {
		return nil, err
	}
	heads, err := compHeadcounts(ctx, q)
	if err != nil {
		return nil, err
	}
	// Offers whose approval request is still pending, per location.
	offers, err := q.C("offer").Find(ctx, bson.M{"request_id": bson.M{"$ne": nil}}, options.Find().SetProjection(compFields("location_id", "request_id")))
	if err != nil {
		return nil, err
	}
	reqIDs := make([]string, 0, len(offers))
	for _, o := range offers {
		reqIDs = append(reqIDs, str(o["request_id"]))
	}
	pendingReq, err := q.C("approval_request").Distinct(ctx, "_id", bson.M{"_id": bson.M{"$in": reqIDs}, "status": "Pending"})
	if err != nil {
		return nil, err
	}
	isPending := map[string]bool{}
	for _, id := range pendingReq {
		isPending[str(id)] = true
	}
	pending := map[string]int64{}
	for _, o := range offers {
		if isPending[str(o["request_id"])] {
			pending[str(o["location_id"])]++
		}
	}
	// Cost: the total monthly cost to the company of the active employees (salary without the minimum-wage top-ups,
	// plus the employer's contributions), as Cost and budget projects it (budget_cost.go).
	cost, err := locMonthlyCost(ctx, q, nil)
	if err != nil {
		return nil, err
	}
	out := make([]store.Row, 0, len(locs))
	for _, l := range locs {
		id := str(l["id"])
		cid, inCity := locCity[id]
		if _, named := cityName[cid]; !inCity || !named { // a location outside a cluster or city is left out
			continue
		}
		out = append(out, store.Row{"id": l["id"], "name": l["name"], "city": cityName[cid], "headcount_budget": l["headcount_budget"], "payroll_budget": l["payroll_budget"],
			"headcount": heads[id], "pending_offers": pending[id], "cost": compRound2(cost[id])})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := str(out[i]["city"]), str(out[j]["city"]); a != b {
			return a < b
		}
		return str(out[i]["name"]) < str(out[j]["name"])
	})
	return out, nil
}

// compGrossOn is each employee's gross in month ym: the latest gross_history row from that month or earlier.
func compGrossOn(ctx context.Context, q store.Q, ids []string, ym string) (map[string]float64, error) {
	rows, err := q.C("gross_history").Aggregate(ctx, bson.A{
		bson.M{"$match": bson.M{"employee_id": bson.M{"$in": compNonNil(ids)}, "from_month": bson.M{"$lte": ym}}},
		bson.M{"$sort": compOrder("employee_id", "-from_month")},
		bson.M{"$group": bson.M{"_id": "$employee_id", "gross": bson.M{"$first": "$gross"}}},
	})
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, x := range rows {
		out[str(x["id"])], _ = x["gross"].(float64)
	}
	return out, nil
}

// compCityBudgets is each active city's headcount budget beside the employees (not inactive) of its locations.
func compCityBudgets(ctx context.Context, q store.Q) ([]store.Row, error) {
	cities, err := q.C("city").Find(ctx, bson.M{"status": "Active"}, options.Find().SetSort(compOrder("name", "_id")))
	if err != nil {
		return nil, err
	}
	locCity, _, err := compPlaces(ctx, q)
	if err != nil {
		return nil, err
	}
	heads, err := compHeadcounts(ctx, q)
	if err != nil {
		return nil, err
	}
	perCity := map[string]int64{} // the employees of each city's locations
	for loc, n := range heads {
		if ct, ok := locCity[loc]; ok {
			perCity[ct] += n
		}
	}
	out := make([]store.Row, 0, len(cities))
	for _, ct := range cities {
		out = append(out, store.Row{"id": ct["id"], "name": ct["name"], "headcount_budget": ct["headcount_budget"], "headcount": perCity[str(ct["id"])]})
	}
	return out, nil
}

func checkRates(st calc.Structure) error {
	switch {
	case st.BasicPct <= 0 || st.BasicPct > 100 || st.HraPct < 0 || st.HraPct > 100:
		return bad("Basic must be 1 to 100%% of gross and HRA 0 to 100%% of basic")
	case st.PfEe < 0 || st.PfEr < 0 || st.PfCeil < 0 || st.EsiEe < 0 || st.EsiEr < 0 || st.EsiCeil < 0:
		return bad("Rates and ceilings cannot be negative")
	case st.PfWage != "basic" && st.PfWage != "basicSa":
		return bad("PF wage is basic, or basic plus special allowance")
	case st.Denom != "calendar" && st.Denom != "30" && st.Denom != "26":
		return bad("Pay days are the calendar days of the month, 30 or 26")
	case st.VarCycle != 1 && st.VarCycle != 3 && st.VarCycle != 6 && st.VarCycle != 12:
		return bad("Variable pay is paid every 1, 3, 6 or 12 months")
	case st.VarPct < 0 || st.VarPct > 100 || st.TdsRate < 0 || st.TdsRate > 50:
		return bad("Check the variable and TDS percentages")
	}
	// Every other rate, cap and day count (SET-010): never negative, a percentage at most 100, the ESI ceiling above
	// zero (0 would switch ESI off for everyone), and the PF ceiling above zero when PF is capped.
	for _, x := range []struct {
		label    string
		v, max   float64
		positive bool
	}{{"PF employee %", st.PfEe, 100, false}, {"PF employer %", st.PfEr, 100, false}, {"ESI employee %", st.EsiEe, 100, false}, {"ESI employer %", st.EsiEr, 100, false},
		{"EPFO admin %", st.EpfoAdm, 100, false}, {"Medical insurance %", st.MedPct, 100, false}, {"Medical insurance cap", st.MedMax, 1e7, false},
		{"Gratuity %", st.Grat, 100, false}, {"PL days", st.PlDays, 366, false}, {"Bonus %", st.BonusPct, 100, false}, {"Bonus base", st.BonusCalc, 1e7, false},
		{"Bonus ceiling", st.BonusCeil, 1e7, false}, {"ESI ceiling", st.EsiCeil, 1e7, true}} {
		if x.v < 0 || x.v > x.max || (x.positive && x.v == 0) {
			if x.positive {
				return bad("%s must be above 0 and at most %v", x.label, x.max)
			}
			return bad("%s must be between 0 and %v", x.label, x.max)
		}
	}
	if st.PfCap && st.PfCeil <= 0 {
		return bad("PF ceiling must be above 0 when PF is capped")
	}
	return nil
}

// structureNameOK: a version name is at most 40 characters and unique ignoring case, since the Salary breakup
// template finds a version by its name in any case (SET-016).
func structureNameOK(ctx context.Context, q store.Q, name, self string) error {
	if n := len([]rune(name)); n > 40 {
		return bad("A version name can be at most 40 characters (this one has %d)", n)
	}
	if dup, _ := q.C("salary_structure").Exists(ctx, bson.M{"name": bulkCI(name), "_id": bson.M{"$ne": self}}); dup {
		return bad("A version named “%s” already exists", name)
	}
	return nil
}

func (s *Server) structureCreate(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("comp.structure", 2); err != nil {
		return nil, err
	}
	var b struct {
		Name     string `json:"name"`
		CopyFrom string `json:"copyFrom"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Name = strings.TrimSpace(b.Name); b.Name == "" {
		return nil, bad("Name the version, for example Version 2")
	}
	ctx := r.Context()
	if err := structureNameOK(ctx, s.st.DB, b.Name, ""); err != nil {
		return nil, err
	}
	st := calc.DefaultStructure
	if b.CopyFrom != "" {
		src, err := s.st.DB.C("salary_structure").FindOne(ctx, bson.M{"_id": b.CopyFrom}, options.FindOne().SetProjection(compFields("rates")))
		if err != nil || src == nil || compJSONInto(src["rates"], &st) != nil {
			return nil, bad("Unknown version to copy")
		}
	}
	if !st.Designed { // variable pay as a target on every version (17 Nov 2026): the fixed components follow the rates
		st.Comps, st.Designed, st.Converted, st.VarPct = calc.TargetComponents(st), true, true, 0
	}
	var id string
	err := s.st.Tx(ctx, func(tx store.Q) error {
		newID, err := tx.C("salary_structure").Insert(ctx, bson.M{"name": b.Name, "rates": st})
		if err != nil {
			return err
		}
		id = str(newID)
		return audit(ctx, tx, c, "Salary structure version added", "salary_structure", id, nil, b)
	})
	return map[string]string{"id": id}, err
}

func (s *Server) structureSave(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("comp.structure", 2); err != nil {
		return nil, err
	}
	var b struct {
		Name  string         `json:"name"`
		Rates calc.Structure `json:"rates"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if err := checkRates(b.Rates); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		cur, err := tx.C("salary_structure").Lock(ctx, bson.M{"_id": id})
		if err != nil {
			return err
		}
		var old calc.Structure
		if cur == nil || compJSONInto(cur["rates"], &old) != nil {
			return notFound("Version not found")
		}
		b.Rates = syncLegacyComps(old, b.Rates) // the components: a designed structure keeps its own, a converted one follows the rates
		set := bson.M{"rates": b.Rates}
		if name := strings.TrimSpace(b.Name); name != "" { // a blank name keeps the current one
			if err := structureNameOK(ctx, tx, name, id); err != nil {
				return err
			}
			set["name"] = name
		}
		if _, err := tx.C("salary_structure").Update(ctx, bson.M{"_id": id}, bson.M{"$set": set}); err != nil {
			return err
		}
		oldR, newR := old, b.Rates
		oldR.Comps, newR.Comps = nil, nil // the converted template follows the rates; its own history is the rates'
		ch := diff(toAny(oldR), toAny(newR))
		if len(ch) > 0 {
			if _, err := tx.C("config_history").Insert(ctx, bson.M{"entity": "salary_structure", "entity_id": id, "changes": ch, "by_name": c.Name}); err != nil {
				return err
			}
		}
		return audit(ctx, tx, c, "Salary structure rates changed", "salary_structure", id, old, b.Rates)
	})
}

func toAny(v any) map[string]any {
	b, _ := json.Marshal(v)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}

func (s *Server) structureDefault(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("comp.structure", 2); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		if _, err := tx.C("salary_structure").UpdateMany(ctx, bson.M{"is_default": true}, bson.M{"$set": bson.M{"is_default": false}}); err != nil {
			return err
		}
		n, err := tx.C("salary_structure").Update(ctx, bson.M{"_id": id, "status": "Active"}, bson.M{"$set": bson.M{"is_default": true}})
		if err != nil {
			return err
		}
		if n == 0 {
			return notFound("Version not found")
		}
		return audit(ctx, tx, c, "Default salary structure changed", "salary_structure", id, nil, nil)
	})
}

// structureMapAdd maps people to a version from a date: chosen IDs, or everyone matching a filter.
// Payroll (pay.run 2, the prototype's pay create) may map chosen people in its scope too.
func (s *Server) structureMapAdd(r *http.Request, c *Ctx) (any, error) {
	if c.Level("comp.structure") < 2 && c.Level("pay.run") < 2 {
		return nil, forbidden("")
	}
	var b struct {
		StructureID string   `json:"structureId"`
		FromDate    string   `json:"fromDate"`
		IDs         []string `json:"ids"`
		Type        string   `json:"type"`
		LocationID  string   `json:"locationId"`
		All         bool     `json:"all"`
		From        string   `json:"from"` // the version they move from, for the mapping log
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if !isDate(b.FromDate) {
		return nil, bad("Give the date the version applies from")
	}
	ctx := r.Context()
	if c.Level("comp.structure") < 2 {
		if len(b.IDs) == 0 {
			return nil, bad("Choose the employees to map")
		}
		for _, id := range b.IDs {
			if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
				return nil, err
			}
		}
	}
	var n int64
	err := s.st.Tx(ctx, func(tx store.Q) error {
		if ok, _ := tx.C("salary_structure").Exists(ctx, bson.M{"_id": b.StructureID, "status": "Active"}); !ok {
			return bad("Choose a version")
		}
		// A month already approved by payroll is not recalculated.
		if ok, _ := tx.C("payroll_month").Exists(ctx, bson.M{"ym": bson.M{"$gte": b.FromDate[:7]}, "status": bson.M{"$in": bson.A{"Approved", "Published"}}}); ok {
			return bad("Payroll for %s is already approved. Map from the open month", ymLabel(b.FromDate[:7]))
		}
		who := bson.M{"status": bson.M{"$ne": "Inactive"}}
		switch {
		case len(b.IDs) > 0:
			who["_id"] = bson.M{"$in": b.IDs}
		case b.Type != "" || b.LocationID != "" || b.All:
			if b.Type != "" {
				who["type"] = b.Type
			}
			if b.LocationID != "" {
				who["location_id"] = b.LocationID
			}
		default:
			return bad("Choose the people: IDs, a type, a location or everyone")
		}
		emps, err := tx.C("employee").Distinct(ctx, "_id", who)
		if err != nil {
			return err
		}
		// One mapping per person and date: a mapping already on that date moves to this version, the rest are added.
		ids := make([]string, len(emps))
		for i, e := range emps {
			ids[i] = str(e)
		}
		had, err := tx.C("structure_map").Distinct(ctx, "employee_id", bson.M{"employee_id": bson.M{"$in": ids}, "from_date": b.FromDate})
		if err != nil {
			return err
		}
		exists := map[string]bool{}
		for _, e := range had {
			exists[str(e)] = true
		}
		if len(had) > 0 {
			if _, err := tx.C("structure_map").UpdateMany(ctx, bson.M{"employee_id": bson.M{"$in": had}, "from_date": b.FromDate},
				bson.M{"$set": bson.M{"structure_id": b.StructureID}}); err != nil {
				return err
			}
		}
		var add []any
		for _, id := range ids {
			if !exists[id] {
				add = append(add, bson.M{"employee_id": id, "from_date": b.FromDate, "structure_id": b.StructureID})
			}
		}
		if err := tx.C("structure_map").InsertMany(ctx, add); err != nil {
			return err
		}
		n = int64(len(ids))
		toName, fromName := compStructureName(ctx, tx, b.StructureID), compStructureName(ctx, tx, b.From)
		return audit(ctx, tx, c, "Mapped people to a salary structure", "salary_structure", b.StructureID, nil,
			map[string]any{"from": b.FromDate, "count": n, "toName": toName, "fromName": fromName})
	})
	return map[string]int64{"mapped": n}, err
}

// compStructureName is a version's name ("" when there is none).
func compStructureName(ctx context.Context, q store.Q, id string) string {
	x, _ := q.C("salary_structure").FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(compFields("name")))
	return str(x["name"])
}

// compStructureDefault is the default version's id ("" when no version is the default).
func compStructureDefault(ctx context.Context, q store.Q) string {
	x, _ := q.C("salary_structure").FindOne(ctx, bson.M{"is_default": true}, options.FindOne().SetProjection(compFields("id")))
	return str(x["id"])
}

func (s *Server) structureMaps(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("comp.structure", 1); err != nil {
		return nil, err
	}
	ctx, sid := r.Context(), r.PathValue("id")
	maps, err := s.st.DB.C("structure_map").Find(ctx, bson.M{"structure_id": sid},
		options.Find().SetSort(compOrder("-from_date", "employee_id")).SetLimit(500))
	if err != nil {
		return nil, err
	}
	out := []store.Row{}
	if len(maps) == 0 {
		return out, nil
	}
	structName := compStructureName(ctx, s.st.DB, sid)
	ids := make([]string, len(maps))
	for i, m := range maps {
		ids[i] = str(m["employee_id"])
	}
	emps, err := s.st.DB.C("employee").Find(ctx, bson.M{"_id": bson.M{"$in": ids}}, options.Find().SetProjection(compFields("id", "name")))
	if err != nil {
		return nil, err
	}
	byID := compRowsByID(emps)
	for _, m := range maps {
		e := byID[str(m["employee_id"])]
		if e == nil {
			continue
		}
		out = append(out, store.Row{"employee_id": m["employee_id"], "name": e["name"], "from_date": m["from_date"], "structure": structName})
	}
	return out, nil
}

func (s *Server) bandSave(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("comp.bands", 2); err != nil {
		return nil, err
	}
	var b struct {
		CityID string  `json:"cityId"`
		Grade  string  `json:"grade"`
		Min    float64 `json:"min"`
		Max    float64 `json:"max"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Min <= 0 || b.Max < b.Min {
		return nil, bad("Minimum must be above zero and maximum at least the minimum")
	}
	ctx := r.Context()
	// The city and grade must be active masters, as the bulk template checks (SET-054).
	if ok, _ := s.st.DB.C("city").Exists(ctx, bson.M{"_id": compUUIDKey(b.CityID), "status": "Active"}); !ok {
		return nil, bad("Choose an active city")
	}
	if ok, _ := s.st.DB.C("grade").Exists(ctx, bson.M{"name": b.Grade, "status": "Active"}); !ok {
		return nil, bad("Grade “%s” is not an active grade", b.Grade)
	}
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		// before matched city_id::text=$1 (exact text); the conflict key went through $1::uuid (any case).
		before, err := tx.C("pay_band").FindOne(ctx, bson.M{"city_id": b.CityID, "grade": b.Grade})
		if err != nil {
			return err
		}
		key := bson.M{"city_id": compUUIDKey(b.CityID), "grade": b.Grade}
		if _, err := tx.C("pay_band").Upsert(ctx, key, bson.M{"min_gross": b.Min, "max_gross": b.Max}, nil); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Pay band changed", "pay_band", b.CityID+"/"+b.Grade, before, b)
	})
}

// compRound500 is round(v × factor / 500) × 500 in exact decimals, as numeric did (half away from zero).
func compRound500(v, factor float64) float64 {
	x, ok1 := new(big.Rat).SetString(strconv.FormatFloat(v, 'f', -1, 64))
	f, ok2 := new(big.Rat).SetString(strconv.FormatFloat(factor, 'f', -1, 64))
	if !ok1 || !ok2 {
		return math.Round(v*factor/500) * 500
	}
	x.Mul(x, f).Quo(x, big.NewRat(500, 1))
	neg := x.Sign() < 0
	x.Abs(x).Add(x, big.NewRat(1, 2))
	n := new(big.Int).Quo(x.Num(), x.Denom())
	if neg {
		n.Neg(n)
	}
	out, _ := new(big.Rat).SetInt(n.Mul(n, big.NewInt(500))).Float64()
	return out
}

// bandCopy copies every grade's band from one city to another, scaled by a factor (1 = same).
func (s *Server) bandCopy(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("comp.bands", 2); err != nil {
		return nil, err
	}
	var b struct {
		From   string  `json:"from"`
		To     string  `json:"to"`
		Factor float64 `json:"factor"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Factor == 0 { // not given: the same bands
		b.Factor = 1
	}
	if b.Factor < 0 || b.Factor > 5 { // a typo here would overwrite a whole city's bands (SET-056)
		return nil, bad("The factor must be above 0 and at most 5 (1 copies the same bands)")
	}
	if b.From == b.To {
		return nil, bad("Choose two different cities")
	}
	ctx := r.Context()
	var n int64
	err := s.st.Tx(ctx, func(tx store.Q) error {
		n = 0
		src, err := tx.C("pay_band").Find(ctx, bson.M{"city_id": b.From})
		if err != nil {
			return err
		}
		// The target city's bands before the copy, so the overwritten ones can be recovered (SET-060).
		was, err := tx.C("pay_band").Find(ctx, bson.M{"city_id": compUUIDKey(b.To)}, options.Find().SetProjection(bson.M{"_id": 0, "grade": 1, "min_gross": 1, "max_gross": 1}))
		if err != nil {
			return err
		}
		for _, x := range src {
			lo, _ := x["min_gross"].(float64)
			hi, _ := x["max_gross"].(float64)
			if _, err := tx.C("pay_band").Upsert(ctx, bson.M{"city_id": compUUIDKey(b.To), "grade": x["grade"]},
				bson.M{"min_gross": compRound500(lo, b.Factor), "max_gross": compRound500(hi, b.Factor)}, nil); err != nil {
				return err
			}
			n++
		}
		return audit(ctx, tx, c, "Pay bands copied", "pay_band", b.To, map[string]any{"bands": was}, b)
	})
	return map[string]int64{"copied": n}, err
}

func (s *Server) lwfSave(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.setup", 2); err != nil {
		return nil, err
	}
	var b struct {
		Rule          calc.LwfRule `json:"rule"`
		EffectiveFrom string       `json:"effectiveFrom"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if !isDate(b.EffectiveFrom) {
		return nil, bad("Give the date the rule applies from")
	}
	for _, a := range []calc.LwfAmount{b.Rule.Ee, b.Rule.Er} {
		if (a.Mode != "fixed" && a.Mode != "pct") || a.Val < 0 || a.Max < 0 || math.IsNaN(a.Val) {
			return nil, bad("Each side is a fixed amount or a percentage of gross")
		}
	}
	if b.Rule.Freq != "monthly" && b.Rule.Freq != "half" && b.Rule.Freq != "year" {
		return nil, bad("Deducted monthly, half-yearly or yearly")
	}
	if b.Rule.Freq != "monthly" && strings.TrimSpace(b.Rule.Months) == "" {
		return nil, bad("List the months it is deducted in, for example 6,12")
	}
	// Months are numbers 1 to 12 and a percentage at most 100 (SET-078): a month "13" would never be deducted.
	if m := strings.TrimSpace(b.Rule.Months); m != "" {
		for _, x := range strings.Split(m, ",") {
			if n, err := strconv.Atoi(strings.TrimSpace(x)); err != nil || n < 1 || n > 12 {
				return nil, bad("Months are numbers from 1 to 12, for example 6,12")
			}
		}
	}
	for _, a := range []calc.LwfAmount{b.Rule.Ee, b.Rule.Er} {
		if a.Mode == "pct" && a.Val > 100 {
			return nil, bad("A percentage of gross is at most 100")
		}
	}
	ctx, st := r.Context(), r.PathValue("state")
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		if _, err := tx.C("lwf_rule").Upsert(ctx, bson.M{"state_id": compUUIDKey(st), "effective_from": b.EffectiveFrom}, bson.M{"rule": b.Rule}, nil); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Labour welfare fund rule changed", "lwf_rule", st, nil, b)
	})
}

func (s *Server) budgetSave(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("comp.budget", 2); err != nil {
		return nil, err
	}
	var b struct {
		Headcount *int     `json:"headcount"`
		Payroll   *float64 `json:"payroll"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if (b.Headcount != nil && *b.Headcount < 0) || (b.Payroll != nil && *b.Payroll < 0) {
		return nil, bad("Budgets cannot be negative")
	}
	ctx, kind, id := r.Context(), r.PathValue("kind"), r.PathValue("id")
	table := map[string]string{"location": "location", "city": "city"}[kind]
	if table == "" || (kind == "city" && b.Payroll != nil) {
		return nil, bad("Budgets are kept per location (headcount and payroll) and per city (headcount)")
	}
	if kind == "location" {
		if err := c.MustInScopeLocation(id); err != nil {
			return nil, err
		}
	} else if c.Scope.Access != "all" {
		return nil, forbidden("")
	}
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		before, err := tx.C(table).FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(compFields("headcount_budget")))
		if err != nil {
			return err
		}
		if before == nil {
			return notFound("")
		}
		if b.Headcount != nil {
			if _, err := tx.C(table).Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"headcount_budget": *b.Headcount}}); err != nil {
				return err
			}
		}
		if b.Payroll != nil {
			if _, err := tx.C("location").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"payroll_budget": *b.Payroll}}); err != nil {
				return err
			}
		}
		return audit(ctx, tx, c, "Budget changed", table, id, before, b)
	})
}
