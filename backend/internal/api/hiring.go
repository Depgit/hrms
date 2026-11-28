package api

// The hiring lifecycle (workforce plan, module 1, decided 16 Nov 2026): candidate (SI-) › offer versions › Offer ID
// (OI-, issued only when HR records the acceptance) › joining day › Joining ID (JI-, a normal employee from day 1:
// punches, attendance and payroll) › approval › Employee ID (EMP-, the record moves to it; JI- stays as an alias).
//
//   - Offer answers recorded by HR: Accepted (OI- issued, the version frozen), Question about the breakup (HR
//     revises: a new version goes for approval and is sent again), Declined (reason). An unanswered offer lapses on
//     its expiry date (the hiring sweep), with a bell reminder before.
//   - Before or at joining: withdrew before the joining date, joining date moved, declined on the day, no-show.
//   - The waiting window: a no-show on the joining day and someone who stops coming in the first days both wait
//     (setting, 7 days by default; extendable per person with a reason; day counter; call log; a daily bell to HR).
//     Back within the window: the case resumes where it stopped. Window over: HR confirms the close (No-show / Early
//     exit), or the sweep closes it when automatic close is on. Saying they quit closes as Early exit at once.
//   - Early exit: paid for the days worked through normal payroll (the record leaves with a last working day), no
//     separation or F&F case, and a rehire flag.
//   - Background check: HR records Pending / Clear / Discrepancy with the uploaded report (document bgvReport); by
//     default anything but Clear holds the Employee ID.
//   - Employment types set to "no Employee ID" (contract staff) get their own series' number at joining, for good.
//   - The status timeline (lifecycle_event): every change, who, when, from → to and why, on the candidate and on the
//     employee; employee.stage (Joining, Probation, Confirmed, On notice, Suspended, Long leave, Absconding) set by HR.
//
// Candidates who joined before this (no ji_at) finish the old way: the Employee ID when the documents are complete.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/jobs"
	"github.com/saltstayz/hrms/internal/store"
)

// ---------- Settings ----------

type hireSettings struct {
	WaitDays      int  `json:"waitDays"`
	WaitAutoClose bool `json:"waitAutoClose"`
	BgvBlocks     bool `json:"bgvBlocks"`
	// BgvOff: the company does no background check (user, 18 Nov): the step is hidden and never holds the Employee ID.
	BgvOff          bool `json:"bgvOff"`
	OfferDays       int  `json:"offerDays"`
	OfferRemindDays int  `json:"offerRemindDays"`
	// The offer letter is released at joining for these departments and employment types (hirefix.go).
	LetterJoinDepts []string `json:"letterJoinDepts"`
	LetterJoinTypes []string `json:"letterJoinTypes"`
}

func loadHireSettings(ctx context.Context, q store.Q) hireSettings {
	h := hireSettings{WaitDays: 7, BgvBlocks: true, OfferDays: 7, OfferRemindDays: 2, LetterJoinDepts: []string{}, LetterJoinTypes: []string{}}
	x, _ := onboardingOne(ctx, q, "onb_setting", 1)
	if x == nil {
		return h
	}
	if l := strs(x["letter_join_depts"]); l != nil {
		h.LetterJoinDepts = l
	}
	if l := strs(x["letter_join_types"]); l != nil {
		h.LetterJoinTypes = l
	}
	if v := toInt(x["wait_days"]); v > 0 {
		h.WaitDays = v
	}
	if v, ok := x["wait_auto_close"].(bool); ok {
		h.WaitAutoClose = v
	}
	if v, ok := x["bgv_blocks"].(bool); ok {
		h.BgvBlocks = v
	}
	if v, ok := x["bgv_off"].(bool); ok && v {
		h.BgvOff, h.BgvBlocks = true, false
	}
	if v := toInt(x["offer_days"]); v > 0 {
		h.OfferDays = v
	}
	if x["offer_remind_days"] != nil {
		h.OfferRemindDays = toInt(x["offer_remind_days"])
	}
	return h
}

func (s *Server) hireSettingsPut(ctx context.Context, c *Ctx, h hireSettings) (any, error) {
	if err := c.Need("onb.setup", 2); err != nil {
		return nil, err
	}
	switch {
	case h.WaitDays < 1 || h.WaitDays > 60:
		return nil, bad("The waiting window is 1 to 60 days")
	case h.OfferDays < 1 || h.OfferDays > 90:
		return nil, bad("An offer stays open 1 to 90 days")
	case h.OfferRemindDays < 0 || h.OfferRemindDays > h.OfferDays:
		return nil, bad("Remind 0 to %d days before the offer lapses", h.OfferDays)
	}
	if h.BgvOff {
		h.BgvBlocks = false
	}
	if h.LetterJoinDepts == nil {
		h.LetterJoinDepts = []string{}
	}
	if h.LetterJoinTypes == nil {
		h.LetterJoinTypes = []string{}
	}
	if err := letterRuleCheck(ctx, s.st.DB, h); err != nil {
		return nil, err
	}
	return map[string]any{"hiring": h}, s.st.Tx(ctx, func(tx store.Q) error {
		before := loadHireSettings(ctx, tx)
		if _, err := tx.C("onb_setting").Update(ctx, bson.D{{Key: "_id", Value: 1}}, bson.D{{Key: "$set", Value: bson.D{{Key: "wait_days", Value: int32(h.WaitDays)},
			{Key: "wait_auto_close", Value: h.WaitAutoClose}, {Key: "bgv_blocks", Value: h.BgvBlocks}, {Key: "bgv_off", Value: h.BgvOff}, {Key: "offer_days", Value: int32(h.OfferDays)},
			{Key: "offer_remind_days", Value: int32(h.OfferRemindDays)}, {Key: "letter_join_depts", Value: h.LetterJoinDepts}, {Key: "letter_join_types", Value: h.LetterJoinTypes},
			{Key: "updated_by", Value: c.Name}, {Key: "updated_at", Value: tx.Now()}}}}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Hiring settings updated", "onb_setting", "1", before, h)
	})
}

// hireView adds the hiring lifecycle to the candidate view: offer versions, the waiting window, the background
// check, the timeline, the settings and what the caller can do.
func hireView(ctx context.Context, q store.Q, c *Ctx, x store.Row) {
	now := today()
	w := waitOf(x["waiting_window"], now)
	x["waitingWindow"] = w
	delete(x, "waiting_window")
	vs := offerVersions(ctx, q, x)
	if c.Level("onb.raise") < 1 && c.Level("onb.pipeline") < 1 { // the breakup only for onboarding (an approver of the shortlist does not)
		for _, v := range vs {
			delete(v, "pay")
		}
	}
	x["offerVersions"] = vs
	x["timeline"] = timelineRows(ctx, q, bson.D{{Key: "candidate_id", Value: x["id"]}})
	hs := loadHireSettings(ctx, q)
	x["hiring"] = hs
	x["newFlow"] = x["ji_at"] != nil
	x["accommodation"] = accomView(ctx, q, x) // the offer's accommodation tag and where it stands (hst_auto.go)
	edit := onbEditor(c)
	accepted := x["stage"] == "Offer" && x["accepted_at"] != nil && x["declined_at"] == nil
	joinedNew := x["stage"] == "Joined" && x["ji_at"] != nil && x["approved_at"] == nil
	sent := x["stage"] == "Offer" && x["released_at"] != nil && x["accepted_at"] == nil && x["declined_at"] == nil && x["lapsed_at"] == nil
	can, _ := x["can"].(map[string]bool)
	if can == nil {
		can = map[string]bool{}
	}
	can["answer"] = edit && sent && x["query_at"] == nil
	can["accept"] = edit && sent
	oaj := oajOpen(x) // offer at joining (offer_at_joining.go): the joining outcomes apply before the offer too
	can["outcome"] = edit && (accepted || oaj) && w == nil
	doj := str(x["doj"])
	if oaj {
		doj = oajDOJ(x)
		x["offerAtJoining"] = map[string]any{"doj": doj, "note": x["oaj_note"], "by": x["oaj_by"], "at": x["oaj_at"], "waitingFor": oajReady(x),
			"raise": edit && c.Level("onb.raise") >= 2 && !oajOfferLive(x), "due": now >= doj}
	}
	can["noShow"] = can["outcome"] && now >= doj
	can["wait"] = edit && w != nil
	can["closeWait"] = edit && w != nil && w.Over
	can["earlyExit"] = edit && joinedNew && w == nil
	can["bgv"] = edit && (accepted || joinedNew)
	can["markJoined"] = can["markJoined"] && w == nil // while waiting: Came, mark joined (the waiting panel)
	x["can"] = can
	if x["stage"] == "Joined" && x["ji_at"] != nil {
		blocked := hs.BgvBlocks && bgvStatus(x) != "Clear"
		x["approval"] = map[string]any{"verified": x["verified_at"] != nil, "bgv": bgvStatus(x), "bgvBlocks": hs.BgvBlocks, "blocked": blocked, "approved": x["approved_at"] != nil}
	}
}

// noEmpIDType: employees of this type never get an Employee ID (Employees › Employee codes); they keep the number
// of their type's own series from joining.
func noEmpIDType(ctx context.Context, q store.Q, typ string) bool {
	x, _ := q.C("employee_code_settings").FindOne(ctx, bson.M{"_id": 1})
	for _, t := range strs(onboardingCol(x, "no_emp_id")) {
		if t == typ {
			return true
		}
	}
	return false
}

// seriesCode takes the next code of one series (key: All or an employment type), skipping codes already used.
func seriesCode(ctx context.Context, tx store.Q, key string) (string, error) {
	series, err := tx.C("employee_code_series").Lock(ctx, bson.M{"key": key})
	if err != nil {
		return "", err
	}
	if series == nil {
		return "", bad("No employee code series for %s. Set it in Employees › Employee codes", key)
	}
	prefix, pad, n := str(series["prefix"]), employeesInt(series["pad"]), employeesInt(series["next_no"])
	for {
		code := fmt.Sprintf("%s%0*d", prefix, pad, n)
		n++
		taken, err := tx.C("employee").Exists(ctx, bson.M{"$or": bson.A{bson.M{"_id": employeesFold(code)}, bson.M{"alias_ids": code}}})
		if err != nil {
			return "", err
		}
		if !taken {
			_, err := tx.C("employee_code_series").Update(ctx, bson.M{"key": key}, bson.M{"$set": bson.M{"next_no": n}})
			return code, err
		}
	}
}

// ---------- Timeline ----------

type lifeEv struct {
	Subject, Cand, Emp, Action, From, To, Reason, Date string
}

// lifeEvent records one change on the status timeline. c nil is the system (the hiring sweep).
func lifeEvent(ctx context.Context, tx store.Q, c *Ctx, e lifeEv) error {
	by, byID := "System", ""
	if c != nil {
		by, byID = c.Name, c.EmployeeID
	}
	if e.Subject == "" {
		e.Subject = "candidate"
	}
	_, err := tx.C(store.LifecycleEventColl).Insert(ctx, bson.D{{Key: "subject", Value: e.Subject}, {Key: "candidate_id", Value: strPtr(e.Cand)},
		{Key: "employee_id", Value: strPtr(e.Emp)}, {Key: "action", Value: e.Action}, {Key: "from_state", Value: strPtr(e.From)}, {Key: "to_state", Value: strPtr(e.To)},
		{Key: "reason", Value: strPtr(strings.TrimSpace(e.Reason))}, {Key: "on_date", Value: strPtr(e.Date)}, {Key: "by_name", Value: by}, {Key: "by_id", Value: strPtr(byID)}})
	return err
}

func timelineRows(ctx context.Context, q store.Q, filter bson.D) []map[string]any {
	rows, _ := q.C(store.LifecycleEventColl).Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "at", Value: 1}, {Key: "_id", Value: 1}}))
	out := []map[string]any{}
	for _, r := range rows {
		out = append(out, map[string]any{"at": r["at"], "action": r["action"], "from": r["from_state"], "to": r["to_state"], "reason": r["reason"],
			"date": r["on_date"], "by": r["by_name"], "subject": r["subject"], "candidateId": r["candidate_id"], "employeeId": r["employee_id"]})
	}
	return out
}

// ---------- Waiting window ----------

type waitView struct {
	Kind   string           `json:"kind"` // no-show | stopped
	Since  string           `json:"since"`
	Days   int              `json:"days"`
	Total  int              `json:"total"`
	Day    int              `json:"day"`
	Ends   string           `json:"ends"`
	Over   bool             `json:"over"`
	Reason string           `json:"reason"`
	Ext    []map[string]any `json:"ext"`
	Calls  []map[string]any `json:"calls"`
}

func dayAdd(d string, n int) string {
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		return d
	}
	return t.AddDate(0, 0, n).Format("2006-01-02")
}

func dayDiff(a, b string) int {
	x, e1 := time.Parse("2006-01-02", a)
	y, e2 := time.Parse("2006-01-02", b)
	if e1 != nil || e2 != nil {
		return 0
	}
	return int(y.Sub(x).Hours() / 24)
}

func anyMaps(v any) []map[string]any {
	out := []map[string]any{}
	switch l := v.(type) {
	case []any:
		for _, x := range l {
			if m, ok := x.(map[string]any); ok {
				out = append(out, m)
			}
		}
	case bson.A:
		for _, x := range l {
			if m, ok := x.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

// waitOf reads the candidate's waiting window as of the date now; nil when they are not waiting.
func waitOf(v any, now string) *waitView {
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		return nil
	}
	w := &waitView{Kind: str(m["kind"]), Since: str(m["since"]), Days: toInt(m["days"]), Reason: str(m["reason"]), Ext: anyMaps(m["ext"]), Calls: anyMaps(m["calls"])}
	w.Total = w.Days
	for _, e := range w.Ext {
		w.Total += toInt(e["days"])
	}
	w.Day = max(1, dayDiff(w.Since, now)+1)
	w.Ends = dayAdd(w.Since, w.Total-1)
	w.Over = now > w.Ends
	return w
}

func (w *waitView) doc() map[string]any {
	ext, calls := []any{}, []any{}
	for _, e := range w.Ext {
		ext = append(ext, e)
	}
	for _, e := range w.Calls {
		calls = append(calls, e)
	}
	return map[string]any{"kind": w.Kind, "since": w.Since, "days": w.Days, "reason": w.Reason, "ext": ext, "calls": calls}
}

func waitLabel(w *waitView) string {
	what := "No-show"
	if w.Kind == "stopped" {
		what = "Stopped coming"
	}
	if w.Over {
		return fmt.Sprintf("%s: the %d-day window ended on %s. Confirm the close or extend it", what, w.Total, longDate(w.Ends))
	}
	return fmt.Sprintf("%s: waiting, day %d of %d", what, w.Day, w.Total)
}

// ---------- Joining: the Joining ID record ----------

// rejoinKeepsCode: a rejoiner keeps their old code (Employees › Employee codes, or the choice on the shortlist).
func rejoinKeepsCode(ctx context.Context, q store.Q, o store.Row) bool {
	rejoin := str(o["rejoin_of"])
	if rejoin == "" {
		return false
	}
	settings, _ := onboardingOne(ctx, q, "employee_code_settings", 1)
	mode := str(onboardingCol(settings, "rejoin"))
	if cm := rowData(o)["codeMode"]; cm == "same" || cm == "new" {
		mode = cm
	}
	return mode != "new"
}

// hireJoin marks a candidate joined on date and makes their record from the offer: a normal, active employee from
// day 1 under the Joining ID (JI-0042), or under the permanent number for a rejoiner who keeps their code and for a
// type that never gets an Employee ID. It returns the record's id.
func (s *Server) hireJoin(ctx context.Context, tx store.Q, c *Ctx, x store.Row, date string) (string, error) {
	id, no := str(x["id"]), toInt(x["no"])
	o, err := candidateOffer(ctx, tx, id)
	if err != nil {
		return "", err
	}
	ji := stageID("JI", no)
	code := ji
	if !rejoinKeepsCode(ctx, tx, o) {
		if typ := str(o["type"]); noEmpIDType(ctx, tx, typ) {
			if code, err = seriesCode(ctx, tx, typ); err != nil {
				return "", err
			}
		} else if ok, err := tx.C("employee").Exists(ctx, bson.M{"$or": bson.A{bson.M{"_id": ji}, bson.M{"alias_ids": ji}}}); err != nil || ok {
			if err == nil {
				err = bad("%s is already used by an employee record", ji)
			}
			return "", err
		}
	}
	by := "Joining " + ji
	code, _, err = employeeFromOfferWith(ctx, tx, o, date, by, empOpts{Code: code, Status: "Active", Stage: "Joining"})
	if err != nil {
		return "", err
	}
	if err := machineGateJoin(ctx, tx, c, code); err != nil { // the cluster's punch machines are full (machines.go)
		return "", err
	}
	if err := releaseLetterAtJoining(ctx, tx, c, o, id, date); err != nil { // a letter held until joining (hirefix.go)
		return "", err
	}
	if _, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: id}}, bson.D{{Key: "$set", Value: bson.D{{Key: "stage", Value: "Joined"},
		{Key: "joined_on", Value: date}, {Key: "joined_at", Value: tx.Now()}, {Key: "joined_by", Value: c.Name}, {Key: "ji_at", Value: tx.Now()},
		{Key: "employee_id", Value: code}, {Key: "waiting", Value: nil}}}}); err != nil {
		return "", err
	}
	note := ""
	if code != ji {
		note = "Permanent number from joining: " + code
	}
	if err := lifeEvent(ctx, tx, c, lifeEv{Cand: id, Emp: code, Action: "Joined", From: "Offer accepted", To: "Joined (" + ji + ")", Date: date, Reason: note}); err != nil {
		return "", err
	}
	if err := lifeEvent(ctx, tx, c, lifeEv{Subject: "employee", Cand: id, Emp: code, Action: "Record made at joining", To: "Joining", Date: date,
		Reason: "Active from " + longDate(date) + ": punches, attendance and payroll count from day 1"}); err != nil {
		return "", err
	}
	// Company accommodation tagged on the offer: a bed from the joining day, or the waiting list (hst_auto.go).
	if yes, cluster := accomTag(o); yes && cluster != "" {
		if _, _, err := hstAutoAllot(ctx, tx, code, cluster, date, str(o["id"]), true); err != nil {
			return "", err
		}
	}
	// The Realtime portal: created automatically by a job once this commits (portal_auto.go).
	if err := ptWantCreate(ctx, tx, code); err != nil {
		return "", err
	}
	if err := s.ptEnqueueTx(ctx, tx, code); err != nil {
		return "", err
	}
	return code, nil
}

// approveHire issues the Employee ID once every step is approved: joined (not waiting), the joining details and
// documents verified by Payroll, and the background check clear (when the setting makes it block). A Joining-ID
// record moves to the new ID in one transaction (store.RekeyEmployee); the JI- stays as an alias. A rejoiner who
// kept their code and contract staff keep the number they have. It returns the record's id, or "" when it is not
// due yet.
func approveHire(ctx context.Context, tx store.Q, c *Ctx, cand string) (string, error) {
	x, err := tx.C("candidate").Lock(ctx, bson.D{{Key: "_id", Value: cand}})
	if err != nil || x == nil {
		return "", err
	}
	if x["ji_at"] == nil || x["approved_at"] != nil || x["stage"] != "Joined" || x["waiting"] != nil {
		return "", nil
	}
	o, err := tx.C("offer").FindOne(ctx, bson.D{{Key: "candidate_id", Value: cand}})
	if err != nil || o == nil || o["verified_at"] == nil {
		return "", err
	}
	if loadHireSettings(ctx, tx).BgvBlocks && bgvStatus(x) != "Clear" {
		return "", nil
	}
	emp := str(x["employee_id"])
	if emp == "" {
		return "", nil
	}
	code := emp
	if strings.HasPrefix(emp, "JI-") {
		typ := str(o["type"])
		if noEmpIDType(ctx, tx, typ) {
			code, err = seriesCode(ctx, tx, typ)
		} else {
			code, err = NextEmployeeCode(ctx, tx, typ)
		}
		if err != nil {
			return "", err
		}
		if _, err := store.RekeyEmployee(ctx, tx, emp, code); err != nil {
			return "", err
		}
	}
	now := tx.Now()
	day := store.DateOf(now)
	if _, err := tx.C("employee").Update(ctx, bson.D{{Key: "_id", Value: code}}, bson.D{{Key: "$set", Value: bson.D{{Key: "stage", Value: "Probation"}, {Key: "stage_since", Value: day}}}}); err != nil {
		return "", err
	}
	if _, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: cand}}, bson.D{{Key: "$set", Value: bson.D{{Key: "approved_at", Value: now},
		{Key: "employee_created_at", Value: now}}}}); err != nil {
		return "", err
	}
	why := "Every step approved: joining details, Payroll verification" + map[bool]string{true: ", background check", false: ""}[bgvStatus(x) == "Clear"]
	if code != emp {
		why += ". " + emp + " stays as an alias"
	}
	if err := lifeEvent(ctx, tx, c, lifeEv{Cand: cand, Emp: code, Action: "Approved", From: "Joined (" + emp + ")", To: "Employee " + code, Date: day, Reason: why}); err != nil {
		return "", err
	}
	if err := lifeEvent(ctx, tx, c, lifeEv{Subject: "employee", Cand: cand, Emp: code, Action: "Employee ID issued: " + code, From: "Joining", To: "Probation", Date: day, Reason: why}); err != nil {
		return "", err
	}
	return code, audit(ctx, tx, c, fmt.Sprintf("Employee ID %s issued for %s (%s)", code, x["name"], emp), "candidate", cand, map[string]string{"id": emp}, map[string]string{"id": code})
}

func bgvStatus(x store.Row) string {
	if m, ok := x["bgv"].(map[string]any); ok {
		return str(m["status"])
	}
	return ""
}

// ---------- Closing and early exit ----------

// hireClose closes the candidate with one of the hiring kinds; the joiner link stops.
func hireClose(ctx context.Context, tx store.Q, c *Ctx, x store.Row, kind, reason, from string) error {
	by := "System"
	if c != nil {
		by = c.Name
	}
	id := str(x["id"])
	if _, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: id}}, bson.D{{Key: "$set", Value: bson.D{{Key: "stage", Value: "Closed"},
		{Key: "closed_kind", Value: kind}, {Key: "closed_reason", Value: strPtr(strings.TrimSpace(reason))}, {Key: "closed_at", Value: tx.Now()}, {Key: "closed_by", Value: by},
		{Key: "waiting", Value: nil}}}}); err != nil {
		return err
	}
	who := by
	if c != nil {
		who = c.Name + " (" + c.EmployeeID + ")"
	}
	if err := stopJoinerLink(ctx, tx, str(x["offer_id"]), who, "The candidate was closed: "+kind); err != nil {
		return err
	}
	emp := ""
	if str(x["employee_id"]) != "" && x["ji_at"] != nil {
		emp = str(x["employee_id"])
	}
	return lifeEvent(ctx, tx, c, lifeEv{Cand: id, Emp: emp, Action: "Closed: " + kind, From: from, To: "Closed", Reason: reason, Date: store.DateOf(tx.Now())})
}

// earlyExit closes someone who left in the first days: the record leaves with lwd as the last working day (paid
// for the days worked by normal payroll), exit reason Early exit and the rehire flag; no separation or F&F case.
func earlyExit(ctx context.Context, tx store.Q, c *Ctx, x store.Row, lwd, reason, rehire, from string) error {
	emp := str(x["employee_id"])
	if emp == "" {
		return bad("%s has no record to close", x["id"])
	}
	if rehire != "Yes" && rehire != "No" {
		rehire = "Yes"
	}
	set := bson.D{{Key: "lwd", Value: lwd}, {Key: "exit_reason", Value: "Early exit"}, {Key: "rehire", Value: rehire}}
	if lwd < store.DateOf(tx.Now()) {
		set = append(set, bson.E{Key: "status", Value: "Inactive"})
	}
	if _, err := tx.C("employee").Update(ctx, bson.D{{Key: "_id", Value: emp}}, bson.D{{Key: "$set", Value: set}}); err != nil {
		return err
	}
	if err := hireClose(ctx, tx, c, x, "Early exit", reason, from); err != nil {
		return err
	}
	return lifeEvent(ctx, tx, c, lifeEv{Subject: "employee", Cand: str(x["id"]), Emp: emp, Action: "Early exit", From: "Joining", To: "Left",
		Date: lwd, Reason: fmt.Sprintf("%s. Last working day %s; paid for the days worked; rehire: %s", strings.TrimSpace(reason), longDate(lwd), rehire)})
}

// ---------- Candidate actions ----------

type hireEventBody struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
	Date   string `json:"date"`
	Days   int    `json:"days"`
	Rehire string `json:"rehire"`
}

// candidateEvent records a joining outcome or a step of the waiting window (POST /api/candidates/{id}/event).
func (s *Server) candidateEvent(r *http.Request, c *Ctx) (any, error) {
	if !onbEditor(c) {
		return nil, forbidden("")
	}
	var b hireEventBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Reason = strings.TrimSpace(b.Reason)
	if len(b.Reason) > 1000 {
		return nil, bad("Keep the reason under 1,000 characters")
	}
	ctx := r.Context()
	out := map[string]any{}
	err := s.st.Tx(ctx, func(tx store.Q) error {
		clear(out)
		x, err := s.loadCandidate(ctx, tx, c, r.PathValue("id"), true)
		if err != nil {
			return err
		}
		id, name, now := str(x["id"]), str(x["name"]), store.DateOf(tx.Now())
		accepted := x["stage"] == "Offer" && x["accepted_at"] != nil && x["declined_at"] == nil
		joinedNew := x["stage"] == "Joined" && x["ji_at"] != nil && x["approved_at"] == nil
		w := waitOf(x["waiting_window"], now)
		oaj := oajOpen(x) // offer at joining (offer_at_joining.go): the outcomes before joining apply to it too
		doj := str(x["doj"])
		if oaj {
			doj = oajDOJ(x)
		}
		needReason := func() error {
			if b.Reason == "" {
				return bad("Give the reason")
			}
			return nil
		}
		msg := ""
		switch b.Action {
		case "doj":
			if !accepted && !oaj {
				return bad("The joining date can be moved after the offer is accepted and before joining")
			}
			if err := needReason(); err != nil {
				return err
			}
			if !isDate(b.Date) {
				return bad("Enter the new joining date")
			}
			old := doj
			if b.Date == old {
				return bad("The joining date is already %s", longDate(old))
			}
			if released, ok := x["released_at"].(time.Time); ok && b.Date < store.DateOf(released) {
				return bad("The joining date cannot be before the offer was released, %s", longDate(store.DateOf(released)))
			}
			if oaj {
				if b.Date < now {
					return bad("The expected joining date is today or later")
				}
				if _, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: id}}, bson.D{{Key: "$set", Value: bson.D{{Key: "oaj_doj", Value: b.Date}}}}); err != nil {
					return err
				}
			}
			if x["offer_id"] != nil {
				if _, err := tx.C("offer").Update(ctx, bson.D{{Key: "_id", Value: x["offer_id"]}}, bson.D{{Key: "$set", Value: bson.D{{Key: "doj", Value: b.Date}}}}); err != nil {
					return err
				}
			}
			if w != nil { // a new date: no longer a no-show
				if _, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: id}}, bson.D{{Key: "$set", Value: bson.D{{Key: "waiting", Value: nil}}}}); err != nil {
					return err
				}
			}
			msg = fmt.Sprintf("Joining date moved to %s", longDate(b.Date))
			err = lifeEvent(ctx, tx, c, lifeEv{Cand: id, Action: "Joining date moved", From: longDate(old), To: longDate(b.Date), Date: b.Date, Reason: b.Reason})
		case "withdrew", "declined-day":
			if !accepted && !oaj {
				return bad("This is for a candidate who accepted the offer and has not joined")
			}
			if err := needReason(); err != nil {
				return err
			}
			kind := map[string]string{"withdrew": "Withdrew before joining", "declined-day": "Declined on joining day"}[b.Action]
			if b.Action == "declined-day" && now < doj {
				return bad("The joining day is %s. Before it, use Withdrew before joining", longDate(doj))
			}
			if oaj { // an offer at joining still waiting for approval is withdrawn with the candidate
				if err := oajCancelPending(ctx, tx, c, x); err != nil {
					return err
				}
			}
			msg = name + ": " + strings.ToLower(kind[:1]) + kind[1:]
			err = hireClose(ctx, tx, c, x, kind, b.Reason, hireState(x, w))
		case "no-show":
			if (!accepted && !oaj) || w != nil {
				return bad("No-show is for a candidate who accepted the offer, on or after the joining date")
			}
			if now < doj {
				return bad("The joining date is %s. Mark the no-show on or after it", longDate(doj))
			}
			w = &waitView{Kind: "no-show", Since: doj, Days: loadHireSettings(ctx, tx).WaitDays, Reason: b.Reason, Ext: []map[string]any{}, Calls: []map[string]any{}}
			if _, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: id}}, bson.D{{Key: "$set", Value: bson.D{{Key: "waiting", Value: w.doc()}}}}); err != nil {
				return err
			}
			w = waitOf(w.doc(), now)
			msg = fmt.Sprintf("%s is waiting: day %d of %d", name, w.Day, w.Total)
			err = lifeEvent(ctx, tx, c, lifeEv{Cand: id, Action: "No-show on the joining day", From: hireState(x, nil), To: "Waiting", Date: doj, Reason: b.Reason})
		case "stopped":
			if !joinedNew || w != nil {
				return bad("Stopped coming is for someone on a Joining ID who is not waiting already")
			}
			if b.Date == "" {
				b.Date = now
			}
			if !isDate(b.Date) || b.Date > now || b.Date <= str(x["joined_on"]) {
				return bad("Enter the first day they did not come: after the joining date, %s, and not in the future", longDate(str(x["joined_on"])))
			}
			w = &waitView{Kind: "stopped", Since: b.Date, Days: loadHireSettings(ctx, tx).WaitDays, Reason: b.Reason, Ext: []map[string]any{}, Calls: []map[string]any{}}
			if _, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: id}}, bson.D{{Key: "$set", Value: bson.D{{Key: "waiting", Value: w.doc()}}}}); err != nil {
				return err
			}
			w = waitOf(w.doc(), now)
			msg = fmt.Sprintf("%s is waiting: day %d of %d", name, w.Day, w.Total)
			err = lifeEvent(ctx, tx, c, lifeEv{Cand: id, Emp: str(x["employee_id"]), Action: "Stopped coming", From: "Joined", To: "Waiting", Date: b.Date, Reason: b.Reason})
		case "quit":
			if !joinedNew {
				return bad("Quit in the first days is for someone on a Joining ID")
			}
			if err := needReason(); err != nil {
				return err
			}
			if b.Date == "" {
				b.Date = now
			}
			if !isDate(b.Date) || b.Date > now || b.Date < str(x["joined_on"]) {
				return bad("Enter the last working day: from the joining date, %s, to today", longDate(str(x["joined_on"])))
			}
			msg = name + " closed as Early exit"
			err = earlyExit(ctx, tx, c, x, b.Date, b.Reason, b.Rehire, hireState(x, w))
		case "extend":
			if w == nil {
				return bad("%s is not waiting", name)
			}
			if err := needReason(); err != nil {
				return err
			}
			if b.Days < 1 || b.Days > 60 {
				return bad("Extend by 1 to 60 days")
			}
			w.Ext = append(w.Ext, map[string]any{"days": b.Days, "reason": b.Reason, "by": c.Name, "at": tx.Now().Format(time.RFC3339)})
			if _, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: id}}, bson.D{{Key: "$set", Value: bson.D{{Key: "waiting", Value: w.doc()}}}}); err != nil {
				return err
			}
			w = waitOf(w.doc(), now)
			msg = fmt.Sprintf("Window extended: day %d of %d", w.Day, w.Total)
			err = lifeEvent(ctx, tx, c, lifeEv{Cand: id, Action: fmt.Sprintf("Waiting window extended by %d day%s", b.Days, map[bool]string{true: "", false: "s"}[b.Days == 1]),
				From: "Waiting", To: "Waiting until " + longDate(w.Ends), Reason: b.Reason})
		case "call":
			if w == nil {
				return bad("%s is not waiting", name)
			}
			if b.Reason == "" {
				return bad("Say what happened on the call")
			}
			w.Calls = append(w.Calls, map[string]any{"note": b.Reason, "by": c.Name, "at": tx.Now().Format(time.RFC3339)})
			if _, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: id}}, bson.D{{Key: "$set", Value: bson.D{{Key: "waiting", Value: w.doc()}}}}); err != nil {
				return err
			}
			msg = "Call logged"
			err = lifeEvent(ctx, tx, c, lifeEv{Cand: id, Action: "Call attempt", From: "Waiting", To: "Waiting", Reason: b.Reason})
		case "back":
			if w == nil {
				return bad("%s is not waiting", name)
			}
			if b.Date == "" {
				b.Date = now
			}
			if !isDate(b.Date) || b.Date > now || b.Date < w.Since {
				return bad("Enter the day they came: from %s to today", longDate(w.Since))
			}
			if w.Kind == "no-show" { // joined on the actual date: the same offer, documents and link
				if oaj { // offer at joining: only once its offer is approved; joining is the acceptance
					if why := oajReady(x); why != "" {
						return bad("%s", why)
					}
					if err := oajAccept(ctx, tx, c, x, b.Date); err != nil {
						return err
					}
				}
				code, err := s.hireJoin(ctx, tx, c, x, b.Date)
				if err != nil {
					return err
				}
				out["employeeId"] = code
				msg = fmt.Sprintf("%s joined on %s: %s", name, longDate(b.Date), code)
				break
			}
			if _, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: id}}, bson.D{{Key: "$set", Value: bson.D{{Key: "waiting", Value: nil}}}}); err != nil {
				return err
			}
			missed := dayDiff(w.Since, b.Date)
			msg = fmt.Sprintf("%s is back. The process resumes; %d missed day%s count as absent", name, missed, map[bool]string{true: "", false: "s"}[missed == 1])
			err = lifeEvent(ctx, tx, c, lifeEv{Cand: id, Emp: str(x["employee_id"]), Action: "Came back", From: "Waiting", To: "Joined", Date: b.Date,
				Reason: fmt.Sprintf("%d missed day%s from %s count as absent. %s", missed, map[bool]string{true: "", false: "s"}[missed == 1], longDate(w.Since), b.Reason)})
		case "close":
			if w == nil {
				return bad("%s is not waiting", name)
			}
			if !w.Over {
				return bad("The window runs until %s (day %d of %d). Close it after that, or record what happened", longDate(w.Ends), w.Day, w.Total)
			}
			reason := b.Reason
			if reason == "" {
				reason = fmt.Sprintf("Did not come within the %d-day window", w.Total)
			}
			if w.Kind == "no-show" {
				msg = name + " closed as No-show"
				err = hireClose(ctx, tx, c, x, "No-show", reason, "Waiting")
			} else {
				msg = name + " closed as Early exit"
				err = earlyExit(ctx, tx, c, x, stoppedLWD(x, w), reason, b.Rehire, "Waiting")
			}
		default:
			return bad("Unknown action")
		}
		if err != nil {
			return err
		}
		out["result"] = msg
		return audit(ctx, tx, c, fmt.Sprintf("%s (%s): %s", name, currentID(x), msg), "candidate", id, nil, b)
	})
	return out, err
}

// stoppedLWD is the last working day of someone who stopped coming: the day before the first missed day.
func stoppedLWD(x store.Row, w *waitView) string {
	lwd := dayAdd(w.Since, -1)
	if j := str(x["joined_on"]); lwd < j {
		lwd = j
	}
	return lwd
}

func hireState(x store.Row, w *waitView) string {
	if w != nil {
		return "Waiting"
	}
	if x["stage"] == "Offer" && x["accepted_at"] != nil {
		return "Offer accepted"
	}
	if oajOpen(x) {
		return "Offer at joining"
	}
	return str(x["stage"])
}

// candidateBGV records the background check: Pending, Clear or Discrepancy, with the report uploaded as the
// candidate's document bgvReport (POST /api/candidates/{id}/bgv).
func (s *Server) candidateBGV(r *http.Request, c *Ctx) (any, error) {
	if !onbEditor(c) {
		return nil, forbidden("")
	}
	var b struct {
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Note = strings.TrimSpace(b.Note)
	if b.Status != "Pending" && b.Status != "Clear" && b.Status != "Discrepancy" {
		return nil, bad("Choose Pending, Clear or Discrepancy")
	}
	if len(b.Note) > 1000 {
		return nil, bad("Keep the note under 1,000 characters")
	}
	if b.Status == "Discrepancy" && b.Note == "" {
		return nil, bad("Say what the discrepancy is")
	}
	ctx := r.Context()
	out := map[string]any{}
	err := s.st.Tx(ctx, func(tx store.Q) error {
		clear(out)
		x, err := s.loadCandidate(ctx, tx, c, r.PathValue("id"), true)
		if err != nil {
			return err
		}
		id := str(x["id"])
		if x["stage"] == "Closed" || x["approved_at"] != nil || !(x["stage"] == "Joined" || (x["stage"] == "Offer" && x["accepted_at"] != nil)) {
			return bad("The background check is recorded after the offer is accepted, until the Employee ID is issued")
		}
		docs, err := docsOf(ctx, tx, "candidate", id)
		if err != nil {
			return err
		}
		rep := docs["bgvReport"]
		if b.Status != "Pending" && rep == nil {
			return bad("Upload the background check report first")
		}
		before := bgvStatus(x)
		v := map[string]any{"status": b.Status, "note": b.Note, "by": c.Name, "at": tx.Now().Format(time.RFC3339)}
		if rep != nil {
			v["report"] = str(rep["id"])
			v["file"] = str(rep["file_name"])
		}
		if _, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: id}}, bson.D{{Key: "$set", Value: bson.D{{Key: "bgv", Value: v}}}}); err != nil {
			return err
		}
		from := before
		if from == "" {
			from = "Not recorded"
		}
		emp := ""
		if x["ji_at"] != nil {
			emp = str(x["employee_id"])
		}
		if err := lifeEvent(ctx, tx, c, lifeEv{Cand: id, Emp: emp, Action: "Background check: " + b.Status, From: from, To: b.Status, Reason: b.Note}); err != nil {
			return err
		}
		out["status"] = b.Status
		if b.Status == "Clear" {
			code, err := approveHire(ctx, tx, c, id)
			if err != nil {
				return err
			}
			if code != "" {
				out["employeeId"] = code
			}
		}
		return audit(ctx, tx, c, fmt.Sprintf("Background check of %s (%s): %s", x["name"], currentID(x), b.Status), "candidate", id, map[string]string{"status": before}, b)
	})
	return out, err
}

// ---------- Offer versions ----------

// offerVersions lists the candidate's offer versions, the current one with its live status.
func offerVersions(ctx context.Context, q store.Q, x store.Row) []map[string]any {
	rows, _ := q.C(store.OfferVersionColl).Find(ctx, bson.D{{Key: "candidate_id", Value: x["id"]}}, options.Find().SetSort(bson.D{{Key: "version", Value: 1}}))
	cur := toInt(x["offer_version"])
	out := []map[string]any{}
	locs := map[string]string{} // offer id → its location, for versions worked out again
	for _, v := range rows {
		oid := str(v["offer_id"])
		if _, ok := locs[oid]; !ok {
			o, _ := q.C("offer").FindOne(ctx, bson.D{{Key: "_id", Value: oid}})
			locs[oid] = str(o["location_id"])
		}
		st := str(v["status"])
		if toInt(v["version"]) == cur && !(v["frozen"] == true) && st != "Question" && st != "Declined" && st != "Lapsed" {
			st = liveOfferState(x)
		}
		out = append(out, map[string]any{"version": v["version"], "status": st, "gross": v["gross"], "type": v["type"], "doj": v["doj"], "grade": v["grade"],
			"answer": v["answer"], "answeredAt": v["answered_at"], "answeredBy": v["answered_by"], "releasedAt": v["released_at"], "expiresOn": v["expires_on"],
			"frozen": v["frozen"], "createdAt": v["created_at"], "createdBy": v["created_by"], "pay": offerVersionPay(ctx, q, v, locs[oid])})
	}
	return out
}

// liveOfferState is the current version's state from the offer and its approval.
func liveOfferState(x store.Row) string {
	switch {
	case x["accepted_at"] != nil:
		return "Accepted"
	case x["declined_at"] != nil:
		return "Declined"
	case x["lapsed_at"] != nil:
		return "Lapsed"
	case x["query_at"] != nil:
		return "Question"
	case x["offer_status"] == "Rejected" || x["offer_status"] == "Cancelled":
		return "Not approved"
	case x["released_at"] != nil:
		return "Sent"
	case x["offer_status"] == "Approved":
		return "Approved"
	}
	return "Pending approval"
}

// offerVersionClose ends the version being replaced by a revised offer: Revised after a question, else Not approved.
// An offer raised before versions existed gets its snapshot now, so the history starts at v1.
func offerVersionClose(ctx context.Context, tx store.Q, c *Ctx, prev store.Row) error {
	cand := str(prev["candidate_id"])
	ver := max(1, toInt(prev["version"]))
	st := "Not approved"
	if prev["query_at"] != nil {
		st = "Revised"
	}
	n, err := tx.C(store.OfferVersionColl).Update(ctx, bson.D{{Key: "candidate_id", Value: cand}, {Key: "version", Value: int32(ver)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: st}}}})
	if err != nil || n > 0 {
		return err
	}
	_, err = tx.C(store.OfferVersionColl).Insert(ctx, bson.D{{Key: "candidate_id", Value: cand}, {Key: "offer_id", Value: prev["id"]}, {Key: "version", Value: int32(ver)},
		{Key: "type", Value: prev["type"]}, {Key: "doj", Value: prev["doj"]}, {Key: "grade", Value: prev["grade"]}, {Key: "gross", Value: prev["gross"]},
		{Key: "data", Value: prev["data"]}, {Key: "status", Value: st}, {Key: "answer", Value: prev["query_text"]}, {Key: "released_at", Value: prev["released_at"]},
		{Key: "expires_on", Value: prev["expires_on"]}, {Key: "created_by", Value: c.Name}})
	return err
}

// offerVersionSet updates the current version of a candidate's offer.
func offerVersionSet(ctx context.Context, tx store.Q, cand string, version int, set bson.D) error {
	if version <= 0 {
		version = 1
	}
	_, err := tx.C(store.OfferVersionColl).Update(ctx, bson.D{{Key: "candidate_id", Value: cand}, {Key: "version", Value: int32(version)}}, bson.D{{Key: "$set", Value: set}})
	return err
}

// offerAnswer runs after offerStep for a candidate's offer: the version, the Offer ID at acceptance, the timeline.
func offerAnswer(ctx context.Context, tx store.Q, c *Ctx, o store.Row, step, reason, expires string) error {
	cand := str(o["candidate_id"])
	if cand == "" {
		return nil
	}
	x, err := tx.C("candidate").FindOne(ctx, bson.D{{Key: "_id", Value: cand}})
	if err != nil || x == nil {
		return err
	}
	ver := max(1, toInt(o["version"]))
	vs := fmt.Sprintf("v%d", ver)
	now := tx.Now()
	switch step {
	case "release":
		if err := offerVersionSet(ctx, tx, cand, ver, bson.D{{Key: "status", Value: "Sent"}, {Key: "released_at", Value: now}, {Key: "expires_on", Value: expires},
			{Key: "pay", Value: offerPaySnap(ctx, tx, o)}}); err != nil { // the breakup the candidate got (offer_paysnap.go)
			return err
		}
		return lifeEvent(ctx, tx, c, lifeEv{Cand: cand, Action: "Offer " + vs + " sent", From: "Approved", To: "Sent", Reason: "Answer due by " + longDate(expires)})
	case "accept":
		oi := str(x["offer_no"])
		if oi == "" {
			oi = stageID("OI", toInt(x["no"]))
		}
		if _, err := tx.C("offer").Update(ctx, bson.D{{Key: "_id", Value: o["id"]}}, bson.D{{Key: "$set", Value: bson.D{{Key: "offer_no", Value: oi}}}}); err != nil {
			return err
		}
		if _, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: cand}}, bson.D{{Key: "$set", Value: bson.D{{Key: "offer_no", Value: oi}}}}); err != nil {
			return err
		}
		if err := offerVersionSet(ctx, tx, cand, ver, bson.D{{Key: "status", Value: "Accepted"}, {Key: "frozen", Value: true}, {Key: "answered_at", Value: now},
			{Key: "answered_by", Value: c.Name}}); err != nil {
			return err
		}
		return lifeEvent(ctx, tx, c, lifeEv{Cand: cand, Action: "Offer accepted: Offer ID " + oi, From: "Sent", To: "Offer accepted",
			Reason: fmt.Sprintf("Offer %s frozen. Joining on %s", vs, longDate(str(o["doj"])))})
	case "query":
		if err := offerVersionSet(ctx, tx, cand, ver, bson.D{{Key: "status", Value: "Question"}, {Key: "answer", Value: reason}, {Key: "answered_at", Value: now},
			{Key: "answered_by", Value: c.Name}}); err != nil {
			return err
		}
		return lifeEvent(ctx, tx, c, lifeEv{Cand: cand, Action: "Question about the breakup (" + vs + ")", From: "Sent", To: "Question", Reason: reason})
	case "decline":
		if err := offerVersionSet(ctx, tx, cand, ver, bson.D{{Key: "status", Value: "Declined"}, {Key: "answer", Value: reason}, {Key: "answered_at", Value: now},
			{Key: "answered_by", Value: c.Name}}); err != nil {
			return err
		}
		return lifeEvent(ctx, tx, c, lifeEv{Cand: cand, Action: "Closed: Offer declined", From: "Sent", To: "Closed", Reason: reason})
	case "expiry":
		if err := offerVersionSet(ctx, tx, cand, ver, bson.D{{Key: "expires_on", Value: expires}}); err != nil {
			return err
		}
		return lifeEvent(ctx, tx, c, lifeEv{Cand: cand, Action: "Offer expiry moved", From: longDate(str(o["expires_on"])), To: longDate(expires), Reason: reason})
	}
	return nil
}

// ---------- Employee stage and timeline ----------

func (s *Server) employeeStage(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("emp.status", 2); err != nil {
		return nil, err
	}
	var b struct {
		Stage  string `json:"stage"`
		Reason string `json:"reason"`
		Date   string `json:"date"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Reason = strings.TrimSpace(b.Reason)
	switch {
	case !containsStr(store.EmployeeStages, b.Stage) || b.Stage == "Joining":
		return nil, bad("Choose Probation, Confirmed, On notice, Suspended, Long leave or Absconding")
	case b.Reason == "":
		return nil, bad("Give the reason")
	case len(b.Reason) > 1000:
		return nil, bad("Keep the reason under 1,000 characters")
	}
	if b.Date == "" {
		b.Date = today()
	}
	if !isDate(b.Date) {
		return nil, bad("Enter the date it applies from")
	}
	ctx, id := r.Context(), r.PathValue("id")
	if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
		return nil, err
	}
	return map[string]string{"stage": b.Stage}, s.st.Tx(ctx, func(tx store.Q) error {
		e, err := tx.C("employee").Lock(ctx, bson.D{{Key: "_id", Value: id}})
		if err != nil {
			return err
		}
		if e == nil {
			return notFound("Employee not found")
		}
		if e["status"] == "Inactive" {
			return bad("%s has left", e["name"])
		}
		before := str(e["stage"])
		if before == b.Stage {
			return bad("%s is already %s", e["name"], b.Stage)
		}
		if _, err := tx.C("employee").Update(ctx, bson.D{{Key: "_id", Value: id}}, bson.D{{Key: "$set", Value: bson.D{{Key: "stage", Value: b.Stage}, {Key: "stage_since", Value: b.Date}}}}); err != nil {
			return err
		}
		from := before
		if from == "" {
			from = "Not set"
		}
		if err := lifeEvent(ctx, tx, c, lifeEv{Subject: "employee", Emp: id, Action: "Stage: " + b.Stage, From: from, To: b.Stage, Date: b.Date, Reason: b.Reason}); err != nil {
			return err
		}
		return audit(ctx, tx, c, fmt.Sprintf("Stage of %s (%s) set to %s", e["name"], id, b.Stage), "employee", id, map[string]string{"stage": before}, b)
	})
}

// employeeTimeline is the employee's status timeline: their own events, the hiring events of the candidate they came
// from, and status changes recorded in the employee change history (separation and others).
func (s *Server) employeeTimeline(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("emp.directory", 1); err != nil {
		return nil, err
	}
	ctx, id, q := r.Context(), r.PathValue("id"), s.st.DB
	if err := c.MustInScope(ctx, q, id); err != nil {
		return nil, err
	}
	e, err := onboardingOne(ctx, q, "employee", id)
	if err != nil || e == nil {
		return nil, orNotFound(err)
	}
	cands, _ := q.C("candidate").Distinct(ctx, "_id", bson.D{{Key: "employee_id", Value: id}})
	or := bson.A{bson.D{{Key: "employee_id", Value: id}}}
	if len(cands) > 0 {
		or = append(or, bson.D{{Key: "candidate_id", Value: bson.D{{Key: "$in", Value: cands}}}})
	}
	events := timelineRows(ctx, q, bson.D{{Key: "$or", Value: or}})
	changes, _ := q.C("employee_change").Find(ctx, bson.D{{Key: "employee_id", Value: id}, {Key: "field", Value: bson.D{{Key: "$in", Value: bson.A{"Status", "Exit status", "Exit"}}}}},
		options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).SetLimit(200))
	for _, ch := range changes {
		events = append(events, map[string]any{"at": ch["created_at"], "action": "Status: " + str(ch["new_value"]), "from": ch["old_value"], "to": ch["new_value"],
			"reason": ch["reason"], "date": ch["effective_from"], "by": ch["changed_by"], "subject": "employee", "employeeId": id})
	}
	sort.SliceStable(events, func(i, j int) bool {
		a, _ := events[i]["at"].(time.Time)
		b, _ := events[j]["at"].(time.Time)
		return a.Before(b)
	})
	return map[string]any{"id": id, "name": e["name"], "status": e["status"], "stage": e["stage"], "stageSince": e["stage_since"], "aliases": strs(e["alias_ids"]),
		"hireIds": hireIDsFor(ctx, q, id), // SI-/OI-/JI- (hirefix.go)
		"stages":  store.EmployeeStages, "canSetStage": c.Level("emp.status") >= 2 && e["status"] != "Inactive", "events": events}, nil
}

// ---------- Alias: a Joining ID still finds the record after it moved ----------

var reJIAlias = regexp.MustCompile(`^(?i)JI-\d{1,9}$`)

// resolveAlias points an old Joining ID in the path or query (id, emp, employeeId) at the record's id now. The
// candidate routes read JI- as the candidate's number and are left alone.
func (s *Server) resolveAlias(r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/candidates") {
		return
	}
	look := func(v string) string {
		if !reJIAlias.MatchString(v) {
			return ""
		}
		v = strings.ToUpper(v)
		if ok, _ := s.st.DB.C("employee").Exists(r.Context(), bson.M{"_id": v}); ok {
			return ""
		}
		return store.EmployeeByAlias(r.Context(), s.st.DB, v)
	}
	for _, k := range []string{"id", "emp", "employeeId"} {
		if n := look(r.PathValue(k)); n != "" {
			r.SetPathValue(k, n)
		}
	}
	qv := r.URL.Query()
	changed := false
	for _, k := range []string{"id", "emp", "employeeId"} {
		if n := look(qv.Get(k)); n != "" {
			qv.Set(k, n)
			changed = true
		}
	}
	if changed {
		r.URL.RawQuery = qv.Encode()
	}
}

// ---------- The hiring sweep: offer reminders and lapse, waiting reminders and automatic close ----------

const hireSweepEvery = 30 * time.Minute

type HireSweepResult struct {
	Lapsed, OfferReminders, WaitReminders, AutoClosed int
}

func noticeOnce(ctx context.Context, tx store.Q, key, to, text, link, view string) (bool, error) {
	if to == "" {
		return false, nil
	}
	if ok, err := tx.C("notice").Exists(ctx, bson.M{"ref": key}); err != nil || ok {
		return false, err
	}
	return true, sendNotice(ctx, tx, to, "", text, link, view, []string{key})
}

// HireSweep runs the hiring lifecycle's timed steps as of now. Every step is safe to run twice.
func (s *Server) HireSweep(ctx context.Context, now time.Time) (HireSweepResult, error) {
	var res HireSweepResult
	day := store.DateOf(now)
	hs := loadHireSettings(ctx, s.st.DB)
	// Offers sent and not answered.
	offers, err := s.st.DB.C("offer").Find(ctx, bson.D{{Key: "candidate_id", Value: bson.D{{Key: "$ne", Value: nil}}}, {Key: "released_at", Value: bson.D{{Key: "$ne", Value: nil}}},
		{Key: "accepted_at", Value: nil}, {Key: "declined_at", Value: nil}, {Key: "lapsed_at", Value: nil}, {Key: "query_at", Value: nil}, {Key: "expires_on", Value: bson.D{{Key: "$ne", Value: nil}}},
		{Key: "at_joining", Value: bson.D{{Key: "$ne", Value: true}}}}) // an offer at joining is never sent, so it never lapses (offer_at_joining.go)
	if err != nil {
		return res, err
	}
	for _, o := range offers {
		exp, cand := str(o["expires_on"]), str(o["candidate_id"])
		err := s.st.Tx(ctx, func(tx store.Q) error {
			x, err := tx.C("candidate").Lock(ctx, bson.D{{Key: "_id", Value: cand}})
			if err != nil || x == nil || x["stage"] != "Offer" {
				return err
			}
			x["offer_id"] = o["id"]
			link, view := "/onboarding?id="+cand, str(x["raised_role"])
			if exp < day { // no answer by the expiry date
				if _, err := tx.C("offer").Update(ctx, bson.D{{Key: "_id", Value: o["id"]}}, bson.D{{Key: "$set", Value: bson.D{{Key: "lapsed_at", Value: tx.Now()}}}}); err != nil {
					return err
				}
				if err := offerVersionSet(ctx, tx, cand, toInt(o["version"]), bson.D{{Key: "status", Value: "Lapsed"}}); err != nil {
					return err
				}
				if err := hireClose(ctx, tx, nil, x, "Offer lapsed", "No answer by "+longDate(exp), "Sent"); err != nil {
					return err
				}
				res.Lapsed++
				_, err := noticeOnce(ctx, tx, "hire:lapsed:"+cand, str(o["raised_by"]), fmt.Sprintf("The offer to %s (%s) lapsed: no answer by %s.", x["name"], cand, longDate(exp)), link, view)
				return err
			}
			if hs.OfferRemindDays > 0 && dayDiff(day, exp) <= hs.OfferRemindDays {
				sent, err := noticeOnce(ctx, tx, "hire:offer-expiry:"+cand+":"+exp+":"+day, str(o["raised_by"]),
					fmt.Sprintf("The offer to %s (%s) lapses after %s without an answer. Record the answer or move the expiry date.", x["name"], cand, longDate(exp)), link, view)
				if sent {
					res.OfferReminders++
				}
				return err
			}
			return nil
		})
		if err != nil {
			return res, err
		}
	}
	// Waiting windows.
	waiting, err := s.st.DB.C("candidate").Find(ctx, bson.D{{Key: "waiting", Value: bson.D{{Key: "$ne", Value: nil}}}, {Key: "stage", Value: bson.D{{Key: "$ne", Value: "Closed"}}}})
	if err != nil {
		return res, err
	}
	for _, wc := range waiting {
		cand := str(wc["id"])
		err := s.st.Tx(ctx, func(tx store.Q) error {
			x, err := s.loadCandidate(ctx, tx, nil, cand, true)
			if err != nil {
				return err
			}
			w := waitOf(x["waiting_window"], day)
			if w == nil || x["stage"] == "Closed" {
				return nil
			}
			link, view, to := "/onboarding?id="+cand, str(x["raised_role"]), str(x["raised_by"])
			if w.Over && hs.WaitAutoClose {
				reason := fmt.Sprintf("Did not come within the %d-day window (closed automatically)", w.Total)
				if w.Kind == "no-show" {
					err = hireClose(ctx, tx, nil, x, "No-show", reason, "Waiting")
				} else {
					err = earlyExit(ctx, tx, nil, x, stoppedLWD(x, w), reason, "Yes", "Waiting")
				}
				if err == nil {
					res.AutoClosed++
					_, err = noticeOnce(ctx, tx, "hire:auto-closed:"+cand, to, fmt.Sprintf("%s (%s) was closed automatically: %s.", x["name"], currentID(x), reason), link, view)
				}
				return err
			}
			text := fmt.Sprintf("%s (%s): %s. Log a call, mark them back, extend or close.", x["name"], currentID(x), waitLabel(w))
			if w.Over {
				text = fmt.Sprintf("%s (%s): %s.", x["name"], currentID(x), waitLabel(w))
			}
			sent, err := noticeOnce(ctx, tx, "hire:wait:"+cand+":"+day, to, text, link, view)
			if sent {
				res.WaitReminders++
			}
			return err
		})
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

type HireSweepArgs struct{}

func (HireSweepArgs) Kind() string { return "hire_sweep" }

func (HireSweepArgs) InsertOpts() jobs.InsertOpts {
	return jobs.InsertOpts{UniqueOpts: jobs.UniqueOpts{ByPeriod: hireSweepEvery}}
}

type hireSweepWorker struct {
	jobs.WorkerDefaults[HireSweepArgs]
	s *Server
}

func (w *hireSweepWorker) Work(ctx context.Context, _ *jobs.Job[HireSweepArgs]) error {
	if _, err := w.s.HireSweep(ctx, time.Now()); err != nil {
		log.Printf("hiring sweep: %v", err) // the next sweep tries again
	}
	return nil
}

// ---------- Drop-off report ----------

// dropKinds are the closures after an offer was made, in the order the report shows them.
var dropKinds = []string{"Offer declined", "Offer lapsed", "Withdrew before joining", "No-show", "Declined on joining day", "Early exit"}

// dropKind maps a closed kind to the report's column ("" when it is not a drop-off after the offer).
func dropKind(k string) string {
	switch k {
	case "Declined":
		return "Offer declined"
	case "Not joined": // before the waiting window existed: accepted and never came
		return "No-show"
	}
	if containsStr(dropKinds, k) {
		return k
	}
	return ""
}

func repDropoff(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	q := s.st.DB
	where, err := onboardingScope(ctx, q, c.Scope)
	if err != nil {
		return nil, nil, err
	}
	where = append(where, bson.E{Key: "stage", Value: "Closed"})
	if f.From != "" && f.To != "" {
		where = append(where, bson.E{Key: "closed_at", Value: repISTRange(f.From, f.To)})
	}
	rows, err := onboardingStagesCandidates(ctx, q, where, bson.D{{Key: "no", Value: 1}}, 0)
	if err != nil {
		return nil, nil, err
	}
	by := f.DS
	if by == "" {
		by = "month"
	}
	label := map[string]string{"month": "Month", "property": "Property", "role": "Role", "recruiter": "Recruiter"}[by]
	if label == "" {
		return nil, nil, bad("Choose month, property, role or recruiter")
	}
	counts := map[string]map[string]int{}
	keys := []string{}
	for _, x := range rows {
		k := dropKind(str(x["closed_kind"]))
		if k == "" {
			continue
		}
		if f.City != "" && str(x["city_id"]) != f.City {
			continue
		}
		if f.Dept != "" && str(x["department"]) != f.Dept {
			continue
		}
		g := ""
		switch by {
		case "month":
			if t, ok := x["closed_at"].(time.Time); ok {
				g = store.DateOf(t)[:7]
			}
		case "property":
			g = str(x["location"])
		case "role":
			g = str(x["designation"])
		case "recruiter":
			g = str(x["raised_by_name"])
		}
		if counts[g] == nil {
			counts[g] = map[string]int{}
			keys = append(keys, g)
		}
		counts[g][k]++
	}
	sort.Strings(keys)
	cols := append(append([]string{label}, dropKinds...), "Total")
	out := [][]any{}
	for _, g := range keys {
		row, tot := []any{g}, 0
		for _, k := range dropKinds {
			row = append(row, counts[g][k])
			tot += counts[g][k]
		}
		out = append(out, append(row, tot))
	}
	return cols, out, nil
}

var hireDropoffReport = report{ID: "hire_dropoff", Group: "Workforce", Name: "Hiring drop-off",
	Hint:    "Where candidates are lost after the offer: declined, lapsed, withdrew, no-show, declined on the day, early exit",
	Filters: []string{"range", "city", "dept"}, Need: func(c *Ctx) bool { return c.Level("onb.pipeline") >= 1 || c.Level("onb.raise") >= 1 || hrOnly(c) },
	Extra: []repExtra{{K: "ds", L: "By", O: [][2]string{{"month", "Month"}, {"property", "Property"}, {"role", "Role"}, {"recruiter", "Recruiter"}}}},
	Run:   repDropoff, Sum: append(append([]string{}, dropKinds...), "Total")}

// ---------- Routes ----------

func (s *Server) hireRoutes(m *http.ServeMux) {
	if reportByID(hireDropoffReport.ID) == nil {
		reports = append(reports, hireDropoffReport)
	}
	m.HandleFunc("POST /api/candidates/{id}/event", s.a(s.candidateEvent))
	m.HandleFunc("POST /api/candidates/{id}/bgv", s.a(s.candidateBGV))
	m.HandleFunc("POST /api/candidates/{id}/offer-at-joining", s.a(s.candidateOfferAtJoining)) // offer_at_joining.go
	m.HandleFunc("POST /api/employees/{id}/stage", s.a(s.employeeStage))
	m.HandleFunc("GET /api/employees/{id}/timeline", s.a(s.employeeTimeline))
}
