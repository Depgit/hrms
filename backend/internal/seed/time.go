package seed

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/leave"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// timeConfig seeds leave plans and the five work policies of spec 10, once.
func timeConfig(ctx context.Context, tx store.Q) error {
	if n, err := tx.C("work_policy").Count(ctx, nil); err != nil || n > 0 {
		return err
	}
	lv := leave.LV{Year: "cal", Prorate: true, Half: true}
	lrx := leave.LRX{StaffOn: true, StaffPct: 20, Club: "ok"}
	// The prototype's ltFromLegacy, from the policy defaults: leave for permanent staff only.
	standard := func(plDays, plCF float64, plFrom string) map[string]leave.TypeCfg {
		t := map[string]leave.TypeCfg{}
		mk := func(name, kind string, f func(c *leave.TypeCfg)) {
			c := leave.Default(kind)
			c.Types = []string{"Permanent"}
			f(&c)
			t[name] = c
		}
		mk("Casual leave", leave.KindBalance, func(c *leave.TypeCfg) {
			c.Days, c.Back, c.MaxReq, c.Offs, c.Grp = 8, 3, 3, "sandwich", "C"
		})
		mk("Privilege leave", leave.KindBalance, func(c *leave.TypeCfg) {
			c.Days, c.From, c.CF, c.Encash, c.Adv, c.Back, c.AdvOnly = plDays, plFrom, plCF, true, 15, 0, true
		})
		mk("Sick leave", leave.KindBalance, func(c *leave.TypeCfg) {
			c.Days, c.Credit, c.DocAfter, c.DocLabel, c.Blk, c.Offs, c.Grp = 6, "upfront", 2, "Medical certificate", false, "sandwich", "C"
		})
		mk("Comp-off", leave.KindNone, func(c *leave.TypeCfg) { c.Days = 0 })
		mk("Loss of pay", leave.KindNone, func(c *leave.TypeCfg) { c.Days = 0 })
		mk("Maternity leave", leave.KindEvent, func(c *leave.TypeCfg) {
			c.Days, c.Gender, c.ApplyBy, c.DocLabel, c.Blk, c.Adv, c.Back, c.AdvOnly, c.Offs, c.MaxReq = 182, "Female", "hr", "Medical certificate", false, 30, 0, true, "include", 182
		})
		return t
	}
	none := standard(15, 30, "prob")
	for k, c := range none {
		c.On = false
		none[k] = c
	}
	plans := map[string]string{}
	for _, p := range []struct {
		name, desc string
		types      map[string]leave.TypeCfg
	}{
		{"Standard leave plan", "Casual 8, privilege 15 after probation, sick 6", standard(15, 30, "prob")},
		{"Property leadership leave plan", "Privilege 18 from joining, carry forward 45", standard(18, 45, "doj")},
		{"No leave credit", "For contract and retainer staff", none},
	} {
		id, err := tx.C("leave_plan").Insert(ctx, bson.M{"name": p.name, "description": p.desc, "lv": lv, "lrx": lrx, "types": p.types})
		if err != nil {
			return err
		}
		plans[p.name] = id.(string)
	}
	cal, err := tx.C("holiday_calendar").FindOne(ctx, bson.M{"name": "NCR 2026"})
	if err != nil {
		return err
	}
	if cal == nil {
		return store.ErrNoRows
	}
	ncr := cal["id"]
	for i, p := range []struct {
		name, plan string
		settings   map[string]any
		maps       [][2]string
	}{
		{"Operations – 6 day", "Standard leave plan", map[string]any{"desc": "Sundays off. The default for operations staff in every city."}, nil},
		{"Corporate office – 5.5 day", "Standard leave plan", map[string]any{"sat": "2,4", "desc": "Sundays and 2nd and 4th Saturdays off."},
			[][2]string{{"department", "Sales"}, {"department", "Finance"}, {"department", "People Ops"}, {"department", "Technology"}}},
		{"Operations – 24x7 rotational", "Standard leave plan", map[string]any{"offMode": "rotational", "desc": "Weekly off set on the roster."},
			[][2]string{{"department", "Front Office"}}},
		{"Property leadership", "Property leadership leave plan", map[string]any{"desc": "Privilege leave 18 from joining."},
			[][2]string{{"department", "Operations"}}},
		{"Contract and retainer staff", "No leave credit", map[string]any{"prob": 0, "optHol": 0, "notice": map[string]any{"prob": 15, "conf": 30}, "desc": "No probation and no leave credit."},
			[][2]string{{"type", "Contractual"}, {"type", "Retainer"}}},
	} {
		id, err := tx.C("work_policy").Insert(ctx, bson.M{"name": p.name, "sort": i + 1, "settings": p.settings, "leave_plan_id": plans[p.plan], "calendar_id": ncr})
		if err != nil {
			return err
		}
		for _, m := range p.maps {
			if _, err := tx.C("work_policy_map").Insert(ctx, bson.M{"policy_id": id, "level": m[0], "ref": m[1], "created_by": "Setup"}); err != nil {
				return err
			}
		}
	}
	return nil
}

// LinkCalendars points each city at its state's holiday calendar, on every policy with no city calendars yet.
func LinkCalendars(ctx context.Context, st *store.Store) error {
	calOf := map[string]string{"Haryana": "NCR 2026", "Delhi": "NCR 2026", "Karnataka": "Karnataka 2026", "Telangana": "Telangana 2026", "Maharashtra": "Maharashtra 2026"}
	calID := map[string]any{}
	cals, err := st.DB.C("holiday_calendar").Find(ctx, bson.M{"name": bson.M{"$in": []string{"NCR 2026", "Karnataka 2026", "Telangana 2026", "Maharashtra 2026"}}})
	if err != nil {
		return err
	}
	for _, c := range cals {
		calID[c["name"].(string)] = c["id"]
	}
	stateName := map[any]string{}
	states, err := st.DB.C("state").Find(ctx, nil)
	if err != nil {
		return err
	}
	for _, s := range states {
		stateName[s["id"]] = s["name"].(string)
	}
	cities, err := st.DB.C("city").Find(ctx, nil)
	if err != nil {
		return err
	}
	m := bson.M{} // jsonb_object_agg(city id, calendar id); '{}' when there is none
	for _, c := range cities {
		if id, ok := calID[calOf[stateName[c["state_id"]]]]; ok {
			m[c["id"].(string)] = id
		}
	}
	_, err = st.DB.C("work_policy").UpdateMany(ctx, bson.M{"cal_map": bson.M{}}, bson.M{"$set": bson.M{"cal_map": m}})
	return err
}

// SampleAttendance fills the last six weeks with punches for the sample organisation, once:
// mostly on time, with a few late, missing and off-day punches so every screen has something to show.
func SampleAttendance(ctx context.Context, st *store.Store) (int, error) {
	if n, err := st.DB.C("attendance_day").Count(ctx, nil); err != nil || n > 0 {
		return 0, err
	}
	type person struct {
		id, doj, dept, mode, start, end string
		corp                            bool
	}
	// Active employees with their department and shift (an inner join: both must exist), in id order.
	emps, err := st.DB.C("employee").Find(ctx, bson.M{"status": "Active"}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return 0, err
	}
	depts, err := st.DB.C("department").Find(ctx, nil)
	if err != nil {
		return 0, err
	}
	shifts, err := st.DB.C("shift").Find(ctx, nil)
	if err != nil {
		return 0, err
	}
	deptBy := map[any]store.Row{}
	for _, d := range depts {
		deptBy[d["id"]] = d
	}
	shiftBy := map[any]store.Row{}
	for _, s := range shifts {
		shiftBy[s["code"]] = s
	}
	var people []person
	for _, e := range emps {
		d, s := deptBy[e["department_id"]], shiftBy[e["shift_code"]]
		if d == nil || s == nil {
			continue
		}
		mode, _ := e["attendance_mode"].(string)
		people = append(people, person{id: e["id"].(string), doj: e["doj"].(string), dept: d["name"].(string), corp: d["corporate"] == true,
			mode: mode, start: s["start_time"].(string), end: s["end_time"].(string)})
	}
	hol := map[string]bool{}
	days, err := st.DB.C("holiday").Distinct(ctx, "date", bson.M{"kind": "National"})
	if err != nil {
		return 0, err
	}
	for _, d := range days {
		hol[d.(string)] = true
	}
	end := time.Now().AddDate(0, 0, -1)
	count := 0
	err = st.Tx(ctx, func(tx store.Q) error {
		rng := rand.New(rand.NewSource(7)) // the transaction may run again: same punches each time
		count = 0
		var docs []any
		for _, p := range people {
			s, _ := minutes(p.start)
			e, _ := minutes(p.end)
			if e <= s {
				e += 1440
			}
			src := "Mobile app"
			if p.mode == "Biometric" {
				src = "Punch machine"
			}
			sat := "none"
			if p.corp {
				sat = "2,4"
			}
			pol := policy.Defaults()
			pol.Sat = sat
			if p.dept == "Front Office" {
				pol.OffMode = "rotational"
			}
			for t := end.AddDate(0, 0, -42); !t.After(end); t = t.AddDate(0, 0, 1) {
				d := t.Format("2006-01-02")
				if d < p.doj {
					continue
				}
				code := pol.DefCode(p.id, "G", d, nil)
				if (code == "WO" || hol[d]) && rng.Float64() > 0.04 {
					continue
				}
				r := rng.Float64()
				if r < 0.03 {
					continue // no punch at all
				}
				in, out := s+rng.Intn(22)-8, e+rng.Intn(40)-5
				switch {
				case r < 0.06:
					out = -1
				case r < 0.08:
					in = -1
				case r < 0.095:
					in += 25
				}
				var inT, outT *string
				if in >= 0 {
					v := hhmm(in)
					inT = &v
				}
				if out >= 0 {
					v := hhmm(out)
					outT = &v
				}
				docs = append(docs, bson.M{"employee_id": p.id, "date": d, "in_time": inT, "out_time": outT, "source": src, "by_name": "Sample data"})
				count++
			}
		}
		return tx.C("attendance_day").InsertMany(ctx, docs)
	})
	return count, err
}

func minutes(t string) (int, bool) {
	var h, m int
	if _, err := fmt.Sscanf(t, "%d:%d", &h, &m); err != nil {
		return 0, false
	}
	return h*60 + m, true
}

func hhmm(m int) string {
	m = ((m % 1440) + 1440) % 1440
	return fmt.Sprintf("%02d:%02d", m/60, m%60)
}
