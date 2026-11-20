package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/attend"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// ---------- Attendance views (spec 11.2-11.4) ----------

func hoursText(rec *attend.Rec) string {
	if rec == nil || rec.In == "" || rec.Out == "" {
		return ""
	}
	a, _ := attend.Min(rec.In)
	b, _ := attend.Min(rec.Out)
	if b <= a {
		b += 1440
	}
	return fmt.Sprintf("%.1f", float64(b-a)/60)
}

func summaryKey(st string) string {
	switch {
	case st == "Present", st == "Late", st == "Half day":
		return st
	case st == "No punch", strings.HasPrefix(st, "Absent"):
		return "No punch"
	case strings.Contains(st, "Missing"):
		return "Missing punch"
	}
	return ""
}

// attendanceList: one row per employee for the month, or one employee's days.
func (s *Server) attendanceList(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.records", 1); err != nil {
		return nil, err
	}
	ctx, qv := r.Context(), r.URL.Query()
	ym := or(qv.Get("ym"), today()[:7])
	from, to, err := monthRange(ym)
	if err != nil {
		return nil, err
	}
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	if id := qv.Get("employee"); id != "" {
		if id != c.EmployeeID {
			if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
				return nil, err
			}
		}
		e, err := loadEmp(ctx, s.st.DB, id)
		if err != nil {
			return nil, err
		}
		m, err := loadMonth(ctx, s.st.DB, ts, []string{id}, from, to)
		if err != nil {
			return nil, err
		}
		p, via := ts.policyFor(e)
		days := []map[string]any{}
		counts := map[string]int{}
		for d := to; d >= from; d = policy.AddDays(d, -1) {
			if d > today() || d < e.DOJ {
				continue
			}
			rec := m.rec(e, p, d)
			plan := m.planned(e, p, d)
			st, tone := m.status(e, p, d)
			row := map[string]any{"date": d, "planned": plan.Code, "status": st, "tone": tone, "hours": hoursText(rec)}
			if rec != nil {
				row["in"], row["out"], row["src"], row["carried"], row["orphan"] = rec.In, rec.Out, rec.Src, rec.Carried, rec.Orphan
			}
			if v, x := m.deviation(e, p, d); v != nil {
				row["dev"] = v
				if x != nil {
					row["devStatus"] = x.Status
				} else {
					row["devStatus"] = "Pending"
				}
			}
			if rid := m.regs[e.ID][d]; rid != "" {
				row["regPending"] = rid
			}
			if k := summaryKey(st); k != "" {
				counts[k]++
			}
			days = append(days, row)
		}
		var shift *attend.Shift
		on := to // the shift's timings in effect at the end of the range shown (Set shift versions)
		if on > today() {
			on = today()
		}
		if sh, _, ok := ts.shiftOn(ts.baseShift(e), on); ok {
			shift = &sh
		}
		out := map[string]any{"employee": map[string]any{"id": e.ID, "name": e.Name, "policy": p.Name, "via": via, "grace": p.S.Grace}, "days": days, "counts": counts}
		if shift != nil {
			out["shift"] = map[string]string{"code": shift.Code, "start": attend.HHMM(shift.Start), "end": attend.HHMM(shift.End)}
		}
		return out, nil
	}
	// Everyone but Pre-boarding who had not left before the month, optionally at one property.
	f := bson.D{{Key: "status", Value: bson.D{{Key: "$ne", Value: "Pre-boarding"}}},
		{Key: "$or", Value: bson.A{bson.D{{Key: "lwd", Value: nil}}, bson.D{{Key: "lwd", Value: bson.D{{Key: "$gte", Value: from}}}}}}}
	if loc := qv.Get("location"); loc != "" {
		f = append(f, bson.E{Key: "location_id", Value: loc})
	}
	emps, err := scopedEmps(ctx, s.st.DB, c, f)
	if err != nil {
		return nil, err
	}
	m, err := loadMonth(ctx, s.st.DB, ts, ids(emps), from, to)
	if err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	for _, e := range emps {
		p, _ := ts.policyFor(e)
		cnt := map[string]int{}
		for d := from; d <= to && d < today(); d = policy.AddDays(d, 1) { // today is not over yet
			if d < e.DOJ || (e.LWD != nil && d > *e.LWD && e.Status != "Active") {
				continue
			}
			if l := m.leaveOn(e, d, false); l != nil {
				cnt["Leave"]++
				cnt["On leave"]++
				continue
			}
			st, _ := m.status(e, p, d)
			if k := summaryKey(st); k != "" {
				cnt[k]++
			} else if st == "Week off" || st == "Holiday" {
				cnt[st]++
			}
		}
		rows = append(rows, map[string]any{"id": e.ID, "name": e.Name, "type": e.Type, "location": e.Location, "counts": cnt})
	}
	return rows, nil
}

// attendanceIssues lists missing punches and no-punch days that need regularizing.
func (s *Server) attendanceIssues(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.records", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	ym := or(r.URL.Query().Get("ym"), today()[:7])
	from, to, err := monthRange(ym)
	if err != nil {
		return nil, err
	}
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	emps, err := scopedEmps(ctx, s.st.DB, c, bson.D{{Key: "status", Value: "Active"}})
	if err != nil {
		return nil, err
	}
	m, err := loadMonth(ctx, s.st.DB, ts, ids(emps), from, to)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, e := range emps {
		p, _ := ts.policyFor(e)
		for d := from; d <= to && d < today(); d = policy.AddDays(d, 1) {
			if d < e.DOJ || m.leaveOn(e, d, false) != nil {
				continue
			}
			st, tone := m.status(e, p, d)
			if !strings.Contains(st, "Missing") && st != "No punch" && !strings.HasPrefix(st, "Absent") {
				continue // Absent (short hours) needs a correction too
			}
			row := map[string]any{"id": e.ID, "name": e.Name, "date": d, "status": st, "tone": tone, "planned": m.planned(e, p, d).Code}
			if rec := m.rec(e, p, d); rec != nil {
				row["in"], row["out"], row["src"] = rec.In, rec.Out, rec.Src
			}
			_, _, last := p.S.RegDeadline(d)
			row["deadline"] = last
			if rid := m.regs[e.ID][d]; rid != "" {
				row["regPending"] = rid
			}
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["date"].(string) > out[j]["date"].(string) })
	if len(out) > 500 {
		out = out[:500]
	}
	return out, nil
}

// attendanceDay is the date view: everyone in scope on one date.
func (s *Server) attendanceDay(r *http.Request, c *Ctx) (any, error) {
	if r.URL.Query().Get("from") != "" { // a period for one person (myview.go)
		return s.attendancePeriod(r, c)
	}
	if err := c.Need("att.records", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	d := or(r.URL.Query().Get("date"), policy.AddDays(today(), -1))
	if !isDate(d) {
		return nil, bad("Give a date")
	}
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	// Everyone employed on the date (joined by then, not left before it), optionally at one property.
	f := bson.D{{Key: "status", Value: bson.D{{Key: "$ne", Value: "Pre-boarding"}}}, {Key: "doj", Value: bson.D{{Key: "$lte", Value: d}}},
		{Key: "$or", Value: bson.A{bson.D{{Key: "lwd", Value: nil}}, bson.D{{Key: "lwd", Value: bson.D{{Key: "$gte", Value: d}}}}}}}
	if loc := r.URL.Query().Get("location"); loc != "" {
		f = append(f, bson.E{Key: "location_id", Value: loc})
	}
	emps, err := scopedEmps(ctx, s.st.DB, c, f)
	if err != nil {
		return nil, err
	}
	m, err := loadMonth(ctx, s.st.DB, ts, ids(emps), d, d)
	if err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	counts := map[string]int{}
	for _, e := range emps {
		p, _ := ts.policyFor(e)
		rows = append(rows, attDayRow(m, e, p, d, counts))
	}
	return map[string]any{"date": d, "rows": rows, "counts": counts}, nil
}

// attDayRow is one person's day in the By date list (planned shift, punches, source, status), counted in counts.
func attDayRow(m *monthData, e *emp, p *workPolicy, d string, counts map[string]int) map[string]any {
	plan := m.planned(e, p, d)
	row := map[string]any{"id": e.ID, "name": e.Name, "location": e.Location, "department": e.Dept, "planned": plan.Code, "type": e.Type, "city_id": e.CityID}
	if rec := m.rec(e, p, d); rec != nil && d <= today() {
		row["in"], row["out"], row["hours"], row["src"] = rec.In, rec.Out, hoursText(rec), rec.Src
	}
	if cat := m.attCategory(e, p, d); cat != "" {
		row["cat"] = cat // the summary's category (prototype attDay)
	}
	if l := m.leaveOn(e, d, false); l != nil {
		row["status"], row["tone"] = l.Type, "info"
		counts["On leave"]++
	} else if d > today() {
		row["status"], row["tone"] = "Upcoming", "n"
	} else {
		st, tone := m.status(e, p, d)
		row["status"], row["tone"] = st, tone
		k := summaryKey(st)
		if k == "" {
			k = st
		}
		counts[k]++
	}
	return row
}

// attendanceMonth is the month register with loss of pay (finalAtt), for self-service and payroll.
func (s *Server) attendanceMonth(r *http.Request, c *Ctx) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if id == "me" {
		id = c.EmployeeID
	}
	if id != c.EmployeeID {
		if err := c.Need("att.records", 1); err != nil {
			return nil, err
		}
		if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
			return nil, err
		}
	}
	ym := or(r.URL.Query().Get("ym"), today()[:7])
	from, to, err := monthRange(ym)
	if err != nil {
		return nil, err
	}
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	e, err := loadEmp(ctx, s.st.DB, id)
	if err != nil {
		return nil, err
	}
	m, err := loadMonth(ctx, s.st.DB, ts, []string{id}, from, to)
	if err != nil {
		return nil, err
	}
	p, _ := ts.policyFor(e)
	mo := m.month(e, p, ym)
	pend := []string{}
	for d, rid := range m.regs[id] {
		pend = append(pend, d+" "+rid)
	}
	for _, l := range m.leaves[id] {
		if l.Status == "Pending" {
			pend = append(pend, l.From+" "+l.ID)
		}
	}
	return map[string]any{"month": mo, "policy": p.Name, "pending": sortStrings(pend), "lateN": p.S.Att.LateN, "lateAct": p.S.Att.LateAct}, nil
}

// ---------- Deviations (spec 11.5) ----------

func (s *Server) deviationList(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("ros.roster", 1); err != nil {
		return nil, err
	}
	ctx, qv := r.Context(), r.URL.Query()
	to := or(qv.Get("to"), today())
	from := or(qv.Get("from"), policy.AddDays(to, -13))
	if !isDate(from) || !isDate(to) || to < from || policy.DayDiff(from, to) > 62 {
		return nil, bad("Choose a range of up to two months")
	}
	want, q := qv.Get("status"), strings.ToLower(qv.Get("q"))
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	emps, err := scopedEmps(ctx, s.st.DB, c, bson.D{{Key: "status", Value: bson.D{{Key: "$ne", Value: "Pre-boarding"}}}})
	if err != nil {
		return nil, err
	}
	m, err := loadMonth(ctx, s.st.DB, ts, ids(emps), from, to)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	pending := 0
	for _, e := range emps {
		if q != "" && !strings.Contains(strings.ToLower(e.Name+" "+e.ID), q) {
			continue
		}
		p, _ := ts.policyFor(e)
		for d := from; d <= to && d <= today(); d = policy.AddDays(d, 1) {
			if d < e.DOJ {
				continue
			}
			v, x := m.deviation(e, p, d)
			if v == nil {
				continue
			}
			st := "Pending"
			row := map[string]any{"employeeId": e.ID, "name": e.Name, "location": e.Location, "date": d, "dev": v}
			if x != nil {
				st = x.Status
				row["by"], row["at"], row["note"] = x.By, x.At, x.Note
			} else {
				pending++
				row["overdue"] = policy.DayDiff(d, today()) > p.S.RS.DevWin
			}
			row["status"] = st
			if want != "" && want != st {
				continue
			}
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i]["date"] != out[j]["date"] {
			return out[i]["date"].(string) > out[j]["date"].(string)
		}
		return out[i]["name"].(string) < out[j]["name"].(string)
	})
	if len(out) > 1000 {
		out = out[:1000]
	}
	return map[string]any{"rows": out, "pending": pending, "from": from, "to": to}, nil
}

// deviationDecide marks deviations OK or Not OK. OK regularizes the roster to the shift actually worked.
func (s *Server) deviationDecide(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("ros.publish", 2); err != nil {
		return nil, err
	}
	var b struct {
		Items []struct {
			EmployeeID string `json:"employeeId"`
			Date       string `json:"date"`
		} `json:"items"`
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Status != "OK" && b.Status != "Not OK" {
		return nil, bad("Mark OK or Not OK")
	}
	if len(b.Items) == 0 || len(b.Items) > 500 {
		return nil, bad("Choose up to 500 deviations")
	}
	ctx := r.Context()
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	done, skipped := 0, 0
	err = s.st.Tx(ctx, func(tx store.Q) error {
		done, skipped = 0, 0 // the transaction may run again
		for _, it := range b.Items {
			if it.EmployeeID == c.EmployeeID {
				skipped++ // nobody decides their own deviation
				continue
			}
			if ok, _ := c.InScope(ctx, tx, it.EmployeeID); !ok || !isDate(it.Date) {
				skipped++
				continue
			}
			e, err := loadEmp(ctx, tx, it.EmployeeID)
			if err != nil {
				skipped++
				continue
			}
			m, err := loadMonth(ctx, tx, ts, []string{e.ID}, it.Date, it.Date)
			if err != nil {
				return err
			}
			p, _ := ts.policyFor(e)
			v, x := m.deviation(e, p, it.Date)
			if v == nil || x != nil {
				skipped++
				continue
			}
			if b.Status == "OK" && v.Actual != "Custom" && v.Actual != v.Planned {
				if _, ok := ts.shifts[v.Actual]; ok && containsStr(v.Kinds, "Shift") {
					if _, _, err := s.rosterSet(ctx, tx, c, ts, e, it.Date, v.Actual, "Deviation approved", "deviation"); err != nil {
						return err
					}
				}
			}
			if _, err := tx.C("deviation_decision").Insert(ctx, bson.M{"employee_id": e.ID, "date": it.Date, "planned": v.Planned, "actual": v.Actual, "kinds": v.Kinds,
				"in_time": v.In, "out_time": v.Out, "hours": v.Hours, "status": b.Status, "note": timectxNullIf(b.Note), "by_name": c.Name + " (" + c.Role + ")"}); err != nil {
				return err
			}
			done++
		}
		return audit(ctx, tx, c, fmt.Sprintf("%d deviation%s marked %s", done, map[bool]string{true: "", false: "s"}[done == 1], b.Status), "deviation", "", nil, b)
	})
	return map[string]int{"done": done, "skipped": skipped}, err
}

// myDiscipline is the employee's own roster discipline: score, deviations and consequences.
func (s *Server) myDiscipline(r *http.Request, c *Ctx) (any, error) {
	ctx := r.Context()
	ym := or(r.URL.Query().Get("ym"), today()[:7])
	from, to, err := monthRange(ym)
	if err != nil {
		return nil, err
	}
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	e, err := loadEmp(ctx, s.st.DB, c.EmployeeID)
	if err != nil {
		return nil, err
	}
	m, err := loadMonth(ctx, s.st.DB, ts, []string{e.ID}, from, to)
	if err != nil {
		return nil, err
	}
	p, _ := ts.policyFor(e)
	devs := []map[string]any{}
	no := 0
	for d := from; d <= to && d <= today(); d = policy.AddDays(d, 1) {
		if d < e.DOJ {
			continue
		}
		v, x := m.deviation(e, p, d)
		if v == nil {
			continue
		}
		st := "Waiting for your manager"
		if x != nil {
			st = x.Status
			if x.Status == "Not OK" {
				no++
			}
		}
		devs = append(devs, map[string]any{"date": d, "dev": v, "status": st})
	}
	cons, _ := s.st.DB.C("consequence").Find(ctx, bson.D{{Key: "employee_id", Value: e.ID}, {Key: "status", Value: bson.D{{Key: "$nin", Value: bson.A{"Rejected", "Withdrawn", "Pending approval"}}}}},
		options.Find().SetSort(bson.D{{Key: "_id", Value: -1}}).SetProjection(bson.D{{Key: "kind", Value: 1}, {Key: "seq", Value: 1}, {Key: "days", Value: 1},
			{Key: "reason", Value: 1}, {Key: "status", Value: 1}, {Key: "from_date", Value: 1}, {Key: "to_date", Value: 1}, {Key: "outcome", Value: 1}, {Key: "issued_at", Value: 1}}))
	sc := scoreFor(e, p, ym, no)
	return map[string]any{"ym": ym, "score": sc.Score, "band": sc.Band, "start": sc.Start, "startNote": sc.Note, "points": p.S.RS.Pts,
		"leaveBelow": attend.Scaled(p.S.RS.LeaveBelow, sc.Start), "deviations": devs, "consequences": cons}, nil
}

// discScore is one person's discipline score for a month (spec 11.5): where the month started (100, or
// pro-rata for someone employed only part of it) and what is left after the Not OK deviations.
type discScore struct {
	Start int    `json:"start"`
	Score int    `json:"score"`
	Band  string `json:"band"`
	Note  string `json:"startNote,omitempty"` // why the start is below 100: "joined 21 Nov", "left 10 Nov"
}

func scoreFor(e *emp, p *workPolicy, ym string, notOK int) discScore {
	lwd := ""
	if e.LWD != nil {
		lwd = *e.LWD
	}
	st := attend.StartScore(ym, e.DOJ, lwd)
	sc := attend.Score(st, notOK, p.S.RS.Pts)
	d := discScore{Start: st, Score: sc, Band: attend.Band(sc, st)}
	if st < 100 {
		parts := []string{}
		if e.DOJ > ym+"-01" && e.DOJ[:7] == ym {
			parts = append(parts, "joined "+policy.D(e.DOJ).Format("2 Jan"))
		}
		if lwd != "" && lwd < policy.LastDayOf(ym) && lwd[:7] == ym {
			verb := "leaves "
			if lwd < today() {
				verb = "left "
			}
			parts = append(parts, verb+policy.D(lwd).Format("2 Jan"))
		}
		d.Note = strings.Join(parts, ", ")
	}
	return d
}

// startedAt reads a part month's start for the screens: "Started at 33 (joined 21 Nov)".
func (d discScore) startedAt() string {
	if d.Start >= 100 {
		return ""
	}
	if d.Note == "" {
		return fmt.Sprintf("Started at %d", d.Start)
	}
	return fmt.Sprintf("Started at %d (%s)", d.Start, d.Note)
}

// leaveScoreBlock is why every leave request of the employee is refused this month: the discipline score so
// far is below the policy's limit (rs.leaveBelow, scaled pro-rata like the score). "" when leave is allowed.
func leaveScoreBlock(ctx context.Context, q store.Q, e *emp, p *workPolicy) string {
	if p.S.RS.LeaveBelow <= 0 {
		return ""
	}
	t := today()
	ym := t[:7]
	no, err := q.C("deviation_decision").Count(ctx, bson.D{{Key: "employee_id", Value: e.ID}, {Key: "status", Value: "Not OK"}, {Key: "date", Value: timectxBetween(ym+"-01", t)}})
	if err != nil {
		return ""
	}
	sc := scoreFor(e, p, ym, int(no))
	limit := attend.Scaled(p.S.RS.LeaveBelow, sc.Start)
	if sc.Score >= limit {
		return ""
	}
	msg := fmt.Sprintf("Leave is not allowed while the discipline score is below %d (now %d)", limit, sc.Score)
	if sc.Start < 100 {
		msg += fmt.Sprintf(". %s this month, so the limit of %d is scaled to %d", sc.startedAt(), p.S.RS.LeaveBelow, limit)
	}
	return msg
}
