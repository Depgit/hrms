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

// Set shift (Masters › Shifts): a shift's timings and attendance rules, kept as versions by effective date.
// The engine uses the version in effect on each date; the shift row mirrors the version in effect today (a
// future version moves onto it when its date arrives, in the maintenance sweep). Every rule is optional and
// falls back to the employee's work policy. Rights: the shift master's (mas.job 2 to change, 1 to read).

type shiftBody struct {
	Code          string       `json:"code"`
	EffectiveFrom string       `json:"effectiveFrom"`
	StartTime     string       `json:"startTime"`
	EndTime       string       `json:"endTime"`
	BreakMinutes  int          `json:"breakMinutes"`
	Name          string       `json:"name"`
	LocationIDs   []string     `json:"locationIds"` // null = all locations
	Rules         attend.Rules `json:"rules"`
	Note          string       `json:"note"`
	// Preview only.
	EmployeeID string `json:"employeeId,omitempty"`
	From       string `json:"from,omitempty"`
	To         string `json:"to,omitempty"`
}

var reShiftCode = regexp.MustCompile(`^[A-Z0-9][A-Z0-9-]{0,9}$`)

// rosterCodes are the roster's own cell codes (spec 11.1): a shift with one of them could not be told apart on the
// roster or the upload (TIME-010, TIME-011, TIME-012, TIME-042).
var rosterCodes = map[string]bool{"WO": true, "PH": true, "L": true, "LOP": true}

// shiftCodeProblem is why a new shift's code cannot be used, or "".
func shiftCodeProblem(code string) string {
	if !reShiftCode.MatchString(code) {
		return "Code: letters and digits, up to 10, for example N2"
	}
	if rosterCodes[strings.ToUpper(code)] {
		return "Code " + strings.ToUpper(code) + " is a roster code (week off, holiday, leave). Choose another"
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

// policyDefaults are the values each rule falls back to, for the form ("Policy: 4:00").
func policyDefaults(p *workPolicy, n int, sh attend.Shift) map[string]any {
	s := p.S
	hm := attend.HoursText
	start := "In punch"
	if s.RS.EarlyCount != "yes" && s.RS.EarlyIn > 0 {
		start = fmt.Sprintf("In punch; up to %d min early counts from the shift start", s.RS.EarlyIn)
	}
	single := map[string]string{"half": "Present half day", "absent": "Absent full day", "present": "Present full day"}[s.Att.Missing]
	auto := "No (missing out punch)"
	if s.Att.AutoOut == "end" {
		auto = "Yes"
	}
	return map[string]any{
		"policy": p.Name, "employees": n,
		"text": map[string]string{
			"startFrom": start, "endFrom": "Out punch", "addMissingOut": auto, "forcedOut": "Shift end (" + attend.HHMM(sh.End) + ")",
			"earlyInMax": "No limit", "lateOutMax": "No limit", "officeHours": hm(s.FullH), "shiftIn": "Current day", "singlePunch": single,
			"maxHours": "No cap", "basedOn": "Hours", "absentBelow": hm(s.HalfH), "halfBelow": hm(s.FullH), "fullAbove": hm(s.FullH),
			"weeklyAdj": "Off", "monthlyAdj": "Off", "shortfall": "None",
		},
		"value": map[string]any{"addMissingOut": s.Att.AutoOut == "end", "singlePunch": s.Att.Missing, "shiftIn": "current", "basedOn": "hours", "endFrom": "punch",
			"fullH": s.FullH, "halfH": s.HalfH},
	}
}

// validateShift checks a Set shift body. def is the policy the empty rules fall back to (for the threshold order).
func validateShift(b *shiftBody, def *workPolicy, create bool) error {
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
	length := en - st
	if length <= 0 {
		length += 1440
	}
	if b.BreakMinutes < 0 || b.BreakMinutes >= length {
		return fieldErr("breakMinutes", "Break minutes must be 0 or more and shorter than the shift")
	}
	for _, id := range b.LocationIDs {
		if !reUUID.MatchString(id) {
			return fieldErr("locationIds", "Locations must be location ids")
		}
	}
	if b.LocationIDs != nil && len(b.LocationIDs) == 0 {
		return fieldErr("locationIds", "Pick at least one location, or choose all locations")
	}
	r := &b.Rules
	r.Normalize()
	if r.NextDay != nil {
		if *r.NextDay && en > st {
			return fieldErr("nextDay", "Shift ending next day is Yes, so the end time must be at or before the start time (for example 22:00 to 06:00)")
		}
		if !*r.NextDay && en <= st {
			return fieldErr("nextDay", "Shift ending next day is No, so the end time must be after the start time. Choose Yes for a shift that ends the next day")
		}
	}
	enums := []struct {
		key, v, label string
		ok            []string
	}{{"startFrom", r.StartFrom, "Consider shift start from", []string{"shift", "punch"}}, {"endFrom", r.EndFrom, "Consider shift end from", []string{"shift", "punch"}},
		{"shiftIn", r.ShiftIn, "Consider shift in", []string{"current", "previous", "next"}}, {"singlePunch", r.SinglePunch, "Mark single punch as", []string{"absent", "half", "present"}},
		{"basedOn", r.BasedOn, "Attendance based on", []string{"hours", "shift", "punch"}}}
	for _, x := range enums {
		if x.v != "" && !containsStr(x.ok, x.v) {
			return fieldErr(x.key, "%s must be one of: %s", x.label, strings.Join(x.ok, ", "))
		}
	}
	if r.ForcedOut != "" && !reTime.MatchString(r.ForcedOut) {
		return fieldErr("forcedOut", "Forced time out must be a time of day, HH:MM")
	}
	durs := []struct {
		key, v, label string
		max           int
	}{{"earlyInMax", r.EarlyInMax, "Maximum early in punch allowed", 12 * 60}, {"lateOutMax", r.LateOutMax, "Maximum late out punch allowed", 12 * 60},
		{"officeHours", r.OfficeHours, "Office hour time", 24 * 60}, {"maxHours", r.MaxHours, "Max Hrs:mins", 24 * 60}, {"absentBelow", r.AbsentBelow, "Absent rule", 24 * 60},
		{"halfBelow", r.HalfBelow, "Half day rule", 24 * 60}, {"fullAbove", r.FullAbove, "Full day rule", 24 * 60}}
	for _, x := range durs {
		if x.v == "" {
			continue
		}
		m, ok := attend.Dur(x.v)
		if !ok || len(x.v) > 5 {
			return fieldErr(x.key, "%s must be hours and minutes, HH:MM, for example 4:30", x.label)
		}
		if m > x.max {
			return fieldErr(x.key, "%s can be at most %s", x.label, attend.DurText(x.max))
		}
		if (x.key == "officeHours" || x.key == "maxHours") && m == 0 {
			return fieldErr(x.key, "%s must be more than 0:00", x.label)
		}
	}
	for _, a := range []struct {
		key, label string
		v          *attend.Adj
	}{{"weeklyAdj", "Weekly adjustment", r.WeeklyAdj}, {"monthlyAdj", "Monthly adjustment", r.MonthlyAdj}} {
		if a.v == nil {
			continue
		}
		if a.v.Hours = strings.TrimSpace(a.v.Hours); a.v.Hours == "" {
			return fieldErr(a.key, "%s is ticked: enter the hours it can cover, HHH:MM, for example 4:00", a.label)
		}
		if _, ok := attend.Dur(a.v.Hours); !ok {
			return fieldErr(a.key, "%s must be hours and minutes, HHH:MM, for example 12:00", a.label)
		}
	}
	// Absent ≤ half day ≤ full day, with the policy's values where the shift leaves a rule empty.
	type th struct {
		key, label string
		h          float64
		own        bool
	}
	val := func(key, label, v string, fallback float64) th {
		if m, ok := attend.Dur(v); ok {
			return th{key, label, float64(m) / 60, true}
		}
		return th{key, label, fallback, false}
	}
	ab := val("absentBelow", "Absent rule", r.AbsentBelow, def.S.HalfH)
	hf := val("halfBelow", "Half day rule", r.HalfBelow, def.S.FullH)
	fu := val("fullAbove", "Full day rule", r.FullAbove, def.S.FullH)
	show := func(t th) string {
		if t.own {
			return attend.HoursText(t.h)
		}
		return attend.HoursText(t.h) + " from the work policy"
	}
	for _, pair := range [][2]th{{ab, hf}, {hf, fu}} {
		if pair[0].h > pair[1].h+1e-9 {
			k := pair[0].key
			if !pair[0].own {
				k = pair[1].key
			}
			return fieldErr(k, "%s (%s) must not be above the %s (%s). Absent ≤ half day ≤ full day", pair[0].label, show(pair[0]), strings.ToLower(pair[1].label[:1])+pair[1].label[1:], show(pair[1]))
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
		bson.M{"start_time": sh["start_time"], "end_time": sh["end_time"], "break_minutes": sh["break_minutes"], "rules": sh["rules"],
			"created_by": "System", "note": "Shift as it stood before shift versions"})
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
			{Key: "rules", Value: bson.D{{Key: "$first", Value: "$rules"}}}}}},
	})
	if err != nil {
		return 0, err
	}
	ids := []string{}
	for _, v := range vers {
		ids = append(ids, str(v["id"]))
	}
	shifts, err := timectxByID(ctx, q, "shift", ids, "start_time", "end_time", "break_minutes", "rules")
	if err != nil {
		return 0, err
	}
	var n int64
	for _, v := range vers {
		sh := shifts[str(v["id"])]
		if sh == nil {
			continue
		}
		if sh["start_time"] == v["start_time"] && sh["end_time"] == v["end_time"] && timectxInt(sh["break_minutes"]) == timectxInt(v["break_minutes"]) &&
			reflect.DeepEqual(sh["rules"], v["rules"]) {
			continue
		}
		m, err := q.C("shift").Update(ctx, bson.D{{Key: "_id", Value: sh["id"]}},
			bson.M{"$set": bson.M{"start_time": v["start_time"], "end_time": v["end_time"], "break_minutes": v["break_minutes"], "rules": v["rules"]}})
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
		"locationIds": sh["location_ids"], "status": sh["status"], "inactiveSince": sh["inactive_since"], "rules": sh["rules"]}
	vrows, err := q.C("shift_version").Find(ctx, bson.D{{Key: "shift_id", Value: id}}, options.Find().SetSort(bson.D{{Key: "effective_from", Value: -1}}))
	if err != nil {
		return nil, err
	}
	vers := []store.Row{}
	for _, v := range vrows {
		vers = append(vers, store.Row{"id": v["id"], "effectiveFrom": v["effective_from"], "startTime": v["start_time"], "endTime": v["end_time"],
			"breakMinutes": v["break_minutes"], "rules": v["rules"], "createdBy": str(v["created_by"]), "createdAt": v["created_at"], "note": str(v["note"])})
	}
	if len(vers) == 0 {
		vers = []store.Row{{"id": 0, "effectiveFrom": "2000-01-01", "startTime": row["startTime"], "endTime": row["endTime"], "breakMinutes": row["breakMinutes"],
			"rules": row["rules"], "createdBy": "System", "note": "Shift as it stood before shift versions"}}
	}
	// The OT rule and "Actual hours without break" were removed: older versions read without them.
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
	a, _ := attend.Min(fmt.Sprint(cur["startTime"]))
	b, _ := attend.Min(fmt.Sprint(cur["endTime"]))
	locked, _ := punchdaysLastLocked(ctx, q)
	out := map[string]any{"shift": row, "versions": vers, "current": cur, "policyDefaults": policyDefaults(p, n, attend.Shift{Start: a, End: b}), "today": t}
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
		return map[string]any{"policyDefaults": policyDefaults(p, 0, attend.Shift{Start: 540, End: 1080}), "today": today()}, nil
	}
	return shiftView(ctx, s.st.DB, r.PathValue("id"))
}

// windowSig is what decides how a day's punches are grouped; when it changes, past days are rebuilt.
func windowSig(start, end string, r *attend.Rules) string {
	if !r.Windowed() {
		return ""
	}
	return strings.Join([]string{start, end, fmt.Sprint(r.NextDay != nil && *r.NextDay), r.ShiftIn, r.EarlyInMax, r.LateOutMax}, "|")
}

// dropRemovedRules takes the removed shift rules (otAbove, withoutBreak) out of a stored rules object.
func dropRemovedRules(v any) {
	if m, ok := v.(map[string]any); ok {
		delete(m, "otAbove")
		delete(m, "withoutBreak")
	}
}

// saveShiftVersion writes the version from b.EffectiveFrom (replacing one on the same date), mirrors the row
// and rebuilds the punch-machine days when the punch window changed. It returns the version before and after.
func saveShiftVersion(ctx context.Context, tx store.Q, c *Ctx, shiftID string, b *shiftBody) error {
	if err := ensureBaseVersion(ctx, tx, shiftID); err != nil {
		return err
	}
	// The version in effect on the date before this save, and the next version after it.
	var oldStart, oldEnd, until string
	var oldRaw []byte
	of := bson.D{{Key: "shift_id", Value: shiftID}}
	if v, _ := tx.C("shift_version").FindOne(ctx, append(of, bson.E{Key: "effective_from", Value: bson.D{{Key: "$lte", Value: b.EffectiveFrom}}}),
		options.FindOne().SetSort(bson.D{{Key: "effective_from", Value: -1}})); v != nil {
		oldStart, oldEnd, oldRaw = str(v["start_time"]), str(v["end_time"]), timectxJSON(v["rules"])
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
		bson.M{"start_time": b.StartTime, "end_time": b.EndTime, "break_minutes": b.BreakMinutes, "rules": json.RawMessage(rules), "created_by": by,
			"created_at": tx.Now(), "note": timectxNullIf(b.Note)}, nil); err != nil {
		return err
	}
	if _, err := ApplyShiftVersions(ctx, tx, shiftID); err != nil {
		return err
	}
	if windowSig(oldStart, oldEnd, parseRules(oldRaw)) != windowSig(b.StartTime, b.EndTime, &b.Rules) {
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
		ts, err := loadTimeSet(ctx, tx)
		if err != nil {
			return err
		}
		p, _, err := shiftPolicy(ctx, tx, ts, sh["code"].(string))
		if err != nil {
			return err
		}
		if err := validateShift(&b, p, false); err != nil {
			return err
		}
		if _, err := tx.C("shift").Update(ctx, bson.D{{Key: "_id", Value: id}}, bson.M{"$set": bson.M{"name": b.Name, "location_ids": b.LocationIDs}}); err != nil {
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
				"breakMinutes": b.BreakMinutes, "rules": b.Rules, "note": b.Note}})
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
		ts, err := loadTimeSet(ctx, tx)
		if err != nil {
			return err
		}
		def, _ := ts.policyFor(&emp{})
		if err := validateShift(&b, def, true); err != nil {
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
			"location_ids": b.LocationIDs, "rules": json.RawMessage(rules)})
		if err != nil {
			return err
		}
		id = str(v)
		if _, err := tx.C("shift_version").Insert(ctx, bson.M{"shift_id": id, "effective_from": b.EffectiveFrom, "start_time": b.StartTime, "end_time": b.EndTime,
			"break_minutes": b.BreakMinutes, "rules": json.RawMessage(rules), "created_by": c.Name + " (" + c.EmployeeID + ")", "note": timectxNullIf(b.Note)}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Added shift", "shift", id, nil, b)
	})
	if err != nil {
		return nil, err
	}
	return shiftView(ctx, s.st.DB, id)
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
	p, _, err := shiftPolicy(ctx, s.st.DB, ts, code)
	if err != nil {
		return nil, err
	}
	if err := validateShift(&b, p, false); err != nil {
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
	st, _ := attend.Min(b.StartTime)
	en, _ := attend.Min(b.EndTime)
	nv := shiftVer{from: b.EffectiveFrom, sh: attend.Shift{Code: code, Start: st, End: en, Locations: ts.shifts[code].Locations}, brk: b.BreakMinutes}
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
