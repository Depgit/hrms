package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/attend"
	"github.com/saltstayz/hrms/internal/jobs"
	"github.com/saltstayz/hrms/internal/mail"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// Attendance alerts (owner, 16 Nov 2026: "notify the employee" from the punches the HRMS receives). Five alerts,
// each off until HR turns it on (Attendance › Punch machine setup › Attendance alerts):
//
//  1. Missing punch: each morning at a set time, yesterday's days with an in and no out (or an out and no in) →
//     the employee, with the last day to regularize.
//  2. Late arrival: when today's in punch is later than the shift start plus the work policy's grace → the
//     employee, once a day.
//  3. Not punched in: shift start + grace + N minutes with no punch, on a working day without leave (approved or
//     waiting), week off, holiday or regularization → the employee, and one list per tick to their L1 manager.
//  4. Manager digest: at a set time, each L1 manager gets today's present, late, not punched in and on leave.
//  5. Payroll cut-off: N days before the payroll window ends (calendar month, or 26th–25th: company cut-off), the
//     employee's unregularized missing-punch, no-punch and short-hours absent days that would be loss of pay.
//
// Who: active employees employed on the date whose attendance mode is not "Manager marked" (a manager-marked person
// never punches). Bell always; email too when the company mailbox is set up and the person has an email address.
// SMS or WhatsApp: attAlertChannel below is the one place to plug a provider in; nothing is sent by it today.
//
// Safety: an absence is only claimed when the punches are known to be arriving. When the Realtime portal pull is on
// and its last run failed or is older than twice its interval, no missing-punch, not-punched-in alert and no digest
// absence line is sent ("punch feed stale"); and at a location where nobody has punched at all today by the alert
// time (a machine that is likely down), its people are not called absent ("no punches at <location> yet").
//
// Once only: each message carries a key in notice.ref ("att-alert:<type>:<date>:<employee>"), checked before
// sending; a sweep holds company_setting.att_alerts_lease while it runs, so two servers never sweep together. The
// sweep runs every 5 minutes on the job queue (AttAlertArgs) and can be run at any time.

// attAlertChannel sends an alert by SMS or WhatsApp in addition to the bell and email: set it when a provider is
// chosen (the same provider as the OTP's SMSSender, auth.go). nil = bell and email only.
var attAlertChannel func(ctx context.Context, employeeID, mobile, text string) error

const (
	attAlertEvery = 5 * time.Minute
	attAlertLease = 10 * time.Minute
	attNameCap    = 15 // names listed per digest line before "and N more"
)

// attAlertSet is company_setting.att_alerts.
type attAlertSet struct {
	MissingOn    bool   `json:"missingOn"`
	MissingAt    string `json:"missingAt"`
	LateOn       bool   `json:"lateOn"`
	NotInOn      bool   `json:"notInOn"`
	NotInAfter   int    `json:"notInAfter"`
	NotInManager bool   `json:"notInManager"`
	DigestOn     bool   `json:"digestOn"`
	DigestAt     string `json:"digestAt"`
	CutoffOn     bool   `json:"cutoffOn"`
	CutoffDays   int    `json:"cutoffDays"`
	CutoffAt     string `json:"cutoffAt"`
}

func attAlertDefaults() attAlertSet {
	return attAlertSet{MissingAt: "09:00", NotInAfter: 30, NotInManager: true, DigestAt: "11:00", CutoffDays: 5, CutoffAt: "10:00"}
}

func (a attAlertSet) any() bool {
	return a.MissingOn || a.LateOn || a.NotInOn || a.DigestOn || a.CutoffOn
}

func (a attAlertSet) doc() bson.M {
	return bson.M{"missingOn": a.MissingOn, "missingAt": a.MissingAt, "lateOn": a.LateOn, "notInOn": a.NotInOn, "notInAfter": a.NotInAfter,
		"notInManager": a.NotInManager, "digestOn": a.DigestOn, "digestAt": a.DigestAt, "cutoffOn": a.CutoffOn, "cutoffDays": a.CutoffDays, "cutoffAt": a.CutoffAt}
}

func attMap(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case bson.M:
		return m
	case bson.D:
		out := map[string]any{}
		for _, e := range m {
			out[e.Key] = e.Value
		}
		return out
	}
	return nil
}

func loadAttAlerts(ctx context.Context, q store.Q) (attAlertSet, store.Row, error) {
	a := attAlertDefaults()
	r, err := q.C("company_setting").FindOne(ctx, bson.M{"_id": 1}, options.FindOne().SetProjection(bson.M{"att_alerts": 1, "att_alerts_last": 1}))
	if err != nil || r == nil {
		return a, r, err
	}
	m := attMap(r["att_alerts"])
	b := func(k string) bool { return m[k] == true }
	s := func(k, def string) string {
		if _, ok := attend.Min(str(m[k])); ok {
			return str(m[k])
		}
		return def
	}
	n := func(k string, def int) int {
		if v := timectxInt(m[k]); v > 0 {
			return v
		}
		return def
	}
	if m != nil {
		a = attAlertSet{MissingOn: b("missingOn"), MissingAt: s("missingAt", a.MissingAt), LateOn: b("lateOn"), NotInOn: b("notInOn"),
			NotInAfter: n("notInAfter", a.NotInAfter), NotInManager: m["notInManager"] != false, DigestOn: b("digestOn"), DigestAt: s("digestAt", a.DigestAt),
			CutoffOn: b("cutoffOn"), CutoffDays: n("cutoffDays", a.CutoffDays), CutoffAt: s("cutoffAt", a.CutoffAt)}
	}
	return a, r, nil
}

// ---------- Sending ----------

// attSender sends the alerts of one sweep: bell, email when set up, and attAlertChannel when plugged in.
type attSender struct {
	s       *Server
	q       store.Q
	contact map[string][2]string // employee → email, mobile
	sent    map[string]int
}

func attKey(kind, date, who string) string { return "att-alert:" + kind + ":" + date + ":" + who }

func (x *attSender) already(ctx context.Context, key string) (bool, error) {
	n, err := x.q.C("notice").Count(ctx, bson.M{"ref": key})
	return n > 0, err
}

// send delivers text to employee to unless key was sent before; it reports whether it sent.
func (x *attSender) send(ctx context.Context, kind, key, to, subject, text, link, view string) (bool, error) {
	if done, err := x.already(ctx, key); err != nil || done {
		return false, err
	}
	if _, err := x.q.C("notice").Insert(ctx, bson.D{{Key: "to_employee_id", Value: to}, {Key: "from_employee_id", Value: nil}, {Key: "kind", Value: "attendance"},
		{Key: "text", Value: text}, {Key: "link", Value: link}, {Key: "view", Value: view}, {Key: "ref", Value: []string{key}}}); err != nil {
		return false, err
	}
	x.sent[kind]++
	c := x.contact[to]
	if x.s.mail != nil && c[0] != "" {
		comp := companyName(ctx, x.q)
		if err := x.s.mail.Send(ctx, mail.Message{To: c[0], Subject: comp + ": " + subject, Text: text + "\n\n" + comp + " HR\n"}); err != nil {
			log.Printf("attendance alert email to %s: %v", to, err)
		}
	}
	if attAlertChannel != nil && c[1] != "" {
		if err := attAlertChannel(ctx, to, c[1], text); err != nil {
			log.Printf("attendance alert message to %s: %v", to, err)
		}
	}
	return true, nil
}

func loadContacts(ctx context.Context, q store.Q, ids []string) (map[string][2]string, error) {
	out := map[string][2]string{}
	rows, err := q.C("employee_personal").Find(ctx, bson.M{"employee_id": bson.M{"$in": ids}},
		options.Find().SetProjection(bson.M{"employee_id": 1, "email": 1, "mobile": 1}))
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[str(r["employee_id"])] = [2]string{strings.TrimSpace(str(r["email"])), strings.TrimSpace(str(r["mobile"]))}
	}
	return out, nil
}

// ---------- Feed health ----------

// attFeedStale says why absences cannot be trusted now ("" when they can): the Realtime portal pull is on and its
// last run failed or is older than twice its interval.
func attFeedStale(ctx context.Context, q store.Q, now time.Time) (string, error) {
	st, err := rtSettings(ctx, q)
	if err != nil || st["on"] != true {
		return "", err
	}
	last := attMap(st["last"])
	at := attTime(last["at"])
	switch {
	case last == nil || at.IsZero():
		return "punch feed stale: the Realtime portal has not been pulled yet", nil
	case last["ok"] != true:
		return "punch feed stale: the last Realtime portal pull failed", nil
	case now.Sub(at) > 2*time.Duration(timectxInt(st["every"]))*time.Minute:
		return "punch feed stale: no Realtime portal pull since " + at.In(ist).Format("15:04"), nil
	}
	return "", nil
}

// attTime reads a stored instant, whatever form it comes back in.
func attTime(v any) time.Time {
	switch x := v.(type) {
	case time.Time:
		return x
	case bson.DateTime:
		return x.Time()
	case string:
		if t, err := time.Parse(time.RFC3339Nano, x); err == nil {
			return t
		}
	}
	return time.Time{}
}

// ---------- The sweep ----------

// AttAlertResult is what a sweep did.
type AttAlertResult struct {
	At      time.Time      `json:"at"`
	Sent    map[string]int `json:"sent"`
	Skipped map[string]int `json:"skipped"`
	Note    string         `json:"note"`
	// CutoffDone is the payroll month whose cut-off reminders went out (so later sweeps of the day skip the month's work).
	CutoffDone string `json:"cutoffDone,omitempty"`
}

var attKinds = []string{"missing", "late", "notin", "notin-mgr", "digest", "cutoff"}

// attClaim takes the sweep lease; false when another sweep holds it.
func attClaim(ctx context.Context, q store.Q, now time.Time) (bool, error) {
	n, err := q.C("company_setting").Update(ctx, bson.M{"_id": 1, "$or": bson.A{bson.M{"att_alerts_lease": nil}, bson.M{"att_alerts_lease": bson.M{"$lt": now}}}},
		bson.M{"$set": bson.M{"att_alerts_lease": now.Add(attAlertLease)}})
	return n > 0, err
}

func attHM(t time.Time) int { return t.Hour()*60 + t.Minute() }

func attAfter(now int, at string) bool { m, ok := attend.Min(at); return ok && now >= m }

// AttAlertsRun is one sweep at time now: it sends every alert that is on and due and not sent before.
func (s *Server) AttAlertsRun(ctx context.Context, now time.Time) (AttAlertResult, error) {
	res := AttAlertResult{At: now, Sent: map[string]int{}, Skipped: map[string]int{}}
	q := s.st.DB
	set, row, err := loadAttAlerts(ctx, q)
	if err != nil || row == nil || !set.any() {
		return res, err
	}
	ok, err := attClaim(ctx, q, now)
	if err != nil || !ok {
		return res, err
	}
	defer func() {
		_, _ = q.C("company_setting").Update(context.WithoutCancel(ctx), bson.M{"_id": 1}, bson.M{"$set": bson.M{"att_alerts_lease": nil}})
	}()
	prev := attMap(row["att_alerts_last"])
	err = s.attSweep(ctx, q, set, now, str(prev["cutoff_done"]), &res)
	if err != nil {
		res.Note = "Failed: " + err.Error()
	}
	// The card shows today's totals: add this sweep to what earlier sweeps of the same day recorded.
	day := now.In(ist).Format("2006-01-02")
	cutDone := str(prev["cutoff_done"])
	if res.CutoffDone != "" {
		cutDone = res.CutoffDone
	}
	tot, skip := map[string]any{}, map[string]any{}
	if str(prev["day"]) == day {
		for k, v := range attMap(prev["sent"]) {
			tot[k] = timectxInt(v)
		}
		for k, v := range attMap(prev["skipped"]) {
			skip[k] = timectxInt(v)
		}
	}
	for k, v := range res.Sent {
		tot[k] = timectxInt(tot[k]) + v
	}
	for k, v := range res.Skipped {
		skip[k] = timectxInt(skip[k]) + v
	}
	if _, uerr := q.C("company_setting").Update(context.WithoutCancel(ctx), bson.M{"_id": 1}, bson.M{"$set": bson.M{"att_alerts_last": bson.M{
		"at": now, "day": day, "sent": tot, "skipped": skip, "note": res.Note, "cutoff_done": cutDone}}}); uerr != nil && err == nil {
		err = uerr
	}
	return res, err
}

func (s *Server) attSweep(ctx context.Context, q store.Q, set attAlertSet, now time.Time, cutoffDone string, res *AttAlertResult) error {
	loc := now.In(ist)
	d := loc.Format("2006-01-02")
	y := policy.AddDays(d, -1)
	hm := attHM(loc)
	stale, err := attFeedStale(ctx, q, now)
	if err != nil {
		return err
	}
	ts, err := loadTimeSet(ctx, q)
	if err != nil {
		return err
	}
	// Everyone employed today, in ID order.
	emps, err := loadEmps(ctx, q, bson.D{{Key: "status", Value: "Active"}, {Key: "doj", Value: bson.D{{Key: "$lte", Value: d}}},
		{Key: "$or", Value: bson.A{bson.D{{Key: "lwd", Value: nil}}, bson.D{{Key: "lwd", Value: bson.D{{Key: "$gte", Value: d}}}}}}})
	if err != nil || len(emps) == 0 {
		return err
	}
	manual := map[string]bool{} // attendance marked by the manager: no punches expected
	mm, err := q.C("employee").Distinct(ctx, "_id", bson.M{"_id": bson.M{"$in": ids(emps)}, "attendance_mode": "Manager marked"})
	if err != nil {
		return err
	}
	for _, x := range mm {
		manual[str(x)] = true
	}
	cutoff := cutoffSetting(ctx, q)
	payYm := payMonthOf(d, cutoff)
	wFrom, wTo := window(payYm, cutoff)
	cutDue := set.CutoffOn && d == policy.AddDays(wTo, -set.CutoffDays) && attAfter(hm, set.CutoffAt) && cutoffDone != payYm
	from := y
	if cutDue && wFrom < from {
		from = wFrom
	}
	m, err := loadMonth(ctx, q, ts, ids(emps), from, d)
	if err != nil {
		return err
	}
	// Locations where someone has punched today: elsewhere a silent machine is likelier than everyone absent.
	active := map[string]bool{}
	byID := map[string]*emp{}
	for _, e := range emps {
		byID[e.ID] = e
	}
	punched, err := q.C("attendance_day").Distinct(ctx, "employee_id", bson.M{"date": d, "source": bson.M{"$in": bson.A{"Punch machine", "Mobile app"}}})
	if err != nil {
		return err
	}
	for _, x := range punched {
		if e := byID[str(x)]; e != nil {
			active[e.LocID] = true
		}
	}
	contactIDs := ids(emps)
	for _, e := range emps {
		if e.ManagerID != nil {
			contactIDs = append(contactIDs, *e.ManagerID)
		}
	}
	contact, err := loadContacts(ctx, q, contactIDs)
	if err != nil {
		return err
	}
	x := &attSender{s: s, q: q, contact: contact, sent: res.Sent}
	skip := func(why string) { res.Skipped[why]++ }
	const myAtt = "/me?tab=att"

	// 1. Missing punch, yesterday.
	if set.MissingOn && attAfter(hm, set.MissingAt) {
		if stale != "" {
			skip(stale)
		} else {
			for _, e := range emps {
				if manual[e.ID] || y < e.DOJ {
					continue
				}
				p, _ := ts.policyFor(e)
				if m.leaveOn(e, y, true) != nil || m.regs[e.ID][y] != "" {
					continue
				}
				st, _ := m.status(e, p, y)
				if st != "Missing out punch" && st != "Missing in punch" {
					continue
				}
				rec := m.rec(e, p, y)
				what := "out punch"
				have := ""
				if st == "Missing in punch" {
					what = "in punch"
					if rec != nil && rec.Out != "" {
						have = " (out " + rec.Out + ", no in)"
					}
				} else if rec != nil && rec.In != "" {
					have = " (in " + rec.In + ", no out)"
				}
				_, _, last := p.S.RegDeadline(y)
				by := "Regularise it by " + longDate(last) + " in My attendance."
				if last < d {
					by = "The time to regularise it has passed; speak to your manager."
				}
				text := fmt.Sprintf("Missing %s on %s%s. %s", what, longDate(y), have, by)
				if _, err := x.send(ctx, "missing", attKey("missing", y, e.ID), e.ID, "Missing "+what+" on "+longDate(y), text, myAtt, "Employee"); err != nil {
					return err
				}
			}
		}
	}

	// Today's plan and record of each punch-based employee, for the late, not-in and digest alerts.
	type dayInfo struct {
		e        *emp
		plan     attend.Plan
		rec      *attend.Rec
		leave    *leaveSpan
		start    int // late-mark start, minutes
		hasStart bool
		late     int // minutes late beyond the start (0 = on time)
		lateOK   bool
		due      int // minute from which no punch is "not punched in"
		notIn    bool
		silent   bool // nobody at the location has punched today
	}
	todays := map[string]*dayInfo{}
	for _, e := range emps {
		p, _ := ts.policyFor(e)
		t := &dayInfo{e: e, plan: m.planned(e, p, d), rec: m.rec(e, p, d), leave: m.leaveOn(e, d, true)}
		todays[e.ID] = t
		if manual[e.ID] || t.leave != nil || t.plan.Shift == nil {
			continue
		}
		t.start, t.hasStart = t.plan.Shift.Start, true
		if v, _ := m.deviation(e, p, d); v != nil {
			if ls, ov := attend.LateStartFor(v, m.ts.list); ov {
				t.hasStart = ls != nil
				if ls != nil {
					t.start = *ls
				}
			}
		}
		grace := int(p.S.Grace)
		if t.hasStart && len(t.plan.Short) > 0 { // an approved short leave at the start: late and not-in count from its end
			t.start = t.plan.ExcusedStart(t.start, grace)
		}
		if t.rec != nil && t.rec.In != "" {
			if in, ok := attend.Min(t.rec.In); ok && t.hasStart {
				if late := attLateBy(in, t.start); late > grace {
					t.late, t.lateOK = late, true
				}
			}
			continue
		}
		if t.rec != nil && !t.rec.Empty() {
			continue // an out alone: they came
		}
		if raw := m.raw[e.ID][d]; raw != nil && raw.Src == "Regularized" {
			continue
		}
		if m.regs[e.ID][d] != "" {
			continue
		}
		t.due = t.plan.ExcusedStart(t.plan.Shift.Start, grace) + grace + set.NotInAfter
		end := t.plan.Shift.End
		if end <= t.plan.Shift.Start {
			end += 1440 // ends next day
		}
		if t.due < 1440 && hm >= t.due && hm < end {
			t.notIn = true
			t.silent = !active[e.LocID]
		}
	}

	// 2. Late arrival, today.
	if set.LateOn {
		for _, e := range emps {
			t := todays[e.ID]
			if !t.lateOK {
				continue
			}
			p, _ := ts.policyFor(e)
			text := fmt.Sprintf("In at %s today, %s late (shift %s, grace %d min).", t.rec.In, attMins(t.late), attend.HHMM(t.start), int(p.S.Grace))
			if _, err := x.send(ctx, "late", attKey("late", d, e.ID), e.ID, "Late arrival on "+longDate(d), text, myAtt, "Employee"); err != nil {
				return err
			}
		}
	}

	// 3. Not punched in, today.
	if set.NotInOn {
		mgr := map[string][]string{}
		for _, e := range emps {
			t := todays[e.ID]
			if !t.notIn {
				continue
			}
			if stale != "" {
				skip(stale)
				continue
			}
			if t.silent {
				skip("no punches at " + or(e.Location, "a location") + " yet (machine may be offline)")
				continue
			}
			text := fmt.Sprintf("No punch-in yet today (shift %s–%s). If you are working, punch in now; if you are away, apply for leave in the HRMS.",
				attend.HHMM(t.plan.Shift.Start), attend.HHMM(t.plan.Shift.End))
			sent, err := x.send(ctx, "notin", attKey("notin", d, e.ID), e.ID, "No punch-in today", text, myAtt, "Employee")
			if err != nil {
				return err
			}
			if sent && set.NotInManager && e.ManagerID != nil && *e.ManagerID != "" {
				mgr[*e.ManagerID] = append(mgr[*e.ManagerID], fmt.Sprintf("%s (%s, shift %s)", e.Name, e.ID, attend.HHMM(t.plan.Shift.Start)))
			}
		}
		for _, id := range attSortedKeys(mgr) {
			l := mgr[id]
			text := "Not punched in yet today: " + attNames(l) + "."
			key := attKey("notin-mgr", d, id+":"+loc.Format("1504"))
			if _, err := x.send(ctx, "notin-mgr", key, id, "Team members not punched in", text, "/attendance", "Line Manager"); err != nil {
				return err
			}
		}
	}

	// 4. Manager digest, today.
	if set.DigestOn && attAfter(hm, set.DigestAt) {
		reports := map[string][]*emp{}
		for _, e := range emps {
			if e.ManagerID != nil && *e.ManagerID != "" && *e.ManagerID != e.ID {
				reports[*e.ManagerID] = append(reports[*e.ManagerID], e)
			}
		}
		for _, id := range attSortedKeys(reports) {
			key := attKey("digest", d, id)
			if done, err := x.already(ctx, key); err != nil {
				return err
			} else if done {
				continue
			}
			var present, late, notIn, onLeave, off, later, manualL []string
			silent := map[string]int{}
			for _, e := range reports[id] {
				t := todays[e.ID]
				switch {
				case t.leave != nil:
					onLeave = append(onLeave, e.Name+" ("+t.leave.Type+")")
				case manual[e.ID]:
					manualL = append(manualL, e.Name)
				case t.plan.Shift == nil:
					off = append(off, e.Name)
				case t.rec != nil && !t.rec.Empty():
					if t.lateOK {
						late = append(late, fmt.Sprintf("%s (in %s, %s late)", e.Name, t.rec.In, attMins(t.late)))
					} else {
						in := t.rec.In
						if in == "" {
							in = "out " + t.rec.Out
						}
						present = append(present, e.Name+" ("+in+")")
					}
				case t.notIn && t.silent:
					silent[or(e.Location, "their location")]++
				case t.notIn:
					notIn = append(notIn, fmt.Sprintf("%s (shift %s)", e.Name, attend.HHMM(t.plan.Shift.Start)))
				default:
					later = append(later, e.Name)
				}
			}
			lines := []string{fmt.Sprintf("Your team today, %s (%d people):", longDate(d), len(reports[id]))}
			add := func(label string, l []string) {
				if len(l) > 0 {
					lines = append(lines, fmt.Sprintf("%s %d: %s.", label, len(l), attNames(l)))
				}
			}
			add("Present", present)
			add("Late", late)
			if stale != "" {
				lines = append(lines, "Not punched in: not listed, the "+strings.TrimPrefix(stale, "punch feed stale: ")+".")
				if len(notIn) > 0 || len(silent) > 0 {
					skip(stale)
				}
			} else {
				add("Not punched in", notIn)
				for _, l := range attSortedKeys(silent) {
					lines = append(lines, fmt.Sprintf("No punches at %s yet (machine may be offline): %d people not counted as absent.", l, silent[l]))
					skip("no punches at " + l + " yet (machine may be offline)")
				}
			}
			add("Shift not started", later)
			add("On leave", onLeave)
			add("Week off or holiday", off)
			add("Marked by manager", manualL)
			if _, err := x.send(ctx, "digest", key, id, "Team attendance "+longDate(d), strings.Join(lines, "\n"), "/attendance", "Line Manager"); err != nil {
				return err
			}
		}
	}

	// 5. Payroll cut-off: the window's days that would be loss of pay.
	if cutDue {
		for _, e := range emps {
			if manual[e.ID] {
				continue
			}
			p, _ := ts.policyFor(e)
			var bad []string
			for dd := wFrom; dd < d; dd = policy.AddDays(dd, 1) {
				if dd < e.DOJ || m.leaveOn(e, dd, true) != nil || m.regs[e.ID][dd] != "" {
					continue
				}
				st, _ := m.status(e, p, dd)
				if strings.Contains(st, "Missing") || st == "No punch" || strings.HasPrefix(st, "Absent") {
					bad = append(bad, fmt.Sprintf("%s (%s)", attShort(dd), strings.ToLower(st)))
				}
			}
			if len(bad) == 0 {
				continue
			}
			text := fmt.Sprintf("Payroll cut-off for %s is %s. %d day%s in your attendance will be loss of pay unless you regularise %s or apply for leave: %s.",
				ymLabel(payYm), longDate(wTo), len(bad), map[bool]string{true: "", false: "s"}[len(bad) == 1], map[bool]string{true: "it", false: "them"}[len(bad) == 1],
				attNames(bad))
			if _, err := x.send(ctx, "cutoff", attKey("cutoff", payYm, e.ID), e.ID, "Attendance to fix before the payroll cut-off", text, myAtt, "Employee"); err != nil {
				return err
			}
		}
		res.CutoffDone = payYm
	}

	n := 0
	for _, v := range res.Sent {
		n += v
	}
	res.Note = fmt.Sprintf("%d message%s sent", n, map[bool]string{true: "", false: "s"}[n == 1])
	return nil
}

// attLateBy is how many minutes after start the check-in in is, taken relative to the nearest start (a 23:50 in
// for a 00:00 shift is early, not 23 hours late).
func attLateBy(in, start int) int {
	sg := ((in-start+2160)%1440+1440)%1440 - 720
	if sg < 0 {
		return 0
	}
	return sg
}

func attMins(n int) string {
	if n < 60 {
		return fmt.Sprintf("%d min", n)
	}
	return fmt.Sprintf("%dh %02dm", n/60, n%60)
}

func attShort(d string) string {
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		return d
	}
	return t.Format("02 Jan")
}

func attNames(l []string) string {
	if len(l) <= attNameCap {
		return strings.Join(l, ", ")
	}
	return strings.Join(l[:attNameCap], ", ") + fmt.Sprintf(" and %d more", len(l)-attNameCap)
}

func attSortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------- Job ----------

// AttAlertArgs is the periodic attendance alert sweep.
type AttAlertArgs struct{}

func (AttAlertArgs) Kind() string { return "att_alerts" }

func (AttAlertArgs) InsertOpts() jobs.InsertOpts {
	return jobs.InsertOpts{UniqueOpts: jobs.UniqueOpts{ByPeriod: attAlertEvery}}
}

type attAlertWorker struct {
	jobs.WorkerDefaults[AttAlertArgs]
	s *Server
}

func (w *attAlertWorker) Timeout(*jobs.Job[AttAlertArgs]) time.Duration { return attAlertLease }

func (w *attAlertWorker) Work(ctx context.Context, _ *jobs.Job[AttAlertArgs]) error {
	if _, err := w.s.AttAlertsRun(ctx, time.Now()); err != nil {
		log.Printf("attendance alerts: %v", err) // recorded on the setup card; the next sweep tries again
	}
	return nil
}

// ---------- Routes ----------

func (s *Server) attAlertsGet(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 1); err != nil {
		return nil, err
	}
	set, row, err := loadAttAlerts(r.Context(), s.st.DB)
	if err != nil {
		return nil, err
	}
	last := attMap(row["att_alerts_last"])
	if last != nil && str(last["day"]) != time.Now().In(ist).Format("2006-01-02") {
		last["sent"], last["skipped"] = map[string]any{}, map[string]any{} // the counts are today's
	}
	stale, _ := attFeedStale(r.Context(), s.st.DB, time.Now())
	return map[string]any{"settings": set, "last": last, "email": s.mail != nil, "channel": attAlertChannel != nil, "feed": stale}, nil
}

func (s *Server) attAlertsPut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 2); err != nil {
		return nil, err
	}
	var b attAlertSet
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	for _, t := range []struct{ v, label string }{{b.MissingAt, "Missing punch time"}, {b.DigestAt, "Digest time"}, {b.CutoffAt, "Cut-off reminder time"}} {
		if _, ok := attend.Min(t.v); !ok {
			return nil, bad("%s must look like 09:00", t.label)
		}
	}
	if b.NotInAfter < 0 || b.NotInAfter > 240 {
		return nil, bad("Not punched in: 0 to 240 minutes after the shift start and grace")
	}
	if b.CutoffDays < 1 || b.CutoffDays > 15 {
		return nil, bad("Cut-off reminder: 1 to 15 days before the cut-off")
	}
	ctx := r.Context()
	before, _, err := loadAttAlerts(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	if err := s.st.Tx(ctx, func(tx store.Q) error {
		if _, err := tx.C("company_setting").Update(ctx, bson.M{"_id": 1}, bson.M{"$set": bson.M{"att_alerts": b.doc()}}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Attendance alert settings saved", "company_setting", "1", before.doc(), b.doc())
	}); err != nil {
		return nil, err
	}
	return s.attAlertsGet(r, c)
}

// attAlertsTest sends the caller a sample alert (bell, and email when set up).
func (s *Server) attAlertsTest(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 2); err != nil {
		return nil, err
	}
	ctx := r.Context()
	contact, err := loadContacts(ctx, s.st.DB, []string{c.EmployeeID})
	if err != nil {
		return nil, err
	}
	x := &attSender{s: s, q: s.st.DB, contact: contact, sent: map[string]int{}}
	key := attKey("test", time.Now().In(ist).Format("2006-01-02T15:04:05.000"), c.EmployeeID)
	text := "Test of attendance alerts: this is how a message looks, for example \"Missing out punch on " + longDate(policy.AddDays(today(), -1)) +
		" (in 09:02, no out). Regularise it by … in My attendance.\""
	if _, err := x.send(ctx, "test", key, c.EmployeeID, "Test attendance alert", text, "/me?tab=att", ""); err != nil {
		return nil, err
	}
	to := "your bell"
	if s.mail != nil && contact[c.EmployeeID][0] != "" {
		to += " and " + contact[c.EmployeeID][0]
	}
	return map[string]any{"sent": true, "to": to}, nil
}
