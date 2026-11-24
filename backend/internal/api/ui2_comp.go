package api

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/leave"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// Round 2, onboarding, compensation, access, reports and bulk (prototype gaps B and G): rules by
// employment type kept in the database, switching a structure version off, the offer template, the
// work policy and LWF note for an offer, documents asked from the candidate, CSV downloads and the
// "Import employees" bulk template.

// ---------- Rules by employment type ----------

// syncTypeRules makes the stored rules by employment type the rules the salary engine uses. It runs on
// every signed-in request and before every payroll run, so a change made on one server is used everywhere.
func syncTypeRules(ctx context.Context, q store.Q) {
	rows, err := q.C("comp_type_rule").Find(ctx, bson.M{}, options.Find().SetProjection(compFields("type", "rules")))
	if err != nil {
		return
	}
	m := map[calc.EmploymentType]calc.TypeRules{}
	for _, x := range rows {
		var r calc.TypeRules
		if compJSONInto(x["rules"], &r) == nil {
			m[calc.EmploymentType(str(x["type"]))] = r
		}
	}
	calc.SetTypeRules(m)
}

var typeRuleLabels = map[string]string{"statutory": "PF, ESI, medical", "lwf": "Labour welfare fund", "grat": "Gratuity", "plEnc": "PL encashment",
	"bonus": "Bonus allowed", "variable": "Variable pay allowed", "ded": "Employee deductions", "leave": "Can apply for leave", "docs": "Payslip and letters", "leaveTypes": "Leave types allowed"}

// typeRulesPut changes what one employment type switches on (Compensation, Salary structure).
func (s *Server) typeRulesPut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("comp.structure", 2); err != nil {
		return nil, err
	}
	typ := calc.EmploymentType(r.PathValue("type"))
	if _, ok := calc.DefaultTypeRules[typ]; !ok {
		return nil, notFound("Employment type must be Permanent, Retainer or Contractual")
	}
	var b calc.TypeRules
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Ded != "all" && b.Ded != "tds" && b.Ded != "none" {
		return nil, bad("Employee deductions are PF, ESI, meal and accommodation, TDS only, or none")
	}
	ctx := r.Context()
	// Leave eligibility: none (leave off), only the chosen leave types, or all of them (none chosen).
	if !b.Leave || len(b.LeaveTypes) == 0 {
		b.LeaveTypes = nil
	}
	for _, n := range b.LeaveTypes {
		if ok, err := s.st.DB.C("leave_type").Exists(ctx, bson.M{"name": n}); err != nil {
			return nil, err
		} else if !ok {
			return nil, bad("Leave type %s does not exist", n)
		}
	}
	err := s.st.Tx(ctx, func(tx store.Q) error {
		old := calc.DefaultTypeRules[typ] // the stored rules, read over the defaults
		cur, err := tx.C("comp_type_rule").Lock(ctx, bson.M{"type": string(typ)})
		if err != nil {
			return err
		}
		if cur != nil {
			_ = compJSONInto(cur["rules"], &old)
		}
		if reflect.DeepEqual(old, b) {
			return nil
		}
		if _, err := tx.C("comp_type_rule").Upsert(ctx, bson.M{"type": string(typ)}, bson.M{"rules": b, "by_name": c.Name, "at": tx.Now()}, nil); err != nil {
			return err
		}
		ch := diff(toAny(old), toAny(b))
		for _, x := range ch {
			if l, ok := typeRuleLabels[fmt.Sprint(x["field"])]; ok {
				x["field"] = l
			}
		}
		if len(ch) > 0 {
			if _, err := tx.C("config_history").Insert(ctx, bson.M{"entity": "comp_type_rule", "entity_id": string(typ), "changes": ch, "by_name": c.Name}); err != nil {
				return err
			}
		}
		return audit(ctx, tx, c, "Rules for "+string(typ)+" employees changed", "comp_type_rule", string(typ), old, b)
	})
	if err != nil {
		return nil, err
	}
	syncTypeRules(ctx, s.st.DB)
	return map[string]any{"types": calc.TypeRulesNow()}, nil
}

// ---------- Salary structure versions: active employees, switch off and on ----------

// activeOnVersion counts, per structure id, the active employees whose version on date is that structure: their
// latest mapping from that date or earlier, else the default version.
func activeOnVersion(ctx context.Context, q store.Q, date string) (map[string]int64, error) {
	emps, err := q.C("employee").Distinct(ctx, "_id", bson.M{"status": "Active"})
	if err != nil {
		return nil, err
	}
	latest, err := q.C("structure_map").Aggregate(ctx, bson.A{
		bson.M{"$match": bson.M{"employee_id": bson.M{"$in": emps}, "from_date": bson.M{"$lte": date}}},
		bson.M{"$sort": compOrder("employee_id", "-from_date")},
		bson.M{"$group": bson.M{"_id": "$employee_id", "sid": bson.M{"$first": "$structure_id"}}},
	})
	if err != nil {
		return nil, err
	}
	on := map[string]string{}
	for _, x := range latest {
		on[str(x["id"])] = str(x["sid"])
	}
	def := compStructureDefault(ctx, q)
	out := map[string]int64{}
	for _, e := range emps {
		sid, mapped := on[str(e)]
		if !mapped {
			sid = def
		}
		if sid != "" {
			out[sid]++
		}
	}
	return out, nil
}

// structureStatus switches a version off or on. The default version stays on, and a version stays on
// while active employees are on it (prototype stOffV).
func (s *Server) structureStatus(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("comp.structure", 2); err != nil {
		return nil, err
	}
	var b struct {
		On *bool `json:"on"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.On == nil {
		return nil, bad("Say whether the version is switched on or off")
	}
	ctx, id := r.Context(), r.PathValue("id")
	var out map[string]any
	err := s.st.Tx(ctx, func(tx store.Q) error {
		cur, err := tx.C("salary_structure").Lock(ctx, bson.M{"_id": id})
		if err != nil {
			return err
		}
		if cur == nil {
			return notFound("Version not found")
		}
		name, status := str(cur["name"]), str(cur["status"])
		def, _ := cur["is_default"].(bool)
		want := map[bool]string{true: "Active", false: "Inactive"}[*b.On]
		if !*b.On {
			if def {
				return bad("%s is the default for new joiners. Make another version the default first", name)
			}
			active, err := activeOnVersion(ctx, tx, today())
			if err != nil {
				return err
			}
			if now := active[id]; now > 0 {
				return bad("%d employees are still on %s. Map them to another version first", now, name)
			}
			// People mapped to it from a later date (not inactive).
			mapped, err := tx.C("structure_map").Distinct(ctx, "employee_id", bson.M{"structure_id": id, "from_date": bson.M{"$gt": today()}})
			if err != nil {
				return err
			}
			later, err := tx.C("employee").Count(ctx, bson.M{"_id": bson.M{"$in": mapped}, "status": bson.M{"$ne": "Inactive"}})
			if err != nil {
				return err
			}
			if later > 0 {
				return bad("%d employees are mapped to %s from a later date. Map them to another version first", later, name)
			}
		}
		out = map[string]any{"id": id, "status": want}
		if status == want {
			return nil
		}
		if _, err := tx.C("salary_structure").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"status": want}}); err != nil {
			return err
		}
		return audit(ctx, tx, c, name+map[bool]string{true: " switched on", false: " switched off"}[*b.On], "salary_structure", id, map[string]string{"status": status}, map[string]string{"status": want})
	})
	return out, err
}

// ---------- Offer letter template ----------

func (s *Server) offerTemplatePut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("comp.structure", 2); err != nil {
		return nil, err
	}
	var b struct {
		Template string `json:"template"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if strings.TrimSpace(b.Template) == "" {
		return nil, bad("The offer letter template cannot be empty")
	}
	if len(b.Template) > 20000 {
		return nil, bad("Keep the template under 20,000 characters")
	}
	ctx := r.Context()
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		cur, err := tx.C("company_setting").Lock(ctx, bson.M{"_id": 1})
		if err != nil {
			return err
		}
		if cur == nil {
			return store.ErrNoRows
		}
		old := str(cur["offer_template"])
		if old == b.Template {
			return nil
		}
		if _, err := tx.C("company_setting").Update(ctx, bson.M{"_id": 1}, bson.M{"$set": bson.M{"offer_template": b.Template, "updated_at": tx.Now()}}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Offer letter template changed", "company_setting", "offer_template", map[string]string{"template": old}, map[string]string{"template": b.Template})
	})
}

// offerLetterOnly: the caller reads offers only to preview the letter (Compensation, Offer letter), so
// the candidate's personal details and documents are left out.
func offerLetterOnly(c *Ctx) bool {
	return c.Level("onb.pipeline") < 1 && c.Level("onb.raise") < 1 && c.Level("appr.queue") < 1
}

var offerPreviewKeys = []string{"id", "candidate_name", "type", "doj", "grade", "department", "designation", "location", "city", "status", "request_status", "letter", "released_at", "accepted_at",
	"letterAt", "letterHeld", "letterRule", "letterReleasedAt"}

func offerPreviewOnly(o store.Row) store.Row {
	out := store.Row{}
	for _, k := range offerPreviewKeys {
		if v, ok := o[k]; ok {
			out[k] = v
		}
	}
	return out
}

// ---------- Work policy for a candidate (New joining side card and the {{probation}} merge field) ----------

type offerPolicy struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	Via       string `json:"via"`
	WeekOff   string `json:"weekOff"`
	Probation int    `json:"probation"`
	PL        string `json:"pl"`
	CL        string `json:"cl"`
}

var satLabel = map[string]string{"all": "all Saturdays", "2,4": "2nd and 4th Saturdays", "1,3": "1st and 3rd Saturdays"}

// weekOffLine is the prototype's weekOffText.
func weekOffLine(s policy.Settings) string {
	if s.OffMode == "rotational" {
		return "One rotational weekly off, set in the roster"
	}
	t := "No fixed Sunday off"
	if s.Sun == "all" {
		t = "All Sundays"
	}
	if l := satLabel[s.Sat]; l != "" {
		t += " + " + l
	}
	return t
}

func days(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// leaveLine is the prototype's plText and clText for the plan's leave type.
func leaveLine(cfg *leave.TypeCfg, lv leave.LV, name, typ, short string, pl bool) string {
	if cfg == nil || !cfg.On || (len(cfg.Types) > 0 && !containsStr(cfg.Types, typ)) || !calc.RulesFor(calc.EmploymentType(typ)).Leave {
		return short + " not applicable"
	}
	if pl {
		from := "from joining"
		if cfg.From == "prob" {
			from = "after probation is completed"
		}
		back := ""
		if cfg.Catchup {
			back = " with back-dated credit"
		}
		if blk, in := lv.BlockFor(name, *cfg); in {
			return fmt.Sprintf("%s earned by working days, %s, accrues %s%s", short, blk.Text(cfg.Days), from, back)
		}
		return fmt.Sprintf("%s %s a year, accrues %s%s", short, days(cfg.Days), from, back)
	}
	if blk, in := lv.BlockFor(name, *cfg); in {
		return fmt.Sprintf("%s earned by working days, %s", short, blk.Text(cfg.Days))
	}
	credit := "credited upfront"
	if cfg.Credit == "monthly" {
		credit = "credited monthly from joining"
	}
	return fmt.Sprintf("%s %s a year, %s", short, days(cfg.Days), credit)
}

// candidatePolicy resolves the work policy the rules give a candidate (location, cluster, department,
// designation, grade, type, city, state), the same way as for an employee.
func candidatePolicy(ctx context.Context, q store.Q, empID string, j offerJob) *offerPolicy {
	ts, err := loadTimeSet(ctx, q)
	if err != nil {
		return nil
	}
	typ := j.Type
	if typ == "" {
		typ = "Permanent"
	}
	e := &emp{ID: empID, Type: typ, Grade: j.Grade, LocID: j.LocID, CityID: j.CityID, Dept: j.Dept, Desig: j.Desig}
	if e.ID == "" {
		e.ID = "new"
	}
	if j.LocID != "" {
		if cluster, city, state, ok := ui2CompPlaceOf(ctx, q, j.LocID); ok {
			e.ClusterID, e.CityID, e.StateID = cluster, city, state
		}
	} else if j.CityID != "" {
		if ct, _ := q.C("city").FindOne(ctx, bson.M{"_id": j.CityID}, options.FindOne().SetProjection(compFields("state_id"))); ct != nil {
			e.StateID = str(ct["state_id"])
		}
	}
	p, via := ts.policyFor(e)
	out := &offerPolicy{ID: p.ID, Name: p.Name, Via: via, WeekOff: weekOffLine(p.S), Probation: p.S.ProbMonths(e.Grade)}
	var lv leave.LV
	if pl := ts.plan(p); pl != nil {
		lv = pl.LV
	}
	out.PL = leaveLine(ts.typeCfg(p, "Privilege leave"), lv, "Privilege leave", typ, "PL", true)
	out.CL = leaveLine(ts.typeCfg(p, "Casual leave"), lv, "Casual leave", typ, "CL", false)
	return out
}

// ui2CompPlaceOf is a location's cluster, city and state (ok false when the location or one of them is missing).
func ui2CompPlaceOf(ctx context.Context, q store.Q, locID string) (cluster, city, state string, ok bool) {
	l, _ := q.C("location").FindOne(ctx, bson.M{"_id": locID}, options.FindOne().SetProjection(compFields("cluster_id")))
	if l == nil {
		return "", "", "", false
	}
	k, _ := q.C("cluster").FindOne(ctx, bson.M{"_id": l["cluster_id"]}, options.FindOne().SetProjection(compFields("city_id")))
	if k == nil {
		return "", "", "", false
	}
	c, _ := q.C("city").FindOne(ctx, bson.M{"_id": k["city_id"]}, options.FindOne().SetProjection(compFields("state_id")))
	if c == nil {
		return "", "", "", false
	}
	return str(l["cluster_id"]), str(k["city_id"]), str(c["state_id"]), true
}

// offerProbation fills {{probation}} in the offer letter: "6 months", or "none" (prototype offerText).
func offerProbation(ctx context.Context, q store.Q, o store.Row) string {
	j := offerJob{Type: str(o["type"]), Grade: str(o["grade"]), Dept: str(o["department"]), Desig: str(o["designation"]), LocID: str(o["location_id"])}
	p := candidatePolicy(ctx, q, str(o["employee_id"]), j)
	if p == nil {
		return "as per the work policy"
	}
	if p.Probation > 0 {
		return fmt.Sprintf("%d months", p.Probation)
	}
	return "none"
}

// lwfNoteText is the note on the labour welfare fund rows: "Every month" or "Twice a year, months 6 and 12".
func lwfNoteText(r *calc.LwfRule) string {
	if r == nil || !r.On {
		return ""
	}
	if r.Freq == "monthly" {
		return "Every month"
	}
	ms := []string{}
	for _, p := range strings.Split(r.Months, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && n > 0 {
			ms = append(ms, strconv.Itoa(n))
		}
	}
	label := "Twice a year"
	if r.Freq == "year" {
		label = "Once a year"
	}
	if len(ms) > 1 {
		return label + ", months " + strings.Join(ms, " and ")
	}
	return label + ", month " + strings.Join(ms, " and ")
}

// offerExtras adds to the offer check what the New joining page shows beyond the checks: the work
// policy, the LWF note on the breakup and the documents asked from the candidate.
func (s *Server) offerExtras(ctx context.Context, c *Ctx, j offerJob, draft string, out map[string]any) {
	if p := candidatePolicy(ctx, s.st.DB, "", j); p != nil {
		out["policy"] = p
	}
	if j.LocID != "" {
		out["lwfNote"] = lwfNoteText(lwfFor(ctx, s.st.DB, j.LocID, today()))
	}
	if j.Type != "" { // what the type switches on, for the rows of the breakup
		out["rules"] = calc.RulesFor(calc.EmploymentType(j.Type))
	}
	out["asked"] = map[string]bool{}
	if draftOwner(draft) {
		out["asked"] = askedDocs(ctx, s.st.DB, draft, c.EmployeeID)
	}
}

// ---------- Ask candidate for an offer document ----------

func askedDocs(ctx context.Context, q store.Q, owner, by string) map[string]bool {
	out := map[string]bool{}
	filter := bson.M{"owner_id": owner}
	if by != "" {
		filter["by_id"] = by
	}
	rows, err := q.C("offer_doc_request").Find(ctx, filter, options.Find().SetProjection(compFields("doc_key")).SetSort(compOrder("doc_key")))
	if err == nil {
		for _, x := range rows {
			out[str(x["doc_key"])] = true
		}
	}
	return out
}

// draftDocAsk records or cancels "Ask candidate" for a document of a draft offer. The request stays
// with the draft and moves to the offer when it is raised.
func (s *Server) draftDocAsk(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("onb.raise", 2); err != nil {
		return nil, err
	}
	draft, key, action := r.PathValue("draft"), r.PathValue("key"), r.PathValue("action")
	cand := isCandidateID(draft) // an offer for a selected candidate keeps its asks with the candidate (SI-0001)
	if !draftOwner(draft) && !cand {
		return nil, bad("Unknown draft")
	}
	if action != "request" && action != "cancel" {
		return nil, notFound("")
	}
	ctx := r.Context()
	_, ds, err := loadSetup(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	known := false
	for _, d := range ds {
		if d.Key == key && d.On {
			known = true
		}
	}
	if !known {
		return nil, bad("Unknown document")
	}
	by := c.EmployeeID
	if cand {
		x, err := s.loadCandidate(ctx, s.st.DB, c, draft, false)
		if err != nil {
			return nil, err
		}
		if str(x["id"]) != draft {
			return nil, bad("Unknown draft")
		}
		by = ""
	} else if ok, _ := s.st.DB.C("offer_doc_request").Exists(ctx, bson.M{"owner_id": draft, "by_id": bson.M{"$ne": c.EmployeeID}}); ok {
		return nil, forbidden("")
	}
	asks := s.st.DB.C("offer_doc_request")
	if action == "request" { // asked once; asking again changes nothing
		_, err = asks.Upsert(ctx, bson.M{"owner_id": draft, "doc_key": key}, nil, bson.M{"by_id": c.EmployeeID, "by_name": c.Name})
	} else {
		_, err = asks.Delete(ctx, bson.M{"owner_id": draft, "doc_key": key})
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"asked": askedDocs(ctx, s.st.DB, draft, by)}, nil
}

// moveDocAsks hands a draft's requests to the offer raised from it.
func moveDocAsks(ctx context.Context, tx store.Q, draft, offer string) error {
	if !draftOwner(draft) {
		return nil
	}
	_, err := tx.C("offer_doc_request").UpdateMany(ctx, bson.M{"owner_id": draft}, bson.M{"$set": bson.M{"owner_id": offer}})
	return err
}

// ---------- CSV downloads (reports: "Download CSV") ----------

// csvCell writes a value as text; text that a spreadsheet would run as a formula is quoted.
func csvCell(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 64)
	case int, int16, int32, int64:
		return fmt.Sprint(x)
	case time.Time:
		return fmt.Sprint(cell(x))
	}
	s := fmt.Sprint(v)
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func writeCSV(w http.ResponseWriter, name string, cols []string, rows [][]any) {
	name = strings.TrimSuffix(name, ".xlsx") + ".csv"
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"; filename*=UTF-8''`+url.PathEscape(name))
	_, _ = w.Write([]byte("\xef\xbb\xbf")) // so Excel reads ₹ and names in UTF-8
	cw := csv.NewWriter(w)
	head := make([]string, len(cols))
	for i, h := range cols {
		head[i] = csvCell(h)
	}
	_ = cw.Write(head)
	for _, row := range rows {
		rec := make([]string, len(row))
		for i, v := range row {
			rec[i] = csvCell(v)
		}
		_ = cw.Write(rec)
	}
	cw.Flush()
}

// ---------- Bulk: Import employees (prototype TPL.new, HR Admin only) ----------

// Monthly Gross is not a column any more (owner, 14 Nov 2026): a new employee's salary is set afterwards with Bulk
// update › Salary breakup or Compensation. A file that still has it is read with the column ignored and a note.
// Role and Skill are required like the department (hirefix.go); Existing UAN? is optional (b1_employee.go). Each
// imported employee gets Shortlisting, Offer and Joining IDs from the candidate series (hirefix.go).
var newEmpCols = []string{"Employee Code", "Full Name", "Employment Type", "Date of Joining", "Work City", "Reporting Location", "Department", "Designation", "Grade",
	"Shift", "Reporting Manager L1", "Functional Manager L2", "Work Policy", "Attendance Mode", "Role", "Skill", "Mobile", "Email", "Date of Birth", "Gender", "PAN",
	"Aadhaar", "UAN", "Existing UAN?", "ESIC No", "Bank Name", "Account Number", "IFSC", "Status", "Last Working Day", "Opening CL Balance", "Opening PL Balance", "Opening SL Balance",
	"Shirt / T-shirt Size", "Trouser Waist", "Trouser Length", "Shoe Size (UK)"}

func init() {
	lists := map[string]bulkList{}
	for col, k := range uniBulkKey {
		lists[col] = uniBulkList(k)
	}
	bulkTemplates = append(bulkTemplates, &bulkTemplate{Key: "new", Name: "Import employees", Cols: newEmpCols, Optional: append([]string{"Existing UAN?"}, uniBulkCols[1:]...),
		Hint:  "Use this to load employees who are already working, or to add many at once. A code in the first column is kept as it is. Leave it blank and the system issues the next code. Status is Active unless the person has already left, in which case give Inactive and the last working day. Rows are checked against masters before anything is created. The salary is not part of this file: set it afterwards with Bulk update › Salary breakup or in Compensation. Role and Skill are required, like the department, and must be active masters. Each employee also gets Shortlisting, Offer and Joining IDs (SI-, OI-, JI-) with one number from the candidate series. Existing UAN? is Yes or No; Yes needs the UAN.",
		allow: func(c *Ctx) bool { return c.Role == "HR Admin" },
		lists: map[string]bulkList{"Employment Type": bulkFixed("Permanent", "Retainer", "Contractual"), "Work City": bulkActive("city", "name"),
			"Reporting Location": bulkActive("location", "name"), "Department": bulkActive("department", "name"),
			"Designation": bulkDistinct("designation", "name", bson.D{{Key: "status", Value: "Active"}}), "Grade": bulkActive("grade", "name"),
			"Shift": bulkActive("shift", "code"), "Work Policy": bulkQuery("work_policy", "name", bson.D{{Key: "status", Value: "Active"}}, compOrder("sort", "name")),
			"Attendance Mode": bulkMaster("attendance_mode", ""), "Role": bulkMaster(store.KindJobRole, ""), "Skill": bulkMaster(store.KindSkill, ""),
			"Existing UAN?": bulkFixed("Yes", "No"),
			"Gender":        bulkFixed("Male", "Female", "Other", "Prefer not to say"), "Status": bulkFixed("Active", "Inactive"),
			"Shirt / T-shirt Size": lists["Shirt / T-shirt Size"], "Trouser Waist": lists["Trouser Waist"], "Trouser Length": lists["Trouser Length"],
			"Shoe Size (UK)": lists["Shoe Size (UK)"]},
		row: bulkNewEmp})
	attCodeWire(bulkTemplates[len(bulkTemplates)-1], true) // realtime_bulk.go
}

var reNewDate = regexp.MustCompile(`^(\d{1,2})[/-](\d{1,2})[/-](\d{4})$`)

// importDate reads 2023-06-12, 12/06/2023, 12-06-2023 or an Excel date number (prototype pdate).
func importDate(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if isDate(v) {
		return v
	}
	if m := reNewDate.FindStringSubmatch(v); m != nil {
		d := fmt.Sprintf("%s-%02s-%02s", m[3], m[2], m[1])
		d = strings.ReplaceAll(d, " ", "0")
		if isDate(d) {
			return d
		}
		return ""
	}
	if d := excelDate(v); isDate(d) {
		return d
	}
	return ""
}

// ui2CompSameText matches a text field equal to v ignoring case, as lower(col)=lower(v) and upper(col)=upper(v) did.
func ui2CompSameText(v string) bson.M {
	return bson.M{"$regex": "^" + regexp.QuoteMeta(v) + "$", "$options": "i"}
}

// managerRef finds an active employee by Employee ID or by exact name (the name must be unique).
func managerRef(ctx context.Context, q store.Q, v string) (string, string) {
	if e, _ := q.C("employee").FindOne(ctx, bson.M{"_id": ui2CompSameText(v), "status": "Active"}, options.FindOne().SetProjection(compFields("id"))); e != nil {
		return str(e["id"]), ""
	}
	ids, _ := q.C("employee").Find(ctx, bson.M{"name": ui2CompSameText(v), "status": "Active"}, options.Find().SetProjection(compFields("id")).SetLimit(2))
	switch len(ids) {
	case 1:
		return str(ids[0]["id"]), ""
	case 0:
		return "", "unknown"
	}
	return "", "matches more than one employee. Give the Employee ID"
}

// bulkNewEmp checks one row of "Import employees" (prototype newEmpValidate) and creates the employee.
// Rows are created inside the upload's transaction even on Check, which is rolled back, so codes and
// PANs repeated in the file and managers added earlier in the file are seen by later rows.
func bulkNewEmp(s *Server, ctx context.Context, tx store.Q, c *Ctx, r bulkRow, apply bool) (bulkResult, error) {
	V := func(k string) string { return strings.TrimSpace(r.V[k]) }
	code, name := V("Employee Code"), V("Full Name")
	res := bulkResult{ID: or(code, "(system code)"), Name: name}
	add := func(f string, a ...any) { res.Errors = append(res.Errors, fmt.Sprintf(f, a...)) }
	if code != "" {
		if !reCode.MatchString(code) {
			add("Employee Code can have letters, numbers and - _ . only, up to 20 characters")
		} else {
			if ex, _ := tx.C("employee").FindOne(ctx, bson.M{"_id": ui2CompSameText(code)}, options.FindOne().SetProjection(compFields("created_at"))); ex != nil {
				// An employee created earlier in this upload was created at the transaction's time.
				if at, _ := ex["created_at"].(time.Time); at.Equal(tx.Now()) {
					add("Code %s appears twice in the file", code)
				} else {
					add("Code %s already exists", code)
				}
			}
		}
	}
	if name == "" {
		add("Full Name is required")
	} else if n := len([]rune(name)); n > 100 {
		add("Full Name can be at most 100 characters (this one has %d)", n)
	} else if strings.ContainsRune("=+-@", rune(name[0])) {
		add("Full Name cannot start with %c", name[0]) // read as a formula when exported (PPL-110)
	}
	typ := V("Employment Type")
	for _, t := range empTypes {
		if strings.EqualFold(t, typ) {
			typ = t
		}
	}
	if !containsStr(empTypes, typ) {
		add("Employment Type “%s” not in master", V("Employment Type"))
	} else if ok, _ := tx.C("simple_master").Exists(ctx, bson.M{"kind": "employment_type", "name": typ, "status": bson.M{"$ne": "Active"}}); ok {
		add("Employment Type is inactive")
	}
	doj := importDate(V("Date of Joining"))
	if doj == "" {
		add("Date of Joining must be like 2023-06-12 or 12/06/2023")
	}
	// byName is a master record whose name matches ignoring case (nil when there is none).
	byName := func(coll string, v string, more bson.M) store.Row {
		f := bson.M{"name": ui2CompSameText(v)}
		for k, x := range more {
			f[k] = x
		}
		row, _ := tx.C(coll).FindOne(ctx, f)
		return row
	}
	var cityID, cityName, cityStatus string
	if ct := byName("city", V("Work City"), nil); ct == nil {
		add("Work City “%s” not in master", V("Work City"))
	} else if cityID, cityName, cityStatus = str(ct["id"]), str(ct["name"]), str(ct["status"]); cityStatus != "Active" {
		add("Work City is inactive")
	}
	var locID, locName, locStatus, locCity string
	loc := byName("location", V("Reporting Location"), nil)
	if loc != nil { // the location's city, through its cluster
		k, _ := tx.C("cluster").FindOne(ctx, bson.M{"_id": loc["cluster_id"]}, options.FindOne().SetProjection(compFields("city_id")))
		if k == nil {
			loc = nil
		} else {
			locID, locName, locStatus, locCity = str(loc["id"]), str(loc["name"]), str(loc["status"]), str(k["city_id"])
		}
	}
	if loc == nil {
		add("Reporting Location “%s” not in master", V("Reporting Location"))
		locID = ""
	} else if locStatus != "Active" {
		add("Reporting Location is inactive")
	} else if cityID != "" && locCity != cityID {
		add("Location “%s” is not in %s", locName, cityName)
	}
	var deptID, deptName, deptStatus string
	var desID, desName, desStatus string
	var grades []string
	if d := byName("department", V("Department"), nil); d == nil {
		add("Department “%s” not in master", V("Department"))
		deptID = ""
	} else if deptID, deptName, deptStatus = str(d["id"]), str(d["name"]), str(d["status"]); deptStatus != "Active" {
		add("Department is inactive")
	}
	if deptID != "" {
		if g := byName("designation", V("Designation"), bson.M{"department_id": deptID}); g == nil {
			add("Designation “%s” does not exist under %s", V("Designation"), deptName)
			desID = ""
		} else if desID, desName, desStatus, grades = str(g["id"]), str(g["name"]), str(g["status"]), compStrs(g["grades"]); desStatus != "Active" {
			add("Designation is inactive")
		}
	}
	var grade, gradeStatus string
	if g := byName("grade", V("Grade"), nil); g == nil {
		add("Grade “%s” not in master", V("Grade"))
	} else if grade, gradeStatus = str(g["name"]), str(g["status"]); gradeStatus != "Active" {
		add("Grade is inactive")
	} else if desID != "" && len(grades) > 0 && !containsStr(grades, grade) {
		add("Grade %s is not mapped to %s", grade, desName)
	}
	shift := strings.ToUpper(or(V("Shift"), "G"))
	var shiftStatus string
	var shiftOK bool
	if sh, _ := tx.C("shift").FindOne(ctx, bson.M{"code": ui2CompSameText(shift)}, options.FindOne().SetProjection(compFields("id", "status"))); sh == nil {
		add("Shift “%s” not in master", V("Shift"))
	} else if shiftStatus = str(sh["status"]); shiftStatus != "Active" {
		add("Shift is inactive")
	} else if shiftOK = locID == "" || deptID == ""; !shiftOK {
		if shiftOK, _ = shiftRowFor(ctx, tx, str(sh["id"]), deptID, locID); !shiftOK {
			add("%s", shiftNotForMsg(shift, deptName, locName))
		}
	}
	l1, l2 := "", ""
	if v := V("Reporting Manager L1"); v == "" {
		add("Reporting Manager L1 is required")
	} else if id, why := managerRef(ctx, tx, v); id == "" {
		add("Reporting Manager L1 “%s” %s", v, why)
	} else {
		l1 = id
	}
	if v := V("Functional Manager L2"); v != "" {
		if id, why := managerRef(ctx, tx, v); id == "" {
			add("Functional Manager L2 “%s” %s", v, why)
		} else {
			l2 = id
		}
	}
	var polID, polName, polStatus string
	if v := V("Work Policy"); v != "" {
		if wp := byName("work_policy", v, nil); wp == nil {
			add("Work Policy “%s” not found", v)
			polID = ""
		} else if polID, polName, polStatus = str(wp["id"]), str(wp["name"]), str(wp["status"]); polStatus != "Active" {
			add("Work Policy is inactive")
			polID = ""
		}
	}
	attMode := store.SysName(ctx, tx, "mode.default")
	if v := V("Attendance Mode"); v != "" {
		if m := byName("simple_master", v, bson.M{"kind": "attendance_mode", "status": "Active"}); m == nil {
			add("Attendance Mode “%s” not in master", v)
		} else {
			attMode = str(m["name"])
		}
	}
	_, oldGross := r.V["Monthly Gross"] // a file made before 14 Nov 2026: the column is ignored
	jobRole, skill := importJobMasters(ctx, tx, V("Role"), V("Skill"), add)
	// The same rules as Personal details (PPL-090).
	mobile, email := strings.ReplaceAll(V("Mobile"), " ", ""), V("Email")
	if mobile != "" && !reMobile.MatchString(mobile) {
		add("Mobile must be 10 digits")
	} else if other := personalMobileOwner(ctx, tx, mobile, ""); mobile != "" && other != "" {
		add("Mobile %s is already used by %s", mobile, other)
	}
	if email != "" && !reEmail.MatchString(email) {
		add("Email is not valid")
	}
	dob := ""
	if v := V("Date of Birth"); v != "" {
		if dob = importDate(v); dob == "" {
			add("Date of Birth must be a valid date")
		} else if dob >= today() {
			add("Date of Birth must be a past date")
		} else if dob > time.Now().AddDate(-14, 0, 0).Format("2006-01-02") {
			add("Date of Birth makes the person younger than 14")
		}
	}
	gender := V("Gender")
	for _, g := range genders {
		if strings.EqualFold(g, gender) {
			gender = g
		}
	}
	if gender != "" && !containsStr(genders, gender) {
		add("Gender is one of %s", strings.Join(genders, ", "))
	}
	pan := strings.ToUpper(V("PAN"))
	if pan != "" {
		if !rePAN.MatchString(pan) {
			add("PAN is not in the right format")
		} else if taken, _ := tx.C("employee_gov_id").Exists(ctx, bson.M{"pan": ui2CompSameText(pan)}); taken {
			add("PAN %s is already used by another employee", pan)
		}
	}
	aadhaar := strings.ReplaceAll(V("Aadhaar"), " ", "")
	if aadhaar != "" && !reAadhaar.MatchString(aadhaar) {
		add("Aadhaar must be 12 digits")
	} else if aadhaar != "" {
		if taken, _ := tx.C("employee_gov_id").Exists(ctx, bson.M{"aadhaar": aadhaar}); taken {
			add("Aadhaar %s is already on file for another employee", aadhaar[len(aadhaar)-4:])
		}
	}
	uan, esic := statIDClean(V("UAN")), statIDClean(V("ESIC No")) // "No", "NA": none (REAL-39)
	if uan != "" && !reUAN.MatchString(uan) {
		add("UAN must be 12 digits")
	}
	if esic != "" && !reESIC.MatchString(esic) {
		add("ESIC number must be 10 or 17 digits")
	}
	existingUAN := importUAN(V("Existing UAN?"), uan, add)
	acct, ifsc := V("Account Number"), strings.ToUpper(V("IFSC"))
	if acct != "" && ifsc == "" {
		add("IFSC is required with the account number")
	}
	if ifsc != "" && !reIFSC.MatchString(ifsc) {
		add("IFSC is not in the right format")
	}
	if acct != "" && !reAcct.MatchString(acct) {
		add("Account number must be 6 to 18 digits")
	}
	status, lwd := or(V("Status"), "Active"), ""
	if status != "Active" && status != "Inactive" {
		add("Status must be Active or Inactive")
	}
	if status == "Inactive" {
		if lwd = importDate(V("Last Working Day")); lwd == "" {
			add("Last Working Day is required for an inactive employee")
		} else if doj != "" && lwd < doj {
			add("Last Working Day is before the joining date")
		} else if lwd > today() {
			add("An inactive employee's Last Working Day is today or earlier (a later one is a notice period: import as Active)")
		}
	}
	open := map[string]float64{}
	for _, k := range [][2]string{{"CL", "Casual leave"}, {"PL", "Privilege leave"}, {"SL", "Sick leave"}} {
		if v := V("Opening " + k[0] + " Balance"); v != "" {
			if n, err := strconv.ParseFloat(v, 64); err != nil || n < 0 {
				add("Opening %s Balance must be a number, 0 or more", k[0])
			} else {
				open[k[1]] = n
			}
		}
	}
	sizes, serr := importSizes(ctx, tx, map[string]string{"shirt": V("Shirt / T-shirt Size"), "waist": V("Trouser Waist"), "length": V("Trouser Length"), "shoe": V("Shoe Size (UK)")})
	res.Errors = append(res.Errors, serr...)
	if len(res.Errors) > 0 {
		return res, nil
	}
	// A policy given in the file must be the one the rules give, as on the employee record.
	if polID != "" {
		if p := candidatePolicy(ctx, tx, "", offerJob{Type: typ, Grade: grade, Dept: deptName, Desig: desName, LocID: locID, CityID: cityID}); p != nil && p.ID != polID {
			add("Work Policy “%s” does not match the policy rules for this employee (rules give “%s”). Leave it blank or change the rules", polName, p.Name)
			return res, nil
		}
	}
	id := code
	if id == "" {
		var err error
		if id, err = NextEmployeeCode(ctx, tx, typ); err != nil {
			return res, err
		}
	}
	if apply {
		res.ID = id
	}
	if l2 == "" {
		l2 = l1
	}
	var lwdArg any
	if lwd != "" {
		lwdArg = lwd
	}
	orNull := func(v string) any { // an empty text is stored as null
		if v == "" {
			return nil
		}
		return v
	}
	type insert struct {
		coll string
		doc  bson.M
	}
	writes := []insert{
		{"employee", bson.M{"_id": id, "name": name, "type": typ, "doj": doj, "department_id": deptID, "designation_id": desID, "grade": grade, "location_id": locID,
			"shift_code": shift, "manager_id": l1, "functional_manager_id": l2, "attendance_mode": attMode, "status": status, "lwd": lwdArg, "verified": true,
			"job_role": jobRole, "skill": skill, "existing_uan": existingUAN}},
		{"employee_personal", bson.M{"employee_id": id, "dob": orNull(dob), "gender": orNull(gender), "mobile": orNull(mobile), "email": orNull(email)}},
		{"employee_gov_id", bson.M{"employee_id": id, "pan": orNull(pan), "aadhaar": orNull(aadhaar), "uan": orNull(uan), "esic": orNull(esic)}},
	}
	if def := compStructureDefault(ctx, tx); def != "" { // on the default version from the start
		writes = append(writes, insert{"structure_map", bson.M{"employee_id": id, "from_date": "1900-01-01", "structure_id": def}})
	}
	if acct != "" {
		writes = append(writes, insert{"employee_bank", bson.M{"employee_id": id, "bank_name": or(V("Bank Name"), "Bank"), "account_no": acct, "ifsc": ifsc,
			"from_month": doj[:7], "created_by": c.Name}})
	}
	if d := sizesDoc(id, sizes, c.Name+" ("+c.Role+")", tx.Now()); d != nil {
		writes = append(writes, insert{store.UniformSizeColl, d})
	}
	at := today()
	if doj > at {
		at = doj
	}
	if lwd != "" && lwd < at {
		at = lwd
	}
	for lt, n := range open {
		if ok, err := tx.C("leave_type").Exists(ctx, bson.M{"name": lt}); err != nil {
			return res, err
		} else if ok && leaveTypeOK(typ, lt) { // no opening for a type the employment type does not get
			writes = append(writes, insert{"leave_opening", bson.M{"employee_id": id, "leave_type": lt, "at": at, "balance": n, "created_by": c.Name}})
		}
	}
	for _, w := range writes {
		if _, err := tx.C(w.coll).Insert(ctx, w.doc); err != nil {
			return res, err
		}
	}
	if apply {
		if err := audit(ctx, tx, c, "Imported employee "+name, "employee", id, nil, map[string]any{"type": typ, "doj": doj, "status": status}); err != nil {
			return res, err
		}
		if _, err := importHireNo(ctx, tx, c, id, doj); err != nil { // SI-/OI-/JI- (hirefix.go)
			return res, err
		}
	}
	res.Changes = append(res.Changes, bulkChange{Field: "New employee", From: "", To: fmt.Sprintf("%s, %s, %s", typ, cityName, deptName)})
	if oldGross {
		res.Changes[len(res.Changes)-1].Note = "Monthly Gross ignored: set the salary with Bulk update › Salary breakup or Compensation"
	}
	return res, nil
}
