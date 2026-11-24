package api

import (
	"context"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/attend"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// Day records from machine punches, shift-aware (Set shift).
//
// A day's record (attendance_day: first in, last out) used to come from the punches of its calendar day, so a
// 22:00–06:00 shift got its in on one date and its out on the next and showed "Missing out punch". A day whose
// planned shift (the roster's shift for the date, else the policy default) has Shift ending next day, or whose
// employee's work policy sets a maximum early in or late out punch (Attendance › Punching; owner, 24 Nov 2026: these
// were on each shift before, with "Consider shift in", which was removed), is now built from the punches inside the
// shift's window: [start − early-in max, end of its last part (+1 day when it ends next day) + late-out max] on the
// recorded date; 4 hours either side when no limit is set. The first IN (or first punch) is the in, the last OUT (or
// last punch) the out, with out_date when the out is on a later date.
// The punches a window day uses (its in, its out and everything between) are not used again by the day before or
// after it. Every other day keeps its calendar-day record, exactly as before.
//
// Where: when punches arrive (and again when a shift version or a past roster cell changes), stored in
// attendance_day like every other day record, not at month calculation. Why: attendance_day stays the one record
// of the day that every screen, report, regularization (which edits the stored in/out) and the MIS-upload
// fallback read, so they cannot disagree with the month; and month calculation, run for 5,000 people by payroll,
// reports and registers, stays exactly as fast as before (no punch scan per request). The rebuild is batched: one
// query each for the people, roster, punches and stored days of the batch; the new days go in with one insert.

type punchPt struct {
	t   time.Time
	dir string
}

// dayPlanner knows each employee's planned shift (and its version) on a date.
type dayPlanner struct {
	ts     *timeSet
	emps   map[string]*emp
	roster map[string]map[string]string
	other  map[string]map[string]*attend.Shift // roster cells with other time for the day (roster_day.go)
	calls  map[string]map[string][2]string     // calls to work with a shift chosen on the roster: kind, shift
	pols   map[string]*workPolicy              // each employee's work policy, worked out once
}

func loadPlanner(ctx context.Context, q store.Q, ts *timeSet, ids []string, from, to string) (*dayPlanner, error) {
	pl := &dayPlanner{ts: ts, emps: map[string]*emp{}, roster: map[string]map[string]string{}, other: map[string]map[string]*attend.Shift{},
		calls: map[string]map[string][2]string{}, pols: map[string]*workPolicy{}}
	el, err := loadEmps(ctx, q, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}})
	if err != nil {
		return nil, err
	}
	for _, e := range el {
		pl.emps[e.ID] = e
	}
	rows, err := q.C("roster_entry").Find(ctx, bson.D{{Key: "employee_id", Value: bson.D{{Key: "$in", Value: ids}}}, {Key: "date", Value: timectxBetween(from, to)}},
		options.Find().SetProjection(bson.D{{Key: "employee_id", Value: 1}, {Key: "date", Value: 1}, {Key: "code", Value: 1},
			{Key: "start_time", Value: 1}, {Key: "end_time", Value: 1}, {Key: "start2_time", Value: 1}, {Key: "end2_time", Value: 1}}))
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		id := str(r["employee_id"])
		if pl.roster[id] == nil {
			pl.roster[id], pl.other[id] = map[string]string{}, map[string]*attend.Shift{}
		}
		pl.roster[id][str(r["date"])] = str(r["code"])
		if o := otherOfRow(r); o != nil {
			pl.other[id][str(r["date"])] = o
		}
	}
	rows, err = q.C(store.WorkCallColl).Find(ctx, bson.D{{Key: "employee_id", Value: bson.D{{Key: "$in", Value: ids}}}, {Key: "date", Value: timectxBetween(from, to)},
		{Key: "shift_code", Value: bson.D{{Key: "$ne", Value: nil}}}})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		id := str(r["employee_id"])
		if pl.calls[id] == nil {
			pl.calls[id] = map[string][2]string{}
		}
		pl.calls[id][str(r["date"])] = [2]string{str(r["kind"]), str(r["shift_code"])}
	}
	return pl, nil
}

// shiftFor is the day's planned shift with the version in effect on the date, and the employee's work policy.
func (pl *dayPlanner) shiftFor(id, d string) (attend.Shift, *attend.Rules, *workPolicy, bool) {
	e := pl.emps[id]
	if e == nil {
		return attend.Shift{}, nil, nil, false
	}
	p := pl.pols[id]
	if p == nil {
		p, _ = pl.ts.policyFor(e)
		pl.pols[id] = p
	}
	code, ok := pl.roster[id][d]
	if !ok {
		code = p.S.DefCode(e.ID, pl.ts.baseShift(e), d, pl.ts.holidays(p, e.CityID))
	}
	// Called to work from the roster with a shift: the day's punches are those of that shift (on a holiday, or on a
	// day of leave, which the roster does not show).
	if x, ok := pl.calls[id][d]; ok && x[1] != "" && (x[0] == "leave" || code == "PH") {
		if sh, r, found := pl.ts.shiftOn(x[1], d); found {
			return sh, r, p, true
		}
	}
	if o := pl.other[id][d]; o != nil && code == otherCode { // other time for this day
		return *o, otherRules(*o), p, true
	}
	sh, r, found := pl.ts.shiftOn(code, d)
	return sh, r, p, found
}

// window is the span of punches that belong to the day; ok is false for a calendar day.
func (pl *dayPlanner) window(id, d string) (lo, hi time.Time, ok bool) {
	sh, r, p, found := pl.shiftFor(id, d)
	if !found {
		return lo, hi, false
	}
	early, late := attend.PunchLimits(&p.S)
	if !r.Windowed(early, late) {
		return lo, hi, false
	}
	lo, hi = shiftWindow(d, sh, early, late)
	return lo, hi, true
}

// shiftWindow is [start − early-in max, end of the last part + late-out max] for a shift recorded on date d; early
// and late are the work policy's limits in minutes, 4 hours when 0 (no limit).
func shiftWindow(d string, sh attend.Shift, early, late int) (time.Time, time.Time) {
	day, _ := time.ParseInLocation("2006-01-02", d, ist)
	start, end := sh.Start, sh.LastEnd()
	if end <= start {
		end += 1440
	}
	if early <= 0 {
		early = 240
	}
	if late <= 0 {
		late = 240
	}
	return day.Add(time.Duration(start-early) * time.Minute), day.Add(time.Duration(end+late) * time.Minute)
}

// pick chooses the in and out from a day's punches (sorted): the first IN (or first punch) and the last OUT
// (or last punch); one OUT alone is an out with no in. used is the span of punches the day takes.
func pick(ps []punchPt) (in, out *time.Time, used [2]time.Time) {
	// Repeats within 2 minutes are ignored (spec 11.2 machine rules; TIME-233): a double tap at 06:00 and 06:01 is
	// one punch, not an in and an out.
	kept := make([]punchPt, 0, len(ps))
	for _, p := range ps {
		if n := len(kept); n > 0 && p.t.Sub(kept[n-1].t) < 2*time.Minute {
			continue
		}
		kept = append(kept, p)
	}
	ps = kept
	if len(ps) == 0 {
		return nil, nil, used
	}
	a, b := ps[0].t, ps[len(ps)-1].t
	for _, p := range ps {
		if p.dir == "IN" {
			a = p.t
			break
		}
	}
	for i := len(ps) - 1; i >= 0; i-- {
		if ps[i].dir == "OUT" {
			b = ps[i].t
			break
		}
	}
	if len(ps) == 1 && ps[0].dir == "OUT" {
		return nil, &b, [2]time.Time{b, b}
	}
	if len(ps) > 1 && b.After(a) {
		return &a, &b, [2]time.Time{a, b}
	}
	return &a, nil, [2]time.Time{a, a}
}

type builtDay struct {
	in, out, outDate string
	empty            bool // nothing left for the day
	window           bool
	evidence         bool // there were punches on its date or in its window
}

func inUsed(t time.Time, spans [][2]time.Time) bool {
	for _, u := range spans {
		if !t.Before(u[0]) && !t.After(u[1]) {
			return true
		}
	}
	return false
}

// buildDays works out the records for dates R of one employee from their punches (sorted by time).
func (pl *dayPlanner) buildDays(id string, R []string, ps []punchPt) map[string]builtDay {
	sort.Strings(R)
	type win struct {
		lo, hi time.Time
		used   [2]time.Time
		in     *time.Time
		out    *time.Time
		any    bool
	}
	wins := map[string]*win{}
	usedBy := func(w *win) [][2]time.Time {
		if w == nil || (w.in == nil && w.out == nil) {
			return nil
		}
		return [][2]time.Time{w.used}
	}
	// Window days around R, in date order: each leaves out what the window day before it used.
	var prev *win
	for d := policy.AddDays(R[0], -2); d <= policy.AddDays(R[len(R)-1], 2); d = policy.AddDays(d, 1) {
		lo, hi, ok := pl.window(id, d)
		if !ok {
			prev = nil
			continue
		}
		w := &win{lo: lo, hi: hi}
		excl := usedBy(prev)
		var cand []punchPt
		for _, p := range ps {
			if p.t.Before(lo) || p.t.After(hi) {
				continue
			}
			w.any = true
			if !inUsed(p.t, excl) {
				cand = append(cand, p)
			}
		}
		w.in, w.out, w.used = pick(cand)
		wins[d] = w
		prev = w
	}
	out := map[string]builtDay{}
	for _, d := range R {
		if w := wins[d]; w != nil {
			b := builtDay{window: true, evidence: w.any || pl.hasOn(ps, d)}
			if w.in == nil && w.out == nil {
				b.empty = true
			} else {
				if w.in != nil {
					b.in = w.in.In(ist).Format("15:04")
				}
				if w.out != nil {
					b.out = w.out.In(ist).Format("15:04")
					if w.in != nil {
						b.outDate = outDate(d, *w.in, *w.out)
					}
				}
			}
			out[d] = b
			continue
		}
		// A calendar day, without what a window day next to it used.
		day, _ := time.ParseInLocation("2006-01-02", d, ist)
		next := day.AddDate(0, 0, 1)
		excl := append(usedBy(wins[policy.AddDays(d, -1)]), usedBy(wins[policy.AddDays(d, 1)])...)
		var cand []punchPt
		b := builtDay{}
		for _, p := range ps {
			if p.t.Before(day) || !p.t.Before(next) {
				continue
			}
			b.evidence = true
			if inUsed(p.t, excl) {
				continue
			}
			cand = append(cand, p)
		}
		in, o, _ := pick(cand)
		switch {
		case in == nil && o == nil:
			b.empty = true
		default:
			if in != nil {
				b.in = in.In(ist).Format("15:04")
			}
			if o != nil {
				b.out = o.In(ist).Format("15:04")
			}
		}
		out[d] = b
	}
	return out
}

// outDate is the stored out date: the out punch's date when it is later than the recorded date and the in is
// on the recorded date. When the in is on another date ("previous" or "next" day shifts), it keeps the span
// right for the engine: empty when the out is on the in's date or the next morning, else the recorded date
// plus the days between in and out.
func outDate(d string, in, out time.Time) string {
	id, od := in.In(ist).Format("2006-01-02"), out.In(ist).Format("2006-01-02")
	n := policy.DayDiff(id, od)
	switch {
	case n <= 0:
		return ""
	case id == d:
		return od
	case n == 1 && out.In(ist).Format("15:04") <= in.In(ist).Format("15:04"):
		return ""
	}
	return policy.AddDays(d, n)
}

func (pl *dayPlanner) hasOn(ps []punchPt, d string) bool {
	for _, p := range ps {
		if p.t.In(ist).Format("2006-01-02") == d {
			return true
		}
	}
	return false
}

type storedDay struct{ in, out, outDate, src string }

// rebuildPunchDays rebuilds the day records touched by punches: touched holds, per employee, the calendar dates
// of the punches (or the dates to look at again). A neighbouring date is rebuilt too when it or the touched date
// is a window day. hook is true for a rebuild after a shift version or roster change: then only days whose
// record came from the punch machine (or that have none) are written, and only when they change.
func rebuildPunchDays(ctx context.Context, tx store.Q, ts *timeSet, touched map[string]map[string]bool, hook bool) error {
	if len(touched) == 0 {
		return nil
	}
	ids := make([]string, 0, len(touched))
	minD, maxD := "9999-12-31", "0000-01-01"
	for id, ds := range touched {
		ids = append(ids, id)
		for d := range ds {
			if d < minD {
				minD = d
			}
			if d > maxD {
				maxD = d
			}
		}
	}
	if ts == nil {
		var err error
		if ts, err = loadTimeSet(ctx, tx); err != nil {
			return err
		}
	}
	pl, err := loadPlanner(ctx, tx, ts, ids, policy.AddDays(minD, -4), policy.AddDays(maxD, 4))
	if err != nil {
		return err
	}
	// The dates to rebuild for each employee.
	plan := map[string][]string{}
	for id, ds := range touched {
		set := map[string]bool{}
		for c := range ds {
			set[c] = true
			_, _, wc := pl.window(id, c)
			for _, n := range []string{policy.AddDays(c, -1), policy.AddDays(c, 1)} {
				if _, _, wn := pl.window(id, n); wc || wn {
					set[n] = true
				}
			}
		}
		for d := range set {
			plan[id] = append(plan[id], d)
		}
	}
	lo, _ := time.ParseInLocation("2006-01-02", policy.AddDays(minD, -3), ist)
	hi, _ := time.ParseInLocation("2006-01-02", policy.AddDays(maxD, 4), ist)
	who := bson.E{Key: "employee_id", Value: bson.D{{Key: "$in", Value: ids}}}
	punches := map[string][]punchPt{}
	rows, err := tx.C("punch").Find(ctx, bson.D{who, {Key: "ts", Value: bson.D{{Key: "$gte", Value: lo}, {Key: "$lt", Value: hi}}}},
		options.Find().SetSort(bson.D{{Key: "employee_id", Value: 1}, {Key: "ts", Value: 1}}).
			SetProjection(bson.D{{Key: "_id", Value: 0}, {Key: "employee_id", Value: 1}, {Key: "ts", Value: 1}, {Key: "direction", Value: 1}}))
	if err != nil {
		return err
	}
	for _, r := range rows {
		t, _ := r["ts"].(time.Time)
		id := str(r["employee_id"])
		punches[id] = append(punches[id], punchPt{t: t, dir: str(r["direction"])})
	}
	stored := map[string]map[string]storedDay{}
	rows, err = tx.C("attendance_day").Find(ctx, bson.D{who, {Key: "date", Value: timectxBetween(policy.AddDays(minD, -1), policy.AddDays(maxD, 1))}})
	if err != nil {
		return err
	}
	for _, r := range rows {
		id := str(r["employee_id"])
		if stored[id] == nil {
			stored[id] = map[string]storedDay{}
		}
		stored[id][str(r["date"])] = storedDay{in: str(r["in_time"]), out: str(r["out_time"]), outDate: str(r["out_date"]), src: str(r["source"])}
	}
	// The days to write are made in memory, then written together: the stored days that change or go are
	// deleted (one statement per few hundred employees) and the new and changed ones inserted in one go. A day
	// written one at a time cost a round trip each, which for a back-dated shift change over many people ran past
	// MongoDB's 60 s transaction limit. Every column of a changed day is rewritten, so this is the old UPDATE.
	now := tx.Now()
	var add []any
	drop := map[string][]string{} // employee → stored dates to delete (gone, or rewritten below)
	for id, R := range plan {
		for d, x := range pl.buildDays(id, R, punches[id]) {
			s, has := stored[id][d]
			switch {
			case !x.evidence: // no punches on the date or in its window: leave the day as it is
				continue
			case has && s.src == "Regularized":
				continue
			case has && x.window && s.src == "MIS upload":
				continue
			case has && s.src == "MIS upload" && s.in != "" && s.out != "" && (x.empty || x.in == "" || x.out == ""):
				// A complete uploaded day is not replaced by an incomplete one from the machine, such as a stray
				// punch (TIME-237); a complete machine day still takes over, as the upload is the fallback.
				continue
			case hook && has && s.src != "Punch machine":
				continue
			}
			if x.empty {
				if has && s.src == "Punch machine" {
					drop[id] = append(drop[id], d)
				}
				continue
			}
			if hook && has && s.in == x.in && s.out == x.out && s.outDate == x.outDate {
				continue
			}
			doc := bson.M{"employee_id": id, "date": d, "in_time": timectxNullIf(x.in), "out_time": timectxNullIf(x.out), "out_date": timectxNullIf(x.outDate),
				"source": "Punch machine", "by_name": "Punch machine"}
			if has {
				drop[id] = append(drop[id], d)
				doc["at"] = now
			}
			add = append(add, doc)
		}
	}
	var ors bson.A
	flush := func() error {
		if len(ors) == 0 {
			return nil
		}
		_, err := tx.C("attendance_day").DeleteMany(ctx, bson.D{{Key: "$or", Value: ors}})
		ors = nil
		return err
	}
	for id, ds := range drop {
		ors = append(ors, bson.D{{Key: "employee_id", Value: id}, {Key: "date", Value: bson.D{{Key: "$in", Value: ds}}}})
		if len(ors) == 500 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	return tx.C("attendance_day").InsertMany(ctx, add)
}

// rebuildForShift brings the punch-machine days from `from` to today in line with a shift's new version
// (only needed when its window changes: times, Shift ending next day, Consider shift in, punch limits).
func rebuildForShift(ctx context.Context, tx store.Q, from string) error {
	t := today()
	if from > t {
		return nil
	}
	if locked, err := punchdaysLastLocked(ctx, tx); err == nil && locked != "" && from <= policy.LastDayOf(locked) {
		from = policy.AddDays(policy.LastDayOf(locked), 1)
	}
	lo, _ := time.ParseInLocation("2006-01-02", policy.AddDays(from, -1), ist)
	hi, _ := time.ParseInLocation("2006-01-02", policy.AddDays(t, 2), ist)
	// Each employee and India date with punches in the span.
	rows, err := tx.C("punch").Aggregate(ctx, bson.A{
		bson.D{{Key: "$match", Value: bson.D{{Key: "ts", Value: bson.D{{Key: "$gte", Value: lo}, {Key: "$lt", Value: hi}}}}}},
		bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: bson.D{{Key: "e", Value: "$employee_id"},
			{Key: "d", Value: bson.D{{Key: "$dateToString", Value: bson.D{{Key: "format", Value: "%Y-%m-%d"}, {Key: "date", Value: "$ts"}, {Key: "timezone", Value: "Asia/Kolkata"}}}}}}}}}},
	})
	if err != nil {
		return err
	}
	touched := map[string]map[string]bool{}
	for _, r := range rows {
		k, _ := r["id"].(map[string]any)
		id, d := str(k["e"]), str(k["d"])
		if d < from || d > t {
			continue
		}
		if touched[id] == nil {
			touched[id] = map[string]bool{}
		}
		touched[id][d] = true
	}
	return rebuildPunchDays(ctx, tx, nil, touched, true)
}

// punchdaysLastLocked is the latest payroll month with locked attendance ("" for none).
func punchdaysLastLocked(ctx context.Context, q store.Q) (string, error) {
	r, err := q.C("payroll_month").FindOne(ctx, bson.D{{Key: "$or", Value: bson.A{
		bson.D{{Key: "locked_at", Value: bson.D{{Key: "$ne", Value: nil}}}}, bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{"Approved", "Published"}}}}}}}},
		options.FindOne().SetSort(bson.D{{Key: "ym", Value: -1}}).SetProjection(bson.D{{Key: "ym", Value: 1}}))
	if err != nil || r == nil {
		return "", err
	}
	return str(r["ym"]), nil
}
