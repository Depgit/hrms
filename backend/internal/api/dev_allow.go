package api

// Deviation allowances of a work policy (owner, 24 Nov 2026; attend/devallow.go has the rules). Work policy ›
// Attendance › "Deviation allowances": Early check-in, Late check-in and Early check-out, each On/Off with "Minutes
// allowed" and "Times allowed in a month", both typed by the admin (no defaults; every rule off on every policy), and
// the Action for a day beyond the allowance (No action, Absent, Half day until regularized, Present until
// regularized; attend/devallow.go), picked by the admin: no default, a rule is not saved On without it. A rule saved
// On before the Action existed reads as No action (policy.DevRule.Action), so nothing already saved changes (hrms had
// no rule saved on 24 Nov 2026; no migration).
//
// The month's count is per employee and calendar month, in date order, over the days the attendance engine knows: a
// day of leave, a regularized day, a day called to work, a week off or a holiday is never a deviation day. While any
// active policy has a rule on, loadMonth reads each month from its 1st so the count is complete.
//
// "Admin informed": the attendance sweep (attDevSweep, every 5 minutes on the job queue, independent of the attendance
// alert settings) puts a bell notice to every active HR Admin and the employee's reporting manager for each deviation
// beyond the allowance, once per employee, date and rule (notice.ref "att-dev:<rule>:<date>:<employee>"). It looks at
// today and the two days before (a late or early check-in as soon as it is punched, an early check-out once the day
// is over), so turning a rule on never floods the bell with the month so far.

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/attend"
	"github.com/saltstayz/hrms/internal/jobs"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// checkDevAllow refuses a rule switched on without both numbers and an action, numbers out of range and an action
// that is not one of the four.
func checkDevAllow(d policy.DevRules) error {
	for _, x := range []struct {
		label string
		r     policy.DevRule
	}{{"Early check-in", d.EarlyIn}, {"Late check-in", d.LateIn}, {"Early check-out", d.EarlyOut}} {
		if x.r.Mins < 0 || x.r.Mins > 720 {
			return bad("%s: minutes allowed must be between 1 and 720", x.label)
		}
		if x.r.N < 0 || x.r.N > 31 {
			return bad("%s: times allowed in a month must be between 1 and 31", x.label)
		}
		if x.r.Act != "" && !containsStr(policy.DevActs, x.r.Act) {
			return bad("%s: the action must be No action, Absent, Half day until regularized or Present until regularized", x.label)
		}
		if !x.r.On {
			continue
		}
		var need []string
		if x.r.Mins == 0 || x.r.N == 0 {
			need = append(need, "enter the minutes allowed and the times allowed in a month")
		}
		if x.r.Act == "" {
			need = append(need, "choose the action for a day beyond the allowance")
		}
		if len(need) > 0 {
			return bad("%s is on: %s, or switch it off", x.label, strings.Join(need, " and "))
		}
	}
	return nil
}

// devActTail is the action of a deviation beyond the allowance for the bell: " → Half day until regularized" ("" for
// No action).
func devActTail(v attend.DevEvent) string {
	if v.Act == "" {
		return ""
	}
	return " → " + attend.ActStatus(v.Act)
}

// anyDev: an active work policy has a deviation allowance in force, or an employee has one of their own switched on
// (dev_own.go).
func (t *timeSet) anyDev() bool {
	for _, p := range t.pols {
		if p.S.Dev.Any() {
			return true
		}
	}
	return t.ownDevOnAny()
}

// devOn is the day's deviation allowance result (nil when neither the policy nor the employee's own rules have one in
// force, or the day has nothing). The whole calendar month is worked out once per employee.
func (m *monthData) devOn(e *emp, p *workPolicy, d string) *attend.DevDay {
	if !m.ts.devMaybe(e, p) || d > m.today || len(d) != 10 {
		return nil
	}
	if m.dev == nil {
		m.dev = map[string]map[string]*attend.DevDay{}
	}
	k := e.ID + "|" + d[:7]
	by, ok := m.dev[k]
	if !ok {
		by = m.devMonth(e, p, d[:7])
		m.dev[k] = by
	}
	return by[d]
}

// devMonth runs the allowances over the employee's month up to today.
func (m *monthData) devMonth(e *emp, p *workPolicy, ym string) map[string]*attend.DevDay {
	var days []attend.DevIn
	for d := ym + "-01"; d[:7] == ym && d <= m.today; d = policy.AddDays(d, 1) {
		in := attend.DevIn{Date: d, Plan: m.shiftPlan(e, p, d), Rec: m.rec(e, p, d), Done: d < m.today, Rules: m.ts.devRulesOn(e, p, d)}
		raw := m.raw[e.ID][d]
		in.Skip = d < e.DOJ || (e.LWD != nil && d > *e.LWD) || m.leaveOn(e, d, false) != nil || m.callOf(e.ID, d) != nil ||
			(raw != nil && raw.Src == "Regularized")
		days = append(days, in)
	}
	out := map[string]*attend.DevDay{}
	for i, x := range attend.Deviations(p.S, days) {
		if x.Allow != nil || len(x.Events) > 0 {
			out[days[i].Date] = &x
		}
	}
	return out
}

// ---------- The sweep: the admin informed ----------

const attDevEvery = 5 * time.Minute

var attDevMu sync.Mutex // one sweep at a time in this process (the periodic job runs on one leader)

// AttDevResult is what a sweep did.
type AttDevResult struct {
	Informed int `json:"informed"` // deviations reported (each to every HR Admin and the manager)
	Noted    int `json:"noted"`    // called-to-work leave days noted on their leave request
}

// AttDevRun is one sweep at now: deviations beyond the allowance are reported, and leave days called to work and
// worked get their line in the leave request's history (work_call.go).
func (s *Server) AttDevRun(ctx context.Context, now time.Time) (AttDevResult, error) {
	attDevMu.Lock()
	defer attDevMu.Unlock()
	var res AttDevResult
	q := s.st.DB
	ts, err := loadTimeSet(ctx, q)
	if err != nil {
		return res, err
	}
	d := now.In(ist).Format("2006-01-02")
	if ts.anyDev() {
		if res.Informed, err = s.attDevInform(ctx, q, ts, d); err != nil {
			return res, err
		}
	}
	res.Noted, err = s.callNoteSweep(ctx, ts, d)
	return res, err
}

// attDevInform reports the deviations beyond the allowance of today and the two days before.
func (s *Server) attDevInform(ctx context.Context, q store.Q, ts *timeSet, d string) (int, error) {
	from := policy.AddDays(d, -2)
	emps, err := loadEmps(ctx, q, bson.D{{Key: "status", Value: bson.D{{Key: "$ne", Value: "Pre-boarding"}}}, {Key: "doj", Value: bson.D{{Key: "$lte", Value: d}}},
		{Key: "$or", Value: bson.A{bson.D{{Key: "lwd", Value: nil}}, bson.D{{Key: "lwd", Value: bson.D{{Key: "$gte", Value: from}}}}}}})
	if err != nil || len(emps) == 0 {
		return 0, err
	}
	on := emps[:0]
	for _, e := range emps {
		if p, _ := ts.policyFor(e); ts.devMaybe(e, p) {
			on = append(on, e)
		}
	}
	if len(on) == 0 {
		return 0, nil
	}
	m, err := loadMonth(ctx, q, ts, ids(on), from, d)
	if err != nil {
		return 0, err
	}
	var admins []string
	adminsLoaded := false
	n := 0
	for _, e := range on {
		p, _ := ts.policyFor(e)
		for day := from; day <= d; day = policy.AddDays(day, 1) {
			x := m.devOn(e, p, day)
			if x == nil {
				continue
			}
			for _, v := range x.Events {
				key := "att-dev:" + v.Rule + ":" + day + ":" + e.ID
				if done, err := q.C("notice").Exists(ctx, bson.M{"ref": key}); err != nil {
					return n, err
				} else if done {
					continue
				}
				if !adminsLoaded {
					if admins, err = activeHRAdmins(ctx, q); err != nil {
						return n, err
					}
					adminsLoaded = true
				}
				text := fmt.Sprintf("%s (%s): %s on %s, %s%s", e.Name, e.ID, v.What()+" "+attend.MinText(v.Mins), longDate(day), devTail(v), devActTail(v))
				if err := s.st.Tx(ctx, func(tx store.Q) error {
					sent := map[string]bool{}
					for _, to := range admins {
						if err := devNotice(ctx, tx, to, text, key, "HR Admin"); err != nil {
							return err
						}
						sent[to] = true
					}
					if e.ManagerID != nil && *e.ManagerID != "" && *e.ManagerID != e.ID && !sent[*e.ManagerID] {
						return devNotice(ctx, tx, *e.ManagerID, text, key, "Line Manager")
					}
					return nil
				}); err != nil {
					return n, err
				}
				n++
			}
		}
	}
	return n, nil
}

// devTail: "6th this month (allowed 5)" or "more than the 2 h allowed".
func devTail(v attend.DevEvent) string {
	if v.K == 0 {
		return "more than the " + attend.MinText(v.M) + " allowed"
	}
	return fmt.Sprintf("%s this month (allowed %d)", attend.Ordinal(v.K), v.N)
}

func devNotice(ctx context.Context, q store.Q, to, text, key, view string) error {
	_, err := q.C("notice").Insert(ctx, bson.D{{Key: "to_employee_id", Value: to}, {Key: "from_employee_id", Value: nil}, {Key: "kind", Value: "attendance"},
		{Key: "text", Value: text}, {Key: "link", Value: "/attendance"}, {Key: "view", Value: view}, {Key: "ref", Value: []string{key}}})
	return err
}

// activeHRAdmins are the employees holding the HR Admin role who are not inactive.
func activeHRAdmins(ctx context.Context, q store.Q) ([]string, error) {
	maps, err := q.C("user_role_map").Find(ctx, bson.M{"role_name": "HR Admin", "active": true})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range maps {
		id := str(r["employee_id"])
		if id == "" || containsStr(out, id) {
			continue
		}
		if ok, err := q.C("employee").Exists(ctx, bson.M{"_id": id, "status": bson.M{"$ne": "Inactive"}}); err != nil {
			return nil, err
		} else if ok {
			out = append(out, id)
		}
	}
	return out, nil
}

// AttDevArgs is the periodic deviation and called-to-work sweep.
type AttDevArgs struct{}

func (AttDevArgs) Kind() string { return "att_dev" }

func (AttDevArgs) InsertOpts() jobs.InsertOpts {
	return jobs.InsertOpts{UniqueOpts: jobs.UniqueOpts{ByPeriod: attDevEvery}}
}

type attDevWorker struct {
	jobs.WorkerDefaults[AttDevArgs]
	s *Server
}

func (w *attDevWorker) Timeout(*jobs.Job[AttDevArgs]) time.Duration { return 2 * attDevEvery }

func (w *attDevWorker) Work(ctx context.Context, _ *jobs.Job[AttDevArgs]) error {
	if _, err := w.s.AttDevRun(ctx, time.Now()); err != nil {
		log.Printf("attendance deviations: %v", err) // the next sweep tries again
	}
	return nil
}
