package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/attend"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// Set shift (Masters › Shifts): a shift's timings, kept as versions by effective date, and where it applies (its
// Department + Location rows, shift_maps.go). The engine uses the version in effect on each date; the shift row mirrors
// the version in effect today (a future version moves onto it when its date arrives, in the maintenance sweep). The
// shift's punch rules and attendance rules were removed (owner, 24 Nov 2026): the work policy decides them. Rights: the
// shift master's (mas.job 2 to change, 1 to read).

type shiftBody struct {
	Code          string       `json:"code"`
	EffectiveFrom string       `json:"effectiveFrom"`
	StartTime     string       `json:"startTime"`
	EndTime       string       `json:"endTime"`
	Start2Time    string       `json:"start2Time"` // a split shift's second part (owner, 24 Nov 2026); both empty for one part
	End2Time      string       `json:"end2Time"`
	BreakMinutes  int          `json:"breakMinutes"`
	Name          string       `json:"name"`
	Maps          []shiftMapIn `json:"maps,omitempty"` // Add shift only: where it applies (shift_maps.go); later rows change on their own
	Rules         attend.Rules `json:"rules"`          // only nextDay: the removed rules are unknown fields, refused
	Note          string       `json:"note"`
	// Preview only.
	EmployeeID string `json:"employeeId,omitempty"`
	From       string `json:"from,omitempty"`
	To         string `json:"to,omitempty"`
}

var reShiftCode = regexp.MustCompile(`^[A-Z0-9][A-Z0-9-]{0,9}$`)

// rosterCodes are the roster's own cell codes (spec 11.1): a shift with one of them could not be told apart on the
// roster or the upload (TIME-010, TIME-011, TIME-012, TIME-042). OT is a cell with other time for the day
// (roster_day.go; no shift had that code when it was added, 24 Nov 2026).
var rosterCodes = map[string]bool{"WO": true, "PH": true, "L": true, "LOP": true, otherCode: true}

// shiftCodeProblem is why a new shift's code cannot be used, or "".
func shiftCodeProblem(code string) string {
	if !reShiftCode.MatchString(code) {
		return "Code: letters and digits, up to 10, for example N2"
	}
	if rosterCodes[strings.ToUpper(code)] {
		return "Code " + strings.ToUpper(code) + " is a roster code (week off, holiday, leave, other time). Choose another"
	}
	return ""
}

// fieldErr is a validation error the form shows on the field.
func fieldErr(field, msg string, a ...any) error {
	return badD(fmt.Sprintf(msg, a...), map[string]string{"field": field})
}

// shiftPolicy is the work policy a shift's rules fall back to on screen and in validation: the one most of the
// shift's active employees follow, else the default policy.
func shiftPolicy(ctx context.Context, q store.Q, ts *timeSet, code string) (*workPolicy, int, error) {
	emps, err := loadEmps(ctx, q, bson.D{{Key: "status", Value: "Active"}, {Key: "shift_code", Value: code}})
	if err != nil {
		return nil, 0, err
	}
	n := map[*workPolicy]int{}
	var best *workPolicy
	for _, e := range emps {
		p, _ := ts.policyFor(e)
		n[p]++
		if best == nil || n[p] > n[best] || (n[p] == n[best] && p.Sort < best.Sort) {
			best = p
		}
	}
	if best == nil {
		best, _ = ts.policyFor(&emp{})
	}
	return best, n[best], nil
}

// policyDefaults names the work policy that decides the shift's attendance rules on screen (the one most of its active
// employees follow) and how many follow it. The shift's own rules were removed (owner, 24 Nov 2026).
func policyDefaults(p *workPolicy, n int) map[string]any {
	return map[string]any{"policy": p.Name, "employees": n}
}

// splitProblem checks a split shift's second part against the first (start st, end en, minutes): both times or
// neither, the second part after the first ends, each part ending after it starts, all on the same day. A split
// shift that ends the next day is refused (owner's choice left to the build, 24 Nov 2026): its parts would need a
// date each. It returns the field and the message, or "" for none.
func splitProblem(st, en int, start2, end2 string, nextDay bool) (string, string) {
	if start2 == "" && end2 == "" {
		return "", ""
	}
	if start2 == "" || !reTime.MatchString(start2) {
		return "start2Time", "Second part start must be HH:MM, for example 15:00. Leave both second part times empty for a shift in one part"
	}
	if end2 == "" || !reTime.MatchString(end2) {
		return "end2Time", "Second part end must be HH:MM, for example 19:00. Leave both second part times empty for a shift in one part"
	}
	s2, _ := attend.Min(start2)
	e2, _ := attend.Min(end2)
	switch {
	case nextDay || en <= st:
		return "end2Time", "A split shift must start and end on the same day: the first part must end after it starts, and Shift ending next day must be No"
	case s2 <= en:
		return "start2Time", fmt.Sprintf("The second part must start after the first part ends (%s)", attend.HHMM(en))
	case e2 <= s2:
		return "end2Time", "The second part must end after it starts, on the same day (a split shift cannot run past midnight)"
	}
	return "", ""
}

// validateShift checks a Set shift body: its timings (the rules hold only whether it ends the next day).
func validateShift(b *shiftBody, create bool) error {
	b.Name, b.Code, b.Note = strings.TrimSpace(b.Name), strings.ToUpper(strings.TrimSpace(b.Code)), strings.TrimSpace(b.Note)
	if b.Name == "" {
		return fieldErr("name", "Enter the shift name")
	}
	if create && !reShiftCode.MatchString(b.Code) {
		return fieldErr("code", "Enter a short code: letters and digits, up to 10, for example N2")
	}
	if create && rosterCodes[strings.ToUpper(b.Code)] {
		return fieldErr("code", "%s", shiftCodeProblem(b.Code))
	}
	if b.EffectiveFrom == "" && !create {
		return fieldErr("effectiveFrom", "Enter the shift effective date")
	}
	if b.EffectiveFrom != "" && !isDate(b.EffectiveFrom) {
		return fieldErr("effectiveFrom", "Shift effective date must be a date")
	}
	if !reTime.MatchString(b.StartTime) {
		return fieldErr("startTime", "Start time must be HH:MM, for example 22:00")
	}
	if !reTime.MatchString(b.EndTime) {
		return fieldErr("endTime", "End time must be HH:MM, for example 06:00")
	}
	st, _ := attend.Min(b.StartTime)
	en, _ := attend.Min(b.EndTime)
	if st == en {
		return fieldErr("endTime", "Start and end time cannot be the same")
	}
	b.Start2Time, b.End2Time = strings.TrimSpace(b.Start2Time), strings.TrimSpace(b.End2Time)
	if k, msg := splitProblem(st, en, b.Start2Time, b.End2Time, b.Rules.EndsNextDay()); k != "" {
		return fieldErr(k, "%s", msg)
	}
	length := en - st
	if length <= 0 {
		length += 1440
	}
	if b.Start2Time != "" { // the working length: both parts, without the gap
		s2, _ := attend.Min(b.Start2Time)
		e2, _ := attend.Min(b.End2Time)
		length += e2 - s2
	}
	if b.BreakMinutes < 0 || b.BreakMinutes >= length {
		return fieldErr("breakMinutes", "Break minutes must be 0 or more and shorter than the shift")
	}
	if r := b.Rules; r.NextDay != nil {
		if *r.NextDay && en > st {
			return fieldErr("nextDay", "Shift ending next day is Yes, so the end time must be at or before the start time (for example 22:00 to 06:00)")
		}
		if !*r.NextDay && en <= st {
			return fieldErr("nextDay", "Shift ending next day is No, so the end time must be after the start time. Choose Yes for a shift that ends the next day")
		}
	}
	return nil
}

// lockedMonth is the first payroll month with locked attendance that a version from `from` would change (it
// runs until the next version, `until`, or for good).
func lockedMonth(ctx context.Context, q store.Q, from, until string) (string, error) {
	r, err := q.C("payroll_month").FindOne(ctx, bson.D{{Key: "$or", Value: shiftsLocked}, {Key: "ym", Value: bson.D{{Key: "$gte", Value: from[:7]}}}},
		options.FindOne().SetSort(bson.D{{Key: "ym", Value: 1}}).SetProjection(bson.D{{Key: "ym", Value: 1}}))
	if err != nil || r == nil {
		return "", err
	}
	// Months only grow, so when the first one starts on or after `until` none is changed.
	if ym := str(r["ym"]); until == "" || ym+"-01" < until {
		return ym, nil
	}
	return "", nil
}

// shiftsLocked matches a payroll month whose attendance is locked: locked, or approved or published.
var shiftsLocked = bson.A{bson.D{{Key: "locked_at", Value: bson.D{{Key: "$ne", Value: nil}}}}, bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{"Approved", "Published"}}}}}}

// ensureBaseVersion gives a shift written without versions its first one, from 2000-01-01, with its values.
func ensureBaseVersion(ctx context.Context, q store.Q, shiftID string) error {
	if has, err := q.C("shift_version").Exists(ctx, bson.D{{Key: "shift_id", Value: shiftID}}); err != nil || has {
		return err
	}
	sh, err := q.C("shift").FindOne(ctx, bson.D{{Key: "_id", Value: shiftID}})
	if err != nil || sh == nil {
		return err
	}
	_, err = q.C("shift_version").Upsert(ctx, bson.D{{Key: "shift_id", Value: shiftID}, {Key: "effective_from", Value: "2000-01-01"}}, nil,
		bson.M{"start_time": sh["start_time"], "end_time": sh["end_time"], "start2_time": sh["start2_time"], "end2_time": sh["end2_time"],
			"break_minutes": sh["break_minutes"], "rules": sh["rules"], "created_by": "System", "note": "Shift as it stood before shift versions"})
	return err
}

// ApplyShiftVersions mirrors the version in effect today onto each shift row (a future version moves on when its
// date arrives). With shiftID it does one shift. It returns the shifts changed.
func ApplyShiftVersions(ctx context.Context, q store.Q, shiftID string) (int64, error) {
	match := bson.D{{Key: "effective_from", Value: bson.D{{Key: "$lte", Value: store.DateOf(q.Now())}}}}
	if shiftID != "" {
		match = append(match, bson.E{Key: "shift_id", Value: shiftID})
	}
	// Each shift's latest version from today or earlier.
	vers, err := q.C("shift_version").Aggregate(ctx, bson.A{
		bson.D{{Key: "$match", Value: match}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "shift_id", Value: 1}, {Key: "effective_from", Value: -1}}}},
		bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$shift_id"}, {Key: "start_time", Value: bson.D{{Key: "$first", Value: "$start_time"}}},
			{Key: "end_time", Value: bson.D{{Key: "$first", Value: "$end_time"}}}, {Key: "break_minutes", Value: bson.D{{Key: "$first", Value: "$break_minutes"}}},
			{Key: "start2_time", Value: bson.D{{Key: "$first", Value: "$start2_time"}}}, {Key: "end2_time", Value: bson.D{{Key: "$first", Value: "$end2_time"}}},
			{Key: "rules", Value: bson.D{{Key: "$first", Value: "$rules"}}}}}},
	})
	if err != nil {
		return 0, err
	}
	ids := []string{}
	for _, v := range vers {
		ids = append(ids, str(v["id"]))
	}
	shifts, err := timectxByID(ctx, q, "shift", ids, "start_time", "end_time", "start2_time", "end2_time", "break_minutes", "rules")
	if err != nil {
		return 0, err
	}
	var n int64
	for _, v := range vers {
		sh := shifts[str(v["id"])]
		if sh == nil {
			continue
		}
		if sh["start_time"] == v["start_time"] && sh["end_time"] == v["end_time"] && sh["start2_time"] == v["start2_time"] && sh["end2_time"] == v["end2_time"] &&
			timectxInt(sh["break_minutes"]) == timectxInt(v["break_minutes"]) && reflect.DeepEqual(sh["rules"], v["rules"]) {
			continue
		}
		m, err := q.C("shift").Update(ctx, bson.D{{Key: "_id", Value: sh["id"]}},
			bson.M{"$set": bson.M{"start_time": v["start_time"], "end_time": v["end_time"], "start2_time": v["start2_time"], "end2_time": v["end2_time"],
				"break_minutes": v["break_minutes"], "rules": v["rules"]}})
		if err != nil {
			return n, err
		}
		n += m
	}
	return n, nil
}

// shiftView is the shift with its versions (newest first), the version in effect today and the policy defaults.
func shiftView(ctx context.Context, q store.Q, id string) (map[string]any, error) {
	sh, err := q.C("shift").FindOne(ctx, bson.D{{Key: "_id", Value: id}})
	if err != nil {
		return nil, err
	}
	if sh == nil {
		return nil, notFound("Shift not found")
	}
	row := store.Row{"id": sh["id"], "code": sh["code"], "name": sh["name"], "startTime": sh["start_time"], "endTime": sh["end_time"], "breakMinutes": sh["break_minutes"],
		"start2Time": sh["start2_time"], "end2Time": sh["end2_time"],
		"status": sh["status"], "inactiveSince": sh["inactive_since"], "rules": sh["rules"]}
	vrows, err := q.C("shift_version").Find(ctx, bson.D{{Key: "shift_id", Value: id}}, options.Find().SetSort(bson.D{{Key: "effective_from", Value: -1}}))
	if err != nil {
		return nil, err
	}
	vers := []store.Row{}
	for _, v := range vrows {
		vers = append(vers, store.Row{"id": v["id"], "effectiveFrom": v["effective_from"], "startTime": v["start_time"], "endTime": v["end_time"],
			"start2Time": v["start2_time"], "end2Time": v["end2_time"],
			"breakMinutes": v["break_minutes"], "rules": v["rules"], "createdBy": str(v["created_by"]), "createdAt": v["created_at"], "note": str(v["note"])})
	}
	if len(vers) == 0 {
		vers = []store.Row{{"id": 0, "effectiveFrom": "2000-01-01", "startTime": row["startTime"], "endTime": row["endTime"], "start2Time": row["start2Time"],
			"end2Time": row["end2Time"], "breakMinutes": row["breakMinutes"], "rules": row["rules"], "createdBy": "System", "note": "Shift as it stood before shift versions"}}
	}
	// The removed shift rules (the OT rule, "Actual hours without break", the punch and single punch rules): older
	// versions read without them.
	dropRemovedRules(row["rules"])
	for _, v := range vers {
		dropRemovedRules(v["rules"])
	}
	t := today()
	var cur store.Row
	for _, v := range vers {
		if v["effectiveFrom"].(string) <= t {
			cur = v
			break
		}
	}
	if cur == nil {
		cur = vers[len(vers)-1]
	}
	cur["current"] = true
	ts, err := loadTimeSet(ctx, q)
	if err != nil {
		return nil, err
	}
	p, n, err := shiftPolicy(ctx, q, ts, row["code"].(string))
	if err != nil {
		return nil, err
	}
	locked, _ := punchdaysLastLocked(ctx, q)
	names, err := loadShiftMapNames(ctx, q)
	if err != nil {
		return nil, err
	}
	maps, err := shiftMapRows(ctx, q, id, names)
	if err != nil {
		return nil, err
	}
	hist, err := shiftMapHistory(ctx, q, id)
	if err != nil {
		return nil, err
	}
	outside, err := shiftOutside(ctx, q, id, str(row["code"]))
	if err != nil {
		return nil, err
	}
	out := map[string]any{"shift": row, "versions": vers, "current": cur, "policyDefaults": policyDefaults(p, n), "today": t, "maps": maps, "mapHistory": hist, "outside": outside}
	if locked != "" {
		out["lockedUntil"] = policy.LastDayOf(locked)
	}
	return out, nil
}

func (s *Server) shiftGet(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("mas.job", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	if r.PathValue("id") == "new" { // the Add shift form: the default policy's values
		ts, err := loadTimeSet(ctx, s.st.DB)
		if err != nil {
			return nil, err
		}
		p, _ := ts.policyFor(&emp{})
		return map[string]any{"policyDefaults": policyDefaults(p, 0), "today": today()}, nil
	}
	return shiftView(ctx, s.st.DB, r.PathValue("id"))
}

// windowSig is what decides how a day's punches are grouped; when it changes, past days are rebuilt. limits: some
// work policy sets a maximum early in or late out punch, so any shift's days may be built from its window.
func windowSig(start, end, end2 string, r *attend.Rules, limits bool) string {
	if !r.EndsNextDay() && !limits {
		return ""
	}
	return strings.Join([]string{start, end, end2, fmt.Sprint(r.EndsNextDay())}, "|")
}

// policyPunchLimits: an active work policy sets a maximum early in or late out punch (Attendance › Punching).
func policyPunchLimits(ctx context.Context, q store.Q) (bool, error) {
	return q.C("work_policy").Exists(ctx, bson.D{{Key: "status", Value: "Active"}, {Key: "$or", Value: bson.A{
		bson.D{{Key: "settings.att.earlyInMax", Value: bson.D{{Key: "$gt", Value: 0}}}}, bson.D{{Key: "settings.att.lateOutMax", Value: bson.D{{Key: "$gt", Value: 0}}}}}}})
}

// removedRules are the shift rules that were removed: the OT rule and "Actual hours without break" (8 Nov 2026), the
// punch rules, the single punch rule and the attendance rules (owner, 24 Nov 2026; the work policy decides them).
var removedRules = []string{"otAbove", "withoutBreak", "startFrom", "endFrom", "addMissingOut", "forcedOut", "earlyInMax", "lateOutMax", "officeHours",
	"shiftIn", "singlePunch", "maxHours", "basedOn", "absentBelow", "halfBelow", "fullAbove", "weeklyAdj", "monthlyAdj", "shortfall"}

// dropRemovedRules takes the removed shift rules out of a stored rules object.
func dropRemovedRules(v any) {
	if m, ok := v.(map[string]any); ok {
		for _, k := range removedRules {
			delete(m, k)
		}
	}
}

// saveShiftVersion writes the version from b.EffectiveFrom (replacing one on the same date), mirrors the row
// and rebuilds the punch-machine days when the punch window changed. It returns the version before and after.
func saveShiftVersion(ctx context.Context, tx store.Q, c *Ctx, shiftID string, b *shiftBody) error {
	if err := ensureBaseVersion(ctx, tx, shiftID); err != nil {
		return err
	}
	// The version in effect on the date before this save, and the next version after it.
	var oldStart, oldEnd, oldEnd2, until string
	var oldRaw []byte
	of := bson.D{{Key: "shift_id", Value: shiftID}}
	if v, _ := tx.C("shift_version").FindOne(ctx, append(of, bson.E{Key: "effective_from", Value: bson.D{{Key: "$lte", Value: b.EffectiveFrom}}}),
		options.FindOne().SetSort(bson.D{{Key: "effective_from", Value: -1}})); v != nil {
		oldStart, oldEnd, oldEnd2, oldRaw = str(v["start_time"]), str(v["end_time"]), str(v["end2_time"]), timectxJSON(v["rules"])
	}
	if v, _ := tx.C("shift_version").FindOne(ctx, append(of, bson.E{Key: "effective_from", Value: bson.D{{Key: "$gt", Value: b.EffectiveFrom}}}),
		options.FindOne().SetSort(bson.D{{Key: "effective_from", Value: 1}})); v != nil {
		until = str(v["effective_from"])
	}
	ym, err := lockedMonth(ctx, tx, b.EffectiveFrom, until)
	if err != nil {
		return err
	}
	if ym != "" {
		return fieldErr("effectiveFrom", "Attendance for %s is locked by payroll, so the shift cannot change from %s. Choose a date after %s", ymLabel(ym), longDate(b.EffectiveFrom), ymLabel(ym))
	}
	rules, _ := json.Marshal(b.Rules)
	by := c.Name + " (" + c.EmployeeID + ")"
	// A version on the same date is replaced.
	if _, err := tx.C("shift_version").Upsert(ctx, bson.D{{Key: "shift_id", Value: shiftID}, {Key: "effective_from", Value: b.EffectiveFrom}},
		bson.M{"start_time": b.StartTime, "end_time": b.EndTime, "start2_time": timectxNullIf(b.Start2Time), "end2_time": timectxNullIf(b.End2Time),
			"break_minutes": b.BreakMinutes, "rules": json.RawMessage(rules), "created_by": by, "created_at": tx.Now(), "note": timectxNullIf(b.Note)}, nil); err != nil {
		return err
	}
	if _, err := ApplyShiftVersions(ctx, tx, shiftID); err != nil {
		return err
	}
	limits, err := policyPunchLimits(ctx, tx)
	if err != nil {
		return err
	}
	if windowSig(oldStart, oldEnd, oldEnd2, parseRules(oldRaw), limits) != windowSig(b.StartTime, b.EndTime, b.End2Time, &b.Rules, limits) {
		return rebuildForShift(ctx, tx, b.EffectiveFrom)
	}
	return nil
}

func (s *Server) shiftPut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("mas.job", 2); err != nil {
		return nil, err
	}
	var b shiftBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	err := s.st.Tx(ctx, func(tx store.Q) error {
		before, err := shiftView(ctx, tx, id)
		if err != nil {
			return err
		}
		sh := before["shift"].(store.Row)
		if b.Code != "" && !strings.EqualFold(strings.TrimSpace(b.Code), sh["code"].(string)) {
			return fieldErr("code", "The code of a shift cannot change. Add a new shift instead")
		}
		if b.Maps != nil {
			return fieldErr("maps", "Change where the shift applies with its own rows (Add row, switch on or off, Remove), not with Save")
		}
		if err := validateShift(&b, false); err != nil {
			return err
		}
		if _, err := tx.C("shift").Update(ctx, bson.D{{Key: "_id", Value: id}}, bson.M{"$set": bson.M{"name": b.Name}}); err != nil {
			return err
		}
		if err := saveShiftVersion(ctx, tx, c, fmt.Sprint(sh["id"]), &b); err != nil {
			return err
		}
		after, err := shiftView(ctx, tx, id)
		if err != nil {
			return err
		}
		return audit(ctx, tx, c, "Changed shift", "shift", fmt.Sprint(sh["id"]), map[string]any{"shift": sh, "current": before["current"]},
			map[string]any{"shift": after["shift"], "version": map[string]any{"effectiveFrom": b.EffectiveFrom, "startTime": b.StartTime, "endTime": b.EndTime,
				"start2Time": b.Start2Time, "end2Time": b.End2Time, "breakMinutes": b.BreakMinutes, "rules": b.Rules, "note": b.Note}})
	})
	if err != nil {
		return nil, err
	}
	return shiftView(ctx, s.st.DB, id)
}

func (s *Server) shiftCreate(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("mas.job", 2); err != nil {
		return nil, err
	}
	var b shiftBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	var id string
	err := s.st.Tx(ctx, func(tx store.Q) error {
		if err := validateShift(&b, true); err != nil {
			return err
		}
		if dup, _ := tx.C("shift").Exists(ctx, bson.D{{Key: "code", Value: b.Code}}); dup {
			return fieldErr("code", "A shift with code %s already exists", b.Code)
		}
		if b.EffectiveFrom == "" {
			b.EffectiveFrom = "2000-01-01"
		}
		rules, _ := json.Marshal(b.Rules)
		v, err := tx.C("shift").Insert(ctx, bson.M{"code": b.Code, "name": b.Name, "start_time": b.StartTime, "end_time": b.EndTime, "break_minutes": b.BreakMinutes,
			"start2_time": timectxNullIf(b.Start2Time), "end2_time": timectxNullIf(b.End2Time), "rules": json.RawMessage(rules)})
		if err != nil {
			return err
		}
		id = str(v)
		if _, err := tx.C("shift_version").Insert(ctx, bson.M{"shift_id": id, "effective_from": b.EffectiveFrom, "start_time": b.StartTime, "end_time": b.EndTime,
			"start2_time": timectxNullIf(b.Start2Time), "end2_time": timectxNullIf(b.End2Time), "break_minutes": b.BreakMinutes, "rules": json.RawMessage(rules),
			"created_by": c.Name + " (" + c.EmployeeID + ")", "note": timectxNullIf(b.Note)}); err != nil {
			return err
		}
		if _, err := createShiftMaps(ctx, tx, c, id, b.Maps); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Added shift", "shift", id, nil, b)
	})
	if err != nil {
		return nil, err
	}
	return shiftView(ctx, s.st.DB, id)
}

// shiftOf is a shift in minutes from its stored times (start2 and end2 empty for a shift in one part), with where it
// applies (nil: everywhere, for a timing that is no shift of the master).
func shiftOf(code, start, end, start2, end2 string, maps []attend.ShiftMap) attend.Shift {
	sh := attend.Shift{Code: code, Maps: maps}
	sh.Start, _ = attend.Min(start)
	sh.End, _ = attend.Min(end)
	if a, ok := attend.Min(start2); ok {
		if b, ok := attend.Min(end2); ok && b > a {
			sh.Start2, sh.End2 = a, b
		}
	}
	return sh
}

// shiftTiming is a stored shift's (or version's) timings as text: "09:00–18:00", or "06:00–10:00 + 15:00–19:00".
func shiftTiming(x store.Row) string {
	p2 := ""
	if a, b := str(x["start2_time"]), str(x["end2_time"]); a != "" && b != "" {
		p2 = a + "–" + b
	}
	return attend.TimingText(str(x["start_time"]), str(x["end_time"]), p2)
}

// shiftPreview shows an employee's days with the shift's rules as they stand and as they would be with the
// body's version, before it is saved. Days built from punches use their stored records.
func (s *Server) shiftPreview(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("mas.job", 1); err != nil {
		return nil, err
	}
	var b shiftBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	view, err := shiftView(ctx, s.st.DB, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	code := view["shift"].(store.Row)["code"].(string)
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	if err := validateShift(&b, false); err != nil {
		return nil, err
	}
	if b.To == "" {
		b.To = today()
	}
	if b.From == "" {
		b.From = b.To[:7] + "-01"
	}
	if !isDate(b.From) || !isDate(b.To) || b.From > b.To || policy.DayDiff(b.From, b.To) > 62 {
		return nil, bad("Give a date range of up to two months")
	}
	if err := c.MustInScope(ctx, s.st.DB, b.EmployeeID); err != nil {
		return nil, err
	}
	e, err := loadEmp(ctx, s.st.DB, b.EmployeeID)
	if err != nil {
		return nil, err
	}
	m, err := loadMonth(ctx, s.st.DB, ts, []string{e.ID}, b.From, b.To)
	if err != nil {
		return nil, err
	}
	ep, _ := ts.policyFor(e)
	old := m.span(e, ep, b.From, b.To)
	// The same days with the new version laid into the time set.
	nv := shiftVer{from: b.EffectiveFrom, sh: shiftOf(code, b.StartTime, b.EndTime, b.Start2Time, b.End2Time, ts.shifts[code].Maps), brk: b.BreakMinutes}
	if !b.Rules.Empty() {
		r := b.Rules
		nv.rules = &r
	}
	vs := []shiftVer{}
	for _, v := range ts.vers[code] {
		if v.from != nv.from {
			vs = append(vs, v)
		}
	}
	vs = append(vs, nv)
	sort.Slice(vs, func(i, j int) bool { return vs[i].from < vs[j].from })
	ts.vers[code] = vs
	nw := m.span(e, ep, b.From, b.To)
	days := []map[string]any{}
	changed := 0
	for i, d := range old.Days {
		n := nw.Days[i]
		row := map[string]any{"date": d.Date, "in": d.In, "out": d.Out, "planned": m.planned(e, ep, d.Date).Code, "old": d.Status, "oldTone": d.Tone, "new": n.Status, "newTone": n.Tone, "note": n.Note}
		if d.Status != n.Status {
			changed++
		}
		days = append(days, row)
	}
	return map[string]any{"employee": map[string]any{"id": e.ID, "name": e.Name, "policy": ep.Name}, "days": days, "changed": changed,
		"old": map[string]any{"lop": old.LOP, "paidDays": old.PaidDays}, "new": map[string]any{"lop": nw.LOP, "paidDays": nw.PaidDays}}, nil
}
