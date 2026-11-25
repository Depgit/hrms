package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/approval"
	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

// Onboarding (spec 8): offer → approval → release → accept → joining details → Payroll verification → active on
// the joining date. Since 11 Nov 2026 every new hire starts as a shortlisted candidate (onboarding_stages.go) and
// the offer is raised for a selected candidate as OI-0001, without an employee record until the Employee ID is
// due. Offers raised before that (OFR-1001) keep their flow: offer approval makes the pre-boarding employee.

// ---------- Form setup ----------

type onbField struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Section string   `json:"section"`
	Kind    string   `json:"kind"`
	Options *string  `json:"options"`
	On      bool     `json:"on"`
	Sys     bool     `json:"sys"`
	Custom  bool     `json:"custom"`
	Offer   int      `json:"offer"`
	Join    int      `json:"join"`
	Types   []string `json:"types"`
	// FilledBy: "joiner" (the new joiner fills it from their link, joining_link.go) or "hr".
	FilledBy string `json:"filledBy"`
	// Threshold: the PF choice (pfChoice) is asked only above this monthly gross (company_setting.pf_exempt_gross).
	Threshold float64 `json:"threshold,omitempty"`
}

type onbDoc struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	On       bool     `json:"on"`
	Sys      bool     `json:"sys"`
	Offer    int      `json:"offer"`
	Join     int      `json:"join"`
	Types    []string `json:"types"`
	Depts    []string `json:"depts"`
	FilledBy string   `json:"filledBy"`
}

// joinerAllowed: the field may be given to the joiner (store.JoinerAllowed).
func (f onbField) joinerAllowed() bool {
	opts := ""
	if f.Options != nil {
		opts = strings.TrimSpace(*f.Options)
	}
	return store.JoinerAllowed(f.Key, f.Section, f.Kind, f.Sys, opts)
}

// byJoiner: the joiner fills this field, from their link.
func (f onbField) byJoiner() bool { return f.FilledBy == store.FilledByJoiner && f.joinerAllowed() }

// loadSetup reads Onboarding setup: the form's fields and the documents, each in their set order.
func loadSetup(ctx context.Context, q store.Q) ([]onbField, []onbDoc, error) {
	inOrder := options.Find().SetSort(bson.D{{Key: "sort", Value: 1}, {Key: "key", Value: 1}})
	rows, err := q.C("onb_field").Find(ctx, bson.D{}, inOrder)
	if err != nil {
		return nil, nil, err
	}
	fs := make([]onbField, 0, len(rows))
	var lists map[string][]string
	for _, r := range rows {
		f := onbField{Key: str(r["key"]), Label: str(r["label"]), Section: str(r["section"]), Kind: str(r["kind"]),
			Options: onboardingStrPtr(r["options"]), On: r["on_form"] == true, Sys: r["sys"] == true, Custom: r["custom"] == true,
			Offer: toInt(r["offer_state"]), Join: toInt(r["join_state"]), Types: onboardingStrings(r["types"]), FilledBy: str(r["filled_by"])}
		if sizeOfField[f.Key] != "" { // the Uniform section offers the size lists HR Admin keeps (uniform.go)
			if lists == nil {
				lists = store.UniformSizeLists(ctx, q)
			}
			opts := strings.Join(lists[sizeOfField[f.Key]], ",")
			f.Options = &opts
		}
		if o := jobMasterOptions(ctx, q, f.Key); o != nil { // Role and Skill offer the active masters (b1_masters.go)
			f.Options = o
		}
		if f.Key == "pfChoice" {
			f.Threshold = loadPfRules(ctx, q).ExemptGross
		}
		if f.FilledBy == "" || !f.joinerAllowed() { // not set yet (an import), or a field HR always fills
			f.FilledBy = store.DefaultFilledBy(f.Key, f.Section, f.Kind, f.Sys, str(r["options"]))
		}
		fs = append(fs, f)
	}
	rows, err = q.C("onb_document").Find(ctx, bson.D{}, inOrder)
	if err != nil {
		return fs, nil, err
	}
	ds := make([]onbDoc, 0, len(rows))
	for _, r := range rows {
		ds = append(ds, onbDoc{Key: str(r["key"]), Label: str(r["label"]), On: r["on_form"] == true, Sys: r["sys"] == true,
			Offer: toInt(r["offer_state"]), Join: toInt(r["join_state"]), Types: onboardingStrings(r["types"]), Depts: onboardingStrings(r["depts"]),
			FilledBy: str(r["filled_by"])})
		if ds[len(ds)-1].FilledBy == "" {
			ds[len(ds)-1].FilledBy = store.FilledByJoiner
		}
	}
	return fs, ds, nil
}

// ---------- Reading documents (helpers for this file and onboarding_stages.go) ----------

// onboardingStrings is a stored text[] as []string: empty (not nil) for an empty array, nil for NULL, as before.
func onboardingStrings(v any) []string {
	l, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(l))
	for _, x := range l {
		out = append(out, str(x))
	}
	return out
}

// onboardingStrPtr is a nullable text column as *string.
func onboardingStrPtr(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

// onboardingCol is a column of a LEFT JOINed document: nil when nothing joined.
func onboardingCol(r store.Row, col string) any {
	if r == nil {
		return nil
	}
	return r[col]
}

// onboardingDecodeJSON reads a jsonb value into out as scanning the column did: out starts from its zero value.
func onboardingDecodeJSON(v any, out any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// onboardingByID loads the documents of a collection with these ids (a JOIN on the primary key), keyed by id.
// Empty and repeated ids are skipped.
func onboardingByID(ctx context.Context, q store.Q, coll string, ids ...string) (map[string]store.Row, error) {
	out := map[string]store.Row{}
	want, seen := []string{}, map[string]bool{}
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			want = append(want, id)
		}
	}
	if len(want) == 0 {
		return out, nil
	}
	rows, err := q.C(coll).Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: want}}}})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[str(r["id"])] = r
	}
	return out, nil
}

// onboardingOne is the document with this id, or nil.
func onboardingOne(ctx context.Context, q store.Q, coll string, id any) (store.Row, error) {
	return q.C(coll).FindOne(ctx, bson.D{{Key: "_id", Value: id}})
}

// onboardingScope is the caller's data scope (access.Scope.SQL over a record joined to its department) as a filter
// on offer or candidate documents, which carry location_id, manager_id and department_id like an employee record.
func onboardingScope(ctx context.Context, q store.Q, sc access.Scope) (bson.D, error) {
	f := bson.D{}
	switch sc.Access {
	case "all":
	case "locs":
		locs := sc.Locations
		if locs == nil {
			locs = []string{}
		}
		f = append(f, bson.E{Key: "location_id", Value: bson.D{{Key: "$in", Value: locs}}})
	case "team":
		f = append(f, bson.E{Key: "manager_id", Value: sc.EmployeeID})
	default:
		f = append(f, bson.E{Key: "_id", Value: sc.EmployeeID})
	}
	if len(sc.Departments) > 0 {
		ids, err := q.C("department").Distinct(ctx, "_id", bson.D{{Key: "name", Value: bson.D{{Key: "$in", Value: sc.Departments}}}})
		if err != nil {
			return nil, err
		}
		f = append(f, bson.E{Key: "department_id", Value: bson.D{{Key: "$in", Value: ids}}})
	}
	return f, nil
}

// onboardingPlace is a location with the city of its cluster (location JOIN cluster), or nil.
func onboardingPlace(ctx context.Context, q store.Q, locID string) (loc store.Row, cityID string, err error) {
	l, err := onboardingOne(ctx, q, "location", locID)
	if err != nil || l == nil {
		return nil, "", err
	}
	k, err := onboardingOne(ctx, q, "cluster", l["cluster_id"])
	if err != nil || k == nil {
		return nil, "", err
	}
	return l, str(k["city_id"]), nil
}

// onboardingCityLocations are the ids of the locations in a city's clusters, filtered further by extra.
func onboardingCityLocations(ctx context.Context, q store.Q, cityID string, extra bson.D) ([]any, error) {
	clusters, err := q.C("cluster").Distinct(ctx, "_id", bson.D{{Key: "city_id", Value: cityID}})
	if err != nil {
		return nil, err
	}
	f := append(bson.D{{Key: "cluster_id", Value: bson.D{{Key: "$in", Value: clusters}}}}, extra...)
	return q.C("location").Distinct(ctx, "_id", f)
}

// applies: switched on, for this employment type, and asked on the answers so far (asked).
func (f onbField) applies(typ string, data map[string]string) bool {
	if f.Key == "pfChoice" && !calc.RulesFor(calc.EmploymentType(typ)).Statutory {
		return false
	}
	return f.On && containsStr(f.Types, typ) && f.asked(data)
}

// asked: the fields asked only on an answer (owner, 19 Nov 2026): the UAN number when an existing UAN was declared,
// the spouse's name when married, and the PF choice when there is no UAN and the gross is above the threshold, or
// when the offer said No to PF (owner, 26 Nov 2026; pf_offer.go).
func (f onbField) asked(d map[string]string) bool {
	switch f.Key {
	case "uan":
		return d["uanAvail"] == "Yes"
	case "spouse":
		return d["marital"] == "Married"
	case "pfChoice":
		return pfChoiceAsked(d, f.Threshold) || pfOfferNo(d)
	}
	return true
}

// joinAskedErrors are the answers missing or wrong among the fields asked on other answers: the spouse's name when
// married, the PF choice (Yes or No) when it is asked.
func joinAskedErrors(fs []onbField, typ string, d map[string]string) []string {
	errs := []string{}
	for _, f := range fs {
		if (f.Key != "spouse" && f.Key != "pfChoice") || !f.applies(typ, d) {
			continue
		}
		v := strings.TrimSpace(d[f.Key])
		switch {
		case f.Key == "spouse" && v == "":
			errs = append(errs, "Spouse name is required when married")
		case f.Key == "pfChoice" && v != "Yes" && v != "No":
			errs = append(errs, fmt.Sprintf("Answer \"Do you want PF deducted from your salary?\" (Yes or No): no existing UAN and a monthly gross above %s", calc.Inr(f.Threshold)))
		}
	}
	return errs
}

// pfChoiceAsked: "Do you want PF deducted from your salary?" is asked when Existing UAN? is No and the monthly gross
// is above the threshold (gross > threshold).
func pfChoiceAsked(d map[string]string, threshold float64) bool {
	g, err := strconv.ParseFloat(strings.ReplaceAll(d["gross"], ",", ""), 64)
	return d["uanAvail"] == "No" && err == nil && threshold > 0 && g > threshold
}

func (d onbDoc) applies(typ, dept string) bool {
	return d.On && containsStr(d.Types, typ) && (len(d.Depts) == 0 || containsStr(d.Depts, dept))
}

func (s *Server) onbSetupGet(r *http.Request, c *Ctx) (any, error) {
	if c.Level("onb.pipeline") < 1 && c.Level("onb.raise") < 1 && c.Level("onb.setup") < 1 {
		return nil, forbidden("")
	}
	fs, ds, err := loadSetup(r.Context(), s.st.DB)
	return map[string]any{"fields": fs, "docs": ds}, err
}

type setupPatch struct {
	Label    *string   `json:"label"`
	On       *bool     `json:"on"`
	Offer    *int      `json:"offer"`
	Join     *int      `json:"join"`
	Types    *[]string `json:"types"`
	Depts    *[]string `json:"depts"`
	FilledBy *string   `json:"filledBy"` // joiner | hr
}

var empTypes = []string{"Permanent", "Retainer", "Contractual"}

func (p setupPatch) check(sys bool) error {
	for _, v := range []*int{p.Offer, p.Join} {
		if v != nil && (*v < 0 || *v > 2) {
			return bad("Choose Not needed, Optional or Mandatory")
		}
	}
	if sys && (p.On != nil && !*p.On || p.Offer != nil || p.Types != nil) {
		return bad("System items are always asked at offer release, for every type")
	}
	if p.Types != nil {
		if len(*p.Types) == 0 {
			return bad("Keep at least one employment type")
		}
		for _, t := range *p.Types {
			if !containsStr(empTypes, t) {
				return bad("Unknown employment type %s", t)
			}
		}
	}
	return nil
}

func (s *Server) onbSetupPatch(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("onb.setup", 2); err != nil {
		return nil, err
	}
	table := map[string]string{"fields": "onb_field", "docs": "onb_document"}[r.PathValue("kind")]
	if table == "" {
		return nil, notFound("")
	}
	var b setupPatch
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx, key := r.Context(), r.PathValue("key")
	item, err := s.st.DB.C(table).FindOne(ctx, bson.D{{Key: "key", Value: key}})
	if err != nil || item == nil {
		return nil, notFound("")
	}
	if err := b.check(item["sys"] == true); err != nil {
		return nil, err
	}
	if b.Depts != nil && table != "onb_document" {
		return nil, bad("Only documents can be limited to departments")
	}
	if b.Depts != nil { // each a department in the master (HIRE-236)
		for _, d := range *b.Depts {
			if ok, _ := s.st.DB.C("department").Exists(ctx, bson.M{"name": d}); !ok {
				return nil, bad("Department “%s” is not in the master", d)
			}
		}
	}
	if b.Label != nil && len([]rune(strings.TrimSpace(*b.Label))) > 80 { // as Add allows (HIRE-237)
		return nil, bad("A label can be at most 80 characters")
	}
	if b.FilledBy != nil {
		if *b.FilledBy != store.FilledByJoiner && *b.FilledBy != store.FilledByHR {
			return nil, bad("Choose who fills it: the joiner or HR")
		}
		if table == "onb_field" && *b.FilledBy == store.FilledByJoiner &&
			!store.JoinerAllowed(key, str(item["section"]), str(item["kind"]), item["sys"] == true, strings.TrimSpace(str(item["options"]))) {
			return nil, bad("%s is always filled by HR", item["label"])
		}
	}
	// Only what was sent changes; a blank label keeps the old one.
	set := bson.D{}
	if b.Label != nil && strings.TrimSpace(*b.Label) != "" {
		set = append(set, bson.E{Key: "label", Value: strings.TrimSpace(*b.Label)})
	}
	if b.On != nil {
		set = append(set, bson.E{Key: "on_form", Value: *b.On})
	}
	if b.Offer != nil {
		set = append(set, bson.E{Key: "offer_state", Value: *b.Offer})
	}
	if b.Join != nil {
		set = append(set, bson.E{Key: "join_state", Value: *b.Join})
	}
	if b.Types != nil {
		set = append(set, bson.E{Key: "types", Value: *b.Types})
	}
	if b.Depts != nil {
		set = append(set, bson.E{Key: "depts", Value: *b.Depts})
	}
	if b.FilledBy != nil {
		set = append(set, bson.E{Key: "filled_by", Value: *b.FilledBy})
	}
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		if len(set) > 0 {
			if _, err := tx.C(table).Update(ctx, bson.D{{Key: "key", Value: key}}, bson.D{{Key: "$set", Value: set}}); err != nil {
				return err
			}
		}
		return audit(ctx, tx, c, "Changed onboarding "+map[string]string{"onb_field": "field", "onb_document": "document"}[table]+" "+key, table, key, nil, b)
	})
}

var setupSections = []string{"Personal", "Identity and statutory", "Bank", "Job", "Location and reporting", "Benefits", "Uniform", "Hiring", "Compensation"}

func (s *Server) onbSetupAdd(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("onb.setup", 2); err != nil {
		return nil, err
	}
	var b struct {
		Label   string `json:"label"`
		Section string `json:"section"`
		Kind    string `json:"kind"`
		Options string `json:"options"`
		Offer   int    `json:"offer"`
		Join    int    `json:"join"`
		// FilledBy: joiner or hr; empty for the default (a field by its section, a document the joiner).
		FilledBy string `json:"filledBy"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Label = strings.TrimSpace(b.Label)
	if b.FilledBy != "" && b.FilledBy != store.FilledByJoiner && b.FilledBy != store.FilledByHR {
		return nil, bad("Choose who fills it: the joiner or HR")
	}
	if b.Label == "" || len(b.Label) > 80 {
		return nil, bad("Enter a label")
	}
	if b.Offer < 0 || b.Offer > 2 || b.Join < 0 || b.Join > 2 {
		return nil, bad("Choose Not needed, Optional or Mandatory")
	}
	ctx, kind := r.Context(), r.PathValue("kind")
	key := "x_" + randomHex(4)
	return map[string]string{"key": key}, s.st.Tx(ctx, func(tx store.Q) error {
		switch kind {
		case "fields":
			if !containsStr(setupSections, b.Section) {
				return bad("Choose a section")
			}
			if !containsStr([]string{"text", "number", "date", "sel"}, b.Kind) {
				return bad("Choose text, number, date or drop-down")
			}
			if b.Kind == "sel" && strings.TrimSpace(b.Options) == "" {
				return bad("Add the drop-down options")
			}
			sort, err := onboardingNextSort(ctx, tx, "onb_field")
			if err != nil {
				return err
			}
			opts := strings.TrimSpace(b.Options)
			by := b.FilledBy
			if by == "" {
				by = store.DefaultFilledBy(key, b.Section, b.Kind, false, opts)
			} else if by == store.FilledByJoiner && !store.JoinerAllowed(key, b.Section, b.Kind, false, opts) {
				return bad("Fields in %s are filled by HR", b.Section)
			}
			if _, err := tx.C("onb_field").Insert(ctx, bson.M{"key": key, "label": b.Label, "section": b.Section, "kind": b.Kind,
				"options": strPtr(opts), "custom": true, "offer_state": b.Offer, "join_state": b.Join, "sort": sort, "filled_by": by}); err != nil {
				return err
			}
		case "docs":
			sort, err := onboardingNextSort(ctx, tx, "onb_document")
			if err != nil {
				return err
			}
			by := b.FilledBy
			if by == "" {
				by = store.FilledByJoiner
			}
			if _, err := tx.C("onb_document").Insert(ctx, bson.M{"key": key, "label": b.Label, "offer_state": b.Offer, "join_state": b.Join, "sort": sort, "filled_by": by}); err != nil {
				return err
			}
		default:
			return notFound("")
		}
		return audit(ctx, tx, c, "Added onboarding "+strings.TrimSuffix(kind, "s")+" “"+b.Label+"”", "onb_"+kind, key, nil, b)
	})
}

// onboardingNextSort puts a new setup item last: one more than the highest sort so far (1 for the first).
func onboardingNextSort(ctx context.Context, tx store.Q, table string) (int, error) {
	last, err := tx.C(table).FindOne(ctx, bson.D{}, options.FindOne().SetSort(bson.D{{Key: "sort", Value: -1}}))
	if err != nil || last == nil {
		return 1, err
	}
	return toInt(last["sort"]) + 1, nil
}

// ---------- Offer validation, pay and route ----------

type offerJob struct {
	Name, Type, DOJ                 string
	DeptID, Dept, DesigID, Desig    string
	Corporate                       bool
	Grade, CityID, City, LocID, Loc string
	Gross                           float64
	TopUp                           float64 // meal and accommodation deductions: a minimum-wage top-up, not company cost
	Reimb                           float64 // the Reimbursement ₹ a month, outside the gross (reimb.go): company cost
	Manager, FManager               *string
	RejoinOf                        string
	Rehire                          string
}

var (
	reDigits12 = regexp.MustCompile(`^\d{12}$`)
	reUAN      = regexp.MustCompile(`^\d{12}$`)
)

// cleanData turns the posted form into trimmed strings.
func cleanData(in map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		if v == nil || len(k) > 40 {
			continue
		}
		s := strings.TrimSpace(fmt.Sprint(v))
		if f, ok := v.(float64); ok {
			s = strconv.FormatFloat(f, 'f', -1, 64)
		}
		if len(s) > 500 {
			s = s[:500]
		}
		out[k] = s
	}
	return out
}

// checkFormat validates the formats of whatever values are present (spec 8.3).
func checkFormat(d map[string]string, errs *[]string) {
	add := func(s string) { *errs = append(*errs, s) }
	if v := strings.ReplaceAll(d["aadhaar"], " ", ""); v != "" && !reDigits12.MatchString(v) {
		add("Aadhaar must be 12 digits")
	}
	if v := strings.ToUpper(d["pan"]); v != "" && !rePAN.MatchString(v) {
		add("PAN format looks wrong (ABCDE1234F)")
	}
	if v := strings.ToUpper(d["ifsc"]); v != "" && !reIFSC.MatchString(v) {
		add("IFSC format looks wrong")
	}
	if v := d["acct"]; v != "" && !reAcct.MatchString(v) {
		add("Account number must be 6 to 18 digits")
	}
	if v := d["email"]; v != "" && !reEmail.MatchString(v) {
		add("Personal email looks invalid")
	}
	for _, k := range []string{"phone", "ecPhone"} {
		if v := strings.ReplaceAll(d[k], " ", ""); v != "" && !reMobile.MatchString(v) {
			add(map[string]string{"phone": "Mobile number", "ecPhone": "Emergency contact mobile"}[k] + " must be 10 digits")
		}
	}
	if v := d["uan"]; v != "" && !reUAN.MatchString(v) {
		add("UAN must be 12 digits")
	}
	if v := d["dob"]; v != "" && (!isDate(v) || v >= today()) {
		add("Date of birth must be a past date")
	}
	for _, k := range []string{"varPct", "tdsPct"} {
		if v := d[k]; v != "" {
			if f, err := strconv.ParseFloat(v, 64); err != nil || f < 0 || f > 100 {
				add("Percentages must be between 0 and 100")
			}
		}
	}
	for _, k := range []string{"dMeal", "dAccom"} {
		if v := d[k]; v != "" {
			if f, err := strconv.ParseFloat(v, 64); err != nil || f < 0 {
				add("Deductions must be zero or more")
			}
		}
	}
	if v := d["varCycle"]; v != "" && !containsStr([]string{"1", "3", "6", "12"}, v) {
		add("Variable payout is every 1, 3, 6 or 12 months")
	}
	if v := d["varMode"]; v != "" && v != "pct" && v != "amount" {
		add("The variable pay target is in % of gross or in ₹ a month")
	}
	if v := d["varAmt"]; v != "" {
		if f, err := strconv.ParseFloat(v, 64); err != nil || f < 0 {
			add("The variable pay target must be zero or more")
		}
	}
}

// offerVar is the offer's variable pay target (17 Nov 2026): mode "pct" (varPct, % of the fixed gross) or "amount"
// (varAmt, ₹ a month). An offer saved before the mode was asked counts as ₹ when it has varAmt, else %.
func offerVar(d map[string]string) (string, *float64) {
	mode := d["varMode"]
	if mode == "" {
		mode = "pct"
		if d["varAmt"] != "" {
			mode = "amount"
		}
	}
	if mode == "pct" {
		return mode, numOr(d["varPct"])
	}
	return mode, numOr(d["varAmt"])
}

// resolveJob looks up the job and place of an offer, collecting every problem (obErrors).
func resolveJob(ctx context.Context, q store.Q, d map[string]string, errs *[]string) offerJob {
	add := func(s string, a ...any) { *errs = append(*errs, fmt.Sprintf(s, a...)) }
	j := offerJob{Name: strings.TrimSpace(d["first"] + " " + d["last"]), Type: d["type"], DOJ: d["doj"], Grade: d["grade"]}
	if !containsStr(empTypes, j.Type) {
		add("Employment type is required")
	}
	if d["doj"] != "" && !isDate(d["doj"]) {
		add("Date of joining must be a date like 2026-12-01")
	}
	if d["dept"] != "" {
		if x, _ := q.C("department").FindOne(ctx, bson.D{{Key: "name", Value: d["dept"]}}); x == nil {
			add("Department “%s” is not in Masters", d["dept"])
		} else if j.DeptID, j.Dept, j.Corporate = str(x["id"]), str(x["name"]), x["corporate"] == true; x["status"] != "Active" {
			add("Department “%s” is inactive now. Choose an active one", j.Dept)
		}
	}
	if d["desig"] != "" && j.DeptID != "" {
		if x, _ := q.C("designation").FindOne(ctx, bson.D{{Key: "name", Value: d["desig"]}, {Key: "department_id", Value: j.DeptID}}); x == nil {
			add("Designation “%s” does not exist under %s", d["desig"], j.Dept)
		} else if j.DesigID, j.Desig = str(x["id"]), str(x["name"]); x["status"] != "Active" {
			add("Designation “%s” is inactive now. Choose an active one", j.Desig)
		} else if j.Grade != "" && !containsStr(onboardingStrings(x["grades"]), j.Grade) {
			add("Grade %s is not mapped to %s", j.Grade, j.Desig)
		}
	}
	if j.Grade != "" {
		if x, _ := q.C("grade").FindOne(ctx, bson.D{{Key: "name", Value: j.Grade}}); x == nil {
			add("Grade %s is not in Masters", j.Grade)
		} else if x["status"] != "Active" {
			add("Grade “%s” is inactive now. Choose an active one", j.Grade)
		}
	}
	if d["city"] != "" {
		if x, _ := onboardingOne(ctx, q, "city", d["city"]); x == nil {
			add("Choose the work city")
		} else if j.CityID, j.City = str(x["id"]), str(x["name"]); x["status"] != "Active" {
			add("City “%s” is inactive now. Choose an active one", j.City)
		}
	}
	if d["loc"] != "" {
		if x, cityID, _ := onboardingPlace(ctx, q, d["loc"]); x == nil {
			add("Choose the reporting location")
		} else if j.LocID, j.Loc = str(x["id"]), str(x["name"]); x["status"] != "Active" {
			add("Location “%s” is inactive now. Choose an active one", j.Loc)
		} else if j.CityID != "" && cityID != j.CityID {
			add("Location “%s” is not in %s", j.Loc, j.City)
		}
	}
	if d["gross"] != "" {
		g, err := strconv.ParseFloat(d["gross"], 64)
		if err != nil || g <= 0 {
			add("Monthly gross must be a number above zero")
		}
		j.Gross = g
	}
	if calc.EmploymentType(j.Type) != calc.Retainer {
		j.Reimb = math.Max(0, offerReimb(d))
	}
	if calc.RulesFor(calc.EmploymentType(j.Type)).Ded == "all" {
		for _, k := range []string{"dMeal", "dAccom"} {
			if v, err := strconv.ParseFloat(d[k], 64); err == nil && v > 0 {
				j.TopUp += v
			}
		}
	}
	for _, k := range []string{"l1", "l2"} {
		if v := d[k]; v != "" {
			if ok, _ := q.C("employee").Exists(ctx, bson.D{{Key: "_id", Value: v}, {Key: "status", Value: "Active"}}); !ok {
				add("%s must be an active employee", map[string]string{"l1": "Reporting manager (L1)", "l2": "Functional manager (L2)"}[k])
			} else if k == "l1" {
				j.Manager = &v
			} else {
				j.FManager = &v
			}
		}
	}
	if v := d["shift"]; v != "" && j.LocID != "" {
		// The shift must apply to the department and location (its rows, shift_maps.go).
		if x, _ := q.C("shift").FindOne(ctx, bson.D{{Key: "code", Value: v}, {Key: "status", Value: "Active"}}); x == nil {
			add("Shift %s is not an active shift", v)
		} else if ok, _ := shiftRowFor(ctx, q, str(x["id"]), j.DeptID, j.LocID); !ok {
			add("%s", shiftNotForMsg(v, j.Dept, j.Loc))
		}
	}
	if v := d["attMode"]; v != "" {
		if ok, _ := q.C("simple_master").Exists(ctx, bson.D{{Key: "kind", Value: "attendance_mode"}, {Key: "name", Value: v}, {Key: "status", Value: "Active"}}); !ok {
			add("Attendance mode “%s” is not active", v)
		}
	}
	if v := d["structId"]; v != "" {
		if ok, _ := q.C("salary_structure").Exists(ctx, bson.D{{Key: "_id", Value: v}, {Key: "status", Value: "Active"}}); !ok {
			add("Choose an active salary structure")
		}
	}
	if v := d["endDate"]; v != "" && (!isDate(v) || (j.DOJ != "" && v <= j.DOJ)) {
		add("Contract end date must be after the joining date")
	}
	jobMasterErrors(ctx, q, d, add) // Role and Skill (b1_masters.go)
	if v := d["rejoinOf"]; v != "" {
		if e, _ := onboardingOne(ctx, q, "employee", v); e == nil {
			add("The earlier employee record %s was not found", v)
		} else if e["status"] != "Inactive" {
			add("%s is not inactive, so they cannot rejoin", v)
		} else {
			j.RejoinOf = v
			j.Rehire = str(e["rehire"])
			// Separation stores Eligible | Not eligible | Only with CHRO approval ("No" from older records); the
			// shortlist and the offer both resolve the job here, so a leaver not eligible is refused at both (EXT-46).
			if j.Rehire == "Not eligible" || j.Rehire == "No" {
				add("%s is marked not eligible for rehire", v)
			}
		}
	}
	return j
}

// offerProblems checks the offer-stage form: required fields, formats, masters and documents.
func offerProblems(ctx context.Context, q store.Q, d map[string]string, docs map[string]store.Row) (offerJob, []string, error) {
	fs, ds, err := loadSetup(ctx, q)
	if err != nil {
		return offerJob{}, nil, err
	}
	errs := []string{}
	typ := d["type"]
	for _, f := range fs {
		if f.Kind == "flow" || f.Kind == "policy" || f.Offer != 2 || !f.applies(typ, d) {
			continue
		}
		if d[f.Key] == "" {
			errs = append(errs, f.Label+" is required")
		}
	}
	checkFormat(d, &errs)
	j := resolveJob(ctx, q, d, &errs)
	offerPfProblems(ctx, q, j.Type, j.Gross, d, &errs)      // "Does the employee want PF?" (pf_offer.go)
	jobMasterRequired(d, &errs)                             // Role and Skill, always, like the department (hirefix.go)
	offerReimbProblems(ctx, q, j, d, &errs)                 // the Reimbursement: zero or more, on a structure that has it (reimb.go)
	if err := accomProblems(ctx, q, d, &errs); err != nil { // Company accommodation: Yes needs a cluster with units (hst_auto.go)
		return offerJob{}, nil, err
	}
	for _, doc := range ds {
		if doc.Offer == 2 && doc.applies(typ, j.Dept) && docs[doc.Key] == nil {
			errs = append(errs, doc.Label+" must be uploaded before the offer is released")
		}
	}
	return j, dedupe(errs), nil
}

func dedupe(l []string) []string {
	seen, out := map[string]bool{}, []string{}
	for _, x := range l {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func numOr(v string) *float64 {
	if v == "" {
		return nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return nil
	}
	return &f
}

func intOr(v string) *int {
	n, err := strconv.Atoi(v)
	if err != nil {
		return nil
	}
	return &n
}

// offerPay works out the breakup for the offer: gross only, the rest from the structure.
func offerPay(ctx context.Context, q store.Q, j offerJob, d map[string]string) calc.Result {
	st := calc.DefaultStructure
	which := bson.D{{Key: "is_default", Value: true}}
	if d["structId"] != "" {
		which = bson.D{{Key: "_id", Value: d["structId"]}}
	}
	if x, _ := q.C("salary_structure").FindOne(ctx, which); x != nil {
		st = calc.Structure{}
		_ = onboardingDecodeJSON(x["rates"], &st)
	}
	in := calc.Input{Type: calc.EmploymentType(j.Type), Gross: j.Gross, Structure: &st, EmpAmts: reimbAmts(offerReimb(d)), AllIn: offerAllIn(ctx, q, j)}
	t := calc.RulesFor(calc.EmploymentType(j.Type))
	if t.Variable && !st.Designed {
		in.VarPct, in.VarCycle = numOr(d["varPct"]), intOr(d["varCycle"])
	}
	if t.Ded == "all" {
		if v := numOr(d["dMeal"]); v != nil {
			in.Meal = *v
		}
		if v := numOr(d["dAccom"]); v != nil {
			in.Accom = *v
		}
	}
	if t.Ded == "tds" {
		in.TdsPct = numOr(d["tdsPct"])
	}
	// Variable pay as a target (paycomp.go): in % of the fixed gross or ₹ a month; the components give the breakup.
	if mode, v := offerVar(d); st.Designed && t.Variable && v != nil && *v >= 0 {
		if mode == "pct" {
			in.VarTargetPct = v
		} else {
			in.VarTarget, in.CTC = v, j.Gross+*v
		}
	}
	if j.LocID != "" {
		in.Lwf = lwfFor(ctx, q, j.LocID, today())
	}
	// The candidate chose no PF (on the offer or the joining form, no existing UAN, gross above the threshold;
	// pfuan.go, pf_offer.go): PF exempt, the fixed gross all Basic.
	if t.Statutory && offerPfOptOut(d, j.Gross, loadPfRules(ctx, q).ExemptGross) {
		in.PfExempt, in.AllBasic = true, true
	}
	if isDate(j.DOJ) {
		m, _ := strconv.Atoi(j.DOJ[5:7])
		in.JoinMonth = m
	}
	return calc.Calc(in)
}

type bandInfo struct {
	Min      float64 `json:"min"`
	Max      float64 `json:"max"`
	Position string  `json:"position"` // below, within, above
}

func bandOf(ctx context.Context, q store.Q, cityID, grade string, gross float64) *bandInfo {
	x, _ := q.C("pay_band").FindOne(ctx, bson.D{{Key: "city_id", Value: cityID}, {Key: "grade", Value: grade}})
	if x == nil {
		return nil
	}
	b := bandInfo{Position: "within"}
	b.Min, _ = x["min_gross"].(float64)
	b.Max, _ = x["max_gross"].(float64)
	if gross < b.Min {
		b.Position = "below"
	} else if gross > b.Max {
		b.Position = "above"
	}
	return &b
}

// budgetOver lists the budgets this hire would go past (spec 13): city, cluster and location headcount (the seats in
// use as the cost console's headcount plan counts them: people in post, offers in flight and offers in approval;
// cost.go seatsUsed) and the location's monthly cost budget: the total monthly cost to the company of the people in
// post plus this hire's, salary without the minimum-wage top-ups plus the employer's contributions, as Cost and
// budget counts it (budget_cost.go; owner, 18 Nov 2026).
func budgetOver(ctx context.Context, q store.Q, j offerJob, exceptOffer string) []string {
	out := []string{}
	// The headcount is only counted against a budget that is set (above zero).
	if city, _ := onboardingOne(ctx, q, "city", j.CityID); city != nil {
		if budget := int64(toInt(city["headcount_budget"])); budget > 0 {
			if locs, err := onboardingCityLocations(ctx, q, j.CityID, nil); err == nil {
				if hc, err := onboardingHeadcount(ctx, q, locs, exceptOffer); err == nil && hc+1 > budget {
					out = append(out, "city headcount budget")
				}
			}
		}
	}
	if loc, _ := onboardingOne(ctx, q, "location", j.LocID); loc != nil {
		if cl, _ := onboardingOne(ctx, q, "cluster", str(loc["cluster_id"])); cl != nil {
			if budget := int64(toInt(cl["headcount_budget"])); budget > 0 {
				if locs, err := q.C("location").Distinct(ctx, "_id", bson.D{{Key: "cluster_id", Value: cl["id"]}}); err == nil {
					if hc, err := onboardingHeadcount(ctx, q, locs, exceptOffer); err == nil && hc+1 > budget {
						out = append(out, "cluster headcount budget")
					}
				}
			}
		}
		budget := int64(toInt(loc["headcount_budget"]))
		payBudget, _ := loc["payroll_budget"].(float64)
		hc, cost := int64(0), 0.0
		var err error
		if budget > 0 {
			hc, err = onboardingHeadcount(ctx, q, []any{j.LocID}, exceptOffer)
		}
		if err == nil && payBudget > 0 {
			var by map[string]float64
			if by, err = locMonthlyCost(ctx, q, bson.M{"location_id": j.LocID}); err == nil {
				cost = by[j.LocID] + offerMonthlyCost(ctx, q, j)
			}
		}
		if err == nil {
			if budget > 0 && hc+1 > budget {
				out = append(out, "location headcount budget")
			}
			if payBudget > 0 && cost > payBudget {
				out = append(out, "location cost budget (CTC)")
			}
		}
	}
	return out
}

// onboardingHeadcount is the seats in use at these locations (cost.go seatsUsed): people in post, offers in flight
// (accepted, not joined; pre-boarding records) and offers waiting for approval, leaving out one offer.
func onboardingHeadcount(ctx context.Context, q store.Q, locs []any, exceptOffer string) (int64, error) {
	use, err := seatsUsed(ctx, q, locs, exceptOffer)
	if err != nil {
		return 0, err
	}
	n := int64(0)
	for _, u := range use {
		n += u.taken()
	}
	return n, nil
}

// onboardingPayrollCost is the monthly gross of the active people at a location: each one's latest gross_history row
// from this month or earlier (in the business zone). Added up in paise, as the numeric sum was exact.
func onboardingPayrollCost(ctx context.Context, q store.Q, loc string) (float64, error) {
	emps, err := q.C("employee").Distinct(ctx, "_id", bson.D{{Key: "location_id", Value: loc}, {Key: "status", Value: "Active"}})
	if err != nil || len(emps) == 0 {
		return 0, err
	}
	month := store.DateOf(q.Now())[:7]
	rows, err := q.C("gross_history").Find(ctx, bson.D{{Key: "employee_id", Value: bson.D{{Key: "$in", Value: emps}}}, {Key: "from_month", Value: bson.D{{Key: "$lte", Value: month}}}},
		options.Find().SetSort(bson.D{{Key: "employee_id", Value: 1}, {Key: "from_month", Value: -1}}).SetProjection(bson.D{{Key: "employee_id", Value: 1}, {Key: "gross", Value: 1}}))
	if err != nil {
		return 0, err
	}
	paise, seen := int64(0), map[string]bool{}
	for _, r := range rows {
		if emp := str(r["employee_id"]); !seen[emp] {
			seen[emp] = true
			g, _ := r["gross"].(float64)
			paise += int64(math.Round(g * 100))
		}
	}
	tops, err := minWageTopUps(ctx, q, emps)
	if err != nil {
		return 0, err
	}
	for _, t := range tops {
		paise -= int64(math.Round(t * 100))
	}
	return float64(paise) / 100, nil
}

// minWageTopUps is each employee's meal and accommodation deduction (employee_pay), for the types the deductions
// apply to: HR raises the gross by them and deducts them back to meet the minimum wage, so they are no company cost
// and count against no budget (owner, 17 Nov 2026).
func minWageTopUps(ctx context.Context, q store.Q, ids []any) (map[string]float64, error) {
	out := map[string]float64{}
	if len(ids) == 0 {
		return out, nil
	}
	pays, err := q.C("employee_pay").Find(ctx, bson.D{{Key: "employee_id", Value: bson.D{{Key: "$in", Value: ids}}}},
		options.Find().SetProjection(bson.D{{Key: "employee_id", Value: 1}, {Key: "meal", Value: 1}, {Key: "accommodation", Value: 1}}))
	if err != nil || len(pays) == 0 {
		return out, err
	}
	emps, err := q.C("employee").Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}}, options.Find().SetProjection(bson.D{{Key: "type", Value: 1}}))
	if err != nil {
		return nil, err
	}
	typ := map[string]string{}
	for _, e := range emps {
		typ[str(e["id"])] = str(e["type"])
	}
	for _, p := range pays {
		id := str(p["employee_id"])
		if calc.RulesFor(calc.EmploymentType(typ[id])).Ded != "all" {
			continue
		}
		if t := payengineFloat(p["meal"]) + payengineFloat(p["accommodation"]); t > 0 {
			out[id] = t
		}
	}
	return out, nil
}

// offerFacts are the condition facts for the offer approval.
func offerFacts(j offerJob, band *bandInfo, over []string) approval.Facts {
	f := approval.Facts{Grade: j.Grade, AboveBudget: len(over) > 0 || (band != nil && band.Position == "above"),
		BandX: band != nil && band.Position != "within", RehireCHRO: j.Rehire == "Only with CHRO approval"}
	return f
}

type offerBody struct {
	Data      map[string]any `json:"data"`
	Draft     string         `json:"draft"`     // documents uploaded before the offer was saved (offers before stages)
	Candidate string         `json:"candidate"` // the selected candidate (SI-0001) the offer is for
	AtJoining bool           `json:"atJoining"` // "Raise offer at joining" (offer_at_joining.go)
}

// offerInputDocs are the documents an offer form sees: a candidate's (owner SI-0001), else a draft's.
func offerInputDocs(ctx context.Context, q store.Q, b offerBody) map[string]store.Row {
	docs := map[string]store.Row{}
	switch {
	case b.Candidate != "":
		docs, _ = docsOf(ctx, q, "candidate", b.Candidate)
	case draftOwner(b.Draft):
		docs, _ = docsOf(ctx, q, "offer", b.Draft)
	}
	return docs
}

// offerDocOwner is where an offer's documents are kept: with the candidate (SI-0001), or with an offer raised before
// stages (OFR-1001).
func offerDocOwner(o store.Row) (string, string) {
	if cand := str(o["candidate_id"]); cand != "" {
		return "candidate", cand
	}
	return "offer", str(o["id"])
}

func (s *Server) offerCheck(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("onb.raise", 2); err != nil {
		return nil, err
	}
	var b offerBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	d := cleanData(b.Data)
	if b.Candidate != "" {
		cand, err := s.offerCandidate(ctx, s.st.DB, c, b.Candidate)
		if err != nil {
			return nil, err
		}
		b.Candidate = str(cand["id"])
		candidateOfferData(cand, d)
	}
	docs := offerInputDocs(ctx, s.st.DB, b)
	j, errs, err := offerProblems(ctx, s.st.DB, d, docs)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"errors": errs, "docs": docs, "pfLimit": loadPfRules(ctx, s.st.DB).ExemptGross}
	if j.Type != "" && j.Gross > 0 {
		res := offerPay(ctx, s.st.DB, j, d)
		out["breakup"], out["ctcYear"] = res, calc.Round2(res.CTC*12)
	}
	var band *bandInfo
	if j.CityID != "" && j.Grade != "" {
		band = bandOf(ctx, s.st.DB, j.CityID, j.Grade, j.Gross)
		out["band"] = band
	}
	// The approval route shows from the start, as the prototype's side card: before a reporting location is picked it
	// is worked out for the city's first location, and the budget check waits for the location and department.
	loc := j.LocID
	if loc == "" && j.CityID != "" {
		if locs, err := onboardingCityLocations(ctx, s.st.DB, j.CityID, bson.D{{Key: "status", Value: "Active"}}); err == nil && len(locs) > 0 {
			first, _ := s.st.DB.C("location").FindOne(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: locs}}}},
				options.FindOne().SetSort(bson.D{{Key: "name", Value: 1}}))
			loc = str(onboardingCol(first, "id"))
		}
	}
	if loc != "" {
		over := []string{}
		if j.LocID != "" && j.DeptID != "" {
			over = budgetOver(ctx, s.st.DB, j, "")
			out["budget"] = over
		}
		f := offerFacts(j, band, over)
		f.InitiatorRole = c.Role
		subj := &subject{ID: "OFR-new", Name: j.Name, Grade: j.Grade, LocationID: loc, Department: j.Dept, Corporate: j.Corporate, ManagerID: j.Manager}
		if p, err := planFor(ctx, s.st.DB, "joining", subj, f); err == nil {
			out["plan"] = p
		}
		if j.DeptID != "" {
			out["flow"] = map[bool]string{true: "Corporate", false: "Operations"}[j.Corporate]
		}
	}
	// How many mandatory items are left for the joining stage.
	fs, ds, _ := loadSetup(ctx, s.st.DB)
	later := 0
	for _, f := range fs {
		if f.Join == 2 && f.applies(j.Type, d) {
			later++
		}
	}
	for _, doc := range ds {
		if doc.Join == 2 && doc.applies(j.Type, j.Dept) {
			later++
		}
	}
	out["later"] = later
	s.offerExtras(ctx, c, j, b.Draft, out)
	if b.Candidate != "" {
		out["asked"] = askedDocs(ctx, s.st.DB, b.Candidate, "")
	}
	return out, nil
}

func (s *Server) offerCreate(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("onb.raise", 2); err != nil {
		return nil, err
	}
	var b offerBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	// Every new hire starts at Shortlist: an offer is raised only for a candidate the hiring manager selected.
	if b.Candidate == "" {
		return nil, bad("Every new hire starts with a shortlist. Raise the offer from a selected candidate in Onboarding")
	}
	cand, err := s.offerCandidate(ctx, s.st.DB, c, b.Candidate)
	if err != nil {
		return nil, err
	}
	b.Candidate = str(cand["id"])
	if b.AtJoining { // offer at joining: chosen before, raised on the joining day (offer_at_joining.go)
		if !oajOpen(cand) {
			return nil, bad("Choose Offer at joining for %s first", b.Candidate)
		}
		if doj := oajDOJ(cand); today() < doj {
			return nil, bad("The joining day is %s. Raise the offer at joining on that day, or use Reschedule joining", longDate(doj))
		}
	}
	d := cleanData(b.Data)
	candidateOfferData(cand, d)
	docs := offerInputDocs(ctx, s.st.DB, b)
	j, errs, err := offerProblems(ctx, s.st.DB, d, docs)
	if err != nil {
		return nil, err
	}
	if len(errs) > 0 {
		return nil, badD(fmt.Sprintf("%d thing%s to fix before sending", len(errs), map[bool]string{true: "", false: "s"}[len(errs) == 1]), errs)
	}
	offerPfData(j.Type, d) // the PF answer as saved, and the joining form's question prefilled (pf_offer.go)
	band := bandOf(ctx, s.st.DB, j.CityID, j.Grade, j.Gross)
	over := budgetOver(ctx, s.st.DB, j, "")
	pos := "none"
	if band != nil {
		pos = band.Position
	}
	var out map[string]any
	err = s.st.Tx(ctx, func(tx store.Q) error {
		// The Offer ID has the candidate's number: SI-0001 → OI-0001. A revised offer (after one was not approved)
		// keeps it and goes for approval again.
		id := stageID("OI", toInt(cand["no"]))
		if _, err := tx.C("candidate").Lock(ctx, bson.D{{Key: "_id", Value: b.Candidate}}); err != nil {
			return err
		}
		// An offer already raised blocks another, unless its approval was rejected or withdrawn.
		prev, err := onboardingOne(ctx, tx, "offer", id)
		if err != nil {
			return err
		}
		if prev != nil {
			req, err := onboardingOne(ctx, tx, "approval_request", prev["request_id"])
			if err != nil {
				return err
			}
			// A question about the breakup is answered with a revised offer: a new version (hiring.go).
			asked := prev["query_at"] != nil && prev["accepted_at"] == nil && prev["declined_at"] == nil && prev["lapsed_at"] == nil
			if st := onboardingCol(req, "status"); st != "Rejected" && st != "Cancelled" && !asked {
				return bad("The offer for %s is already raised", b.Candidate)
			}
		}
		version := 1
		if prev != nil {
			version = max(1, toInt(prev["version"])) + 1
		}
		d["pan"], d["ifsc"] = strings.ToUpper(d["pan"]), strings.ToUpper(d["ifsc"])
		offer := bson.D{{Key: "candidate_name", Value: j.Name}, {Key: "type", Value: j.Type}, {Key: "doj", Value: j.DOJ}, {Key: "department_id", Value: j.DeptID},
			{Key: "designation_id", Value: j.DesigID}, {Key: "grade", Value: j.Grade}, {Key: "location_id", Value: j.LocID}, {Key: "manager_id", Value: j.Manager},
			{Key: "functional_manager_id", Value: j.FManager}, {Key: "gross", Value: j.Gross}, {Key: "band_pos", Value: pos}, {Key: "budget_over", Value: over},
			{Key: "data", Value: d}, {Key: "rejoin_of", Value: strPtr(j.RejoinOf)}, {Key: "raised_by", Value: c.EmployeeID}, {Key: "version", Value: int32(version)}}
		if b.AtJoining || (prev != nil && prev["at_joining"] == true) {
			offer = append(offer, bson.E{Key: "at_joining", Value: b.AtJoining})
		}
		if b.AtJoining {
			if _, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: b.Candidate}}, bson.D{{Key: "$set", Value: bson.D{{Key: "oaj_doj", Value: j.DOJ}}}}); err != nil {
				return err
			}
		} else if err := oajSwitchBack(ctx, tx, c, cand); err != nil { // the usual offer raised instead of the offer at joining
			return err
		}
		if prev != nil { // the revised offer: a new version, sent for approval again, from the start
			offer = append(offer, bson.E{Key: "released_at", Value: nil}, bson.E{Key: "accepted_at", Value: nil}, bson.E{Key: "declined_at", Value: nil},
				bson.E{Key: "query_at", Value: nil}, bson.E{Key: "query_text", Value: nil}, bson.E{Key: "expires_on", Value: nil}, bson.E{Key: "lapsed_at", Value: nil})
			if err := offerVersionClose(ctx, tx, c, prev); err != nil {
				return err
			}
			if _, err := tx.C("offer").Update(ctx, bson.D{{Key: "_id", Value: id}}, bson.D{{Key: "$set", Value: offer}}); err != nil {
				return err
			}
		} else if _, err := tx.C("offer").Insert(ctx, append(bson.D{{Key: "_id", Value: id}}, append(offer, bson.E{Key: "candidate_id", Value: b.Candidate})...)); err != nil {
			return err
		}
		if _, err := tx.C(store.OfferVersionColl).Insert(ctx, bson.D{{Key: "candidate_id", Value: b.Candidate}, {Key: "offer_id", Value: id}, {Key: "version", Value: int32(version)},
			{Key: "type", Value: j.Type}, {Key: "doj", Value: j.DOJ}, {Key: "grade", Value: j.Grade}, {Key: "gross", Value: j.Gross}, {Key: "data", Value: d},
			{Key: "status", Value: "Pending approval"}, {Key: "created_by", Value: c.Name}}); err != nil {
			return err
		}
		if _, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: b.Candidate}}, bson.D{{Key: "$set", Value: bson.D{{Key: "name", Value: j.Name},
			{Key: "offer_version", Value: int32(version)}}}}); err != nil {
			return err
		}
		from := "Selected"
		if prev != nil {
			from = map[bool]string{true: "Question", false: "Not approved"}[prev["query_at"] != nil]
		}
		action := fmt.Sprintf("Offer v%d raised", version)
		if b.AtJoining {
			action += " at joining"
			if prev == nil {
				from = "Offer at joining"
			}
		}
		if err := lifeEvent(ctx, tx, c, lifeEv{Cand: b.Candidate, Action: action, From: from, To: "Pending approval",
			Reason: fmt.Sprintf("%s, gross %s a month, joining %s", j.Type, rupees(j.Gross), longDate(j.DOJ))}); err != nil {
			return err
		}
		if err := offerPfEvent(ctx, tx, c, b.Candidate, prev, d); err != nil { // the PF answer, or its change (pf_offer.go)
			return err
		}
		req, err := s.createRequestTx(ctx, tx, c, "joining", id, offerFacts(j, band, over), map[string]string{"offerId": id, "candidateId": b.Candidate})
		if err != nil {
			return err
		}
		if _, err := tx.C("offer").Update(ctx, bson.D{{Key: "_id", Value: id}}, bson.D{{Key: "$set", Value: bson.D{{Key: "request_id", Value: req["id"]}}}}); err != nil {
			return err
		}
		out = map[string]any{"id": id, "request": req, "version": version}
		what := fmt.Sprintf("Raised offer v%d for %s (%s)", version, j.Name, b.Candidate)
		if b.AtJoining {
			what = fmt.Sprintf("Raised offer v%d at joining for %s (%s)", version, j.Name, b.Candidate)
		}
		return audit(ctx, tx, c, what, "offer", id, nil, map[string]any{"type": j.Type, "gross": j.Gross, "budget": over, "band": pos, "candidate": b.Candidate, "atJoining": b.AtJoining,
			"pf": d["pfWant"]})
	})
	return out, err
}

// offerCandidate is the selected candidate an offer is raised for (stage Offer), within the caller's scope.
func (s *Server) offerCandidate(ctx context.Context, q store.Q, c *Ctx, id string) (store.Row, error) {
	cand, err := s.loadCandidate(ctx, q, c, id, false)
	if err != nil {
		return nil, err
	}
	if cand["stage"] != "Offer" {
		return nil, bad("%s is not at the offer stage", cand["id"])
	}
	return cand, nil
}

// candidateOfferData fills what the offer takes from the shortlist and cannot be changed on the offer form: the
// rejoin and its code choice, and the hiring manager as the reporting manager unless one is chosen.
func candidateOfferData(cand store.Row, d map[string]string) {
	delete(d, "rejoinOf")
	delete(d, "codeMode")
	if rj := str(cand["rejoin_of"]); rj != "" {
		d["rejoinOf"] = rj
		if m, ok := cand["data"].(map[string]any); ok && str(m["codeMode"]) != "" {
			d["codeMode"] = str(m["codeMode"])
		}
	}
	if d["l1"] == "" {
		d["l1"] = str(cand["manager_id"])
	}
}

// ---------- Pipeline ----------

// onboardingOffers lists offers (newest first, at most limit; 0 for all) as the pipeline shows them: the offer with
// the names of its department, designation, location, city and raiser, and its approval and Payroll verification
// requests (null when there is none). An offer whose department, designation, place or raiser is missing is left
// out, as the old inner joins did.
func onboardingOffers(ctx context.Context, q store.Q, filter bson.D, limit int64) ([]store.Row, error) {
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: 1}})
	if limit > 0 {
		opts.SetLimit(limit)
	}
	offers, err := q.C("offer").Find(ctx, filter, opts)
	if err != nil {
		return nil, err
	}
	var deptIDs, desigIDs, locIDs, raisers, reqIDs []string
	for _, o := range offers {
		deptIDs, desigIDs, locIDs = append(deptIDs, str(o["department_id"])), append(desigIDs, str(o["designation_id"])), append(locIDs, str(o["location_id"]))
		raisers, reqIDs = append(raisers, str(o["raised_by"])), append(reqIDs, str(o["request_id"]), str(o["verify_request_id"]))
	}
	depts, err := onboardingByID(ctx, q, "department", deptIDs...)
	if err != nil {
		return nil, err
	}
	desigs, err := onboardingByID(ctx, q, "designation", desigIDs...)
	if err != nil {
		return nil, err
	}
	places, err := onboardingPlaces(ctx, q, locIDs)
	if err != nil {
		return nil, err
	}
	people, err := onboardingByID(ctx, q, "employee", raisers...)
	if err != nil {
		return nil, err
	}
	reqs, err := onboardingByID(ctx, q, "approval_request", reqIDs...)
	if err != nil {
		return nil, err
	}
	out := []store.Row{}
	for _, o := range offers {
		d, g, p, rb := depts[str(o["department_id"])], desigs[str(o["designation_id"])], places[str(o["location_id"])], people[str(o["raised_by"])]
		if d == nil || g == nil || p.loc == nil || rb == nil {
			continue
		}
		r, v := reqs[str(o["request_id"])], reqs[str(o["verify_request_id"])]
		out = append(out, store.Row{"id": o["id"], "candidate_id": o["candidate_id"], "candidate_name": o["candidate_name"], "type": o["type"], "doj": o["doj"],
			"gross": o["gross"], "grade": o["grade"], "employee_id": o["employee_id"], "rejoin_of": o["rejoin_of"], "band_pos": o["band_pos"], "budget_over": o["budget_over"],
			"released_at": o["released_at"], "accepted_at": o["accepted_at"], "declined_at": o["declined_at"], "verified_at": o["verified_at"], "created_at": o["created_at"],
			"data": o["data"], "location_id": o["location_id"], "department_id": o["department_id"], "pan_check": o["pan_check"],
			"offer_no": o["offer_no"], "version": o["version"], "expires_on": o["expires_on"], "query_at": o["query_at"], "query_text": o["query_text"], "lapsed_at": o["lapsed_at"],
			"letter_at": o["letter_at"], "letter_released_at": o["letter_released_at"], // hirefix.go
			"at_joining": o["at_joining"], // offer_at_joining.go
			"department": d["name"], "designation": g["name"], "location": p.loc["name"], "city": p.city["name"], "city_id": p.city["id"], "raised_by": rb["name"],
			"request_id": onboardingCol(r, "id"), "request_status": onboardingCol(r, "status"), "chain": onboardingCol(r, "chain"), "step": onboardingCol(r, "step"),
			"verify_id": onboardingCol(v, "id"), "verify_status": onboardingCol(v, "status"), "verify_chain": onboardingCol(v, "chain"), "verify_step": onboardingCol(v, "step")})
	}
	return out, nil
}

// onboardingPlaceRow is a location with its city (location JOIN cluster JOIN city).
type onboardingPlaceRow struct{ loc, city store.Row }

// onboardingPlaces loads locations with their cities, keyed by location id; a location whose cluster or city is
// missing is left out.
func onboardingPlaces(ctx context.Context, q store.Q, locIDs []string) (map[string]onboardingPlaceRow, error) {
	locs, err := onboardingByID(ctx, q, "location", locIDs...)
	if err != nil {
		return nil, err
	}
	var clusterIDs []string
	for _, l := range locs {
		clusterIDs = append(clusterIDs, str(l["cluster_id"]))
	}
	clusters, err := onboardingByID(ctx, q, "cluster", clusterIDs...)
	if err != nil {
		return nil, err
	}
	var cityIDs []string
	for _, k := range clusters {
		cityIDs = append(cityIDs, str(k["city_id"]))
	}
	cities, err := onboardingByID(ctx, q, "city", cityIDs...)
	if err != nil {
		return nil, err
	}
	out := map[string]onboardingPlaceRow{}
	for id, l := range locs {
		if k := clusters[str(l["cluster_id"])]; k != nil {
			if c := cities[str(k["city_id"])]; c != nil {
				out[id] = onboardingPlaceRow{loc: l, city: c}
			}
		}
	}
	return out, nil
}

// offerStatus is derived from the approval, release, acceptance and verification.
func offerStatus(o store.Row) string {
	switch o["request_status"] {
	case "Pending":
		return "Pending approval"
	case "Rejected":
		return "Rejected"
	case "Cancelled":
		return "Cancelled"
	}
	switch {
	case o["declined_at"] != nil:
		return "Declined"
	case o["lapsed_at"] != nil: // no answer by the expiry date (hiring.go)
		return "Lapsed"
	case o["query_at"] != nil && o["accepted_at"] == nil: // a question about the breakup: HR revises the offer
		return "Question"
	case o["verified_at"] != nil:
		return "Verified"
	case o["verify_status"] == "Pending":
		return "With Payroll"
	case o["verify_status"] == "Rejected":
		return "Returned by Payroll"
	case o["accepted_at"] != nil:
		return "Accepted"
	case o["released_at"] != nil:
		return "Released"
	}
	return "Approved"
}

func rowData(o store.Row) map[string]string {
	out := map[string]string{}
	if m, ok := o["data"].(map[string]any); ok {
		for k, v := range m {
			out[k] = fmt.Sprint(v)
		}
	}
	return out
}

// progress counts the mandatory items of a stage that are complete.
func progress(fs []onbField, ds []onbDoc, o store.Row, d map[string]string, docs map[string]store.Row, stage string) (int, int) {
	done, total := 0, 0
	typ, dept := o["type"].(string), o["department"].(string)
	for _, f := range fs {
		st := f.Join
		if stage == "offer" {
			st = f.Offer
		}
		if st == 2 && f.Kind != "flow" && f.Kind != "policy" && f.applies(typ, d) {
			total++
			if d[f.Key] != "" {
				done++
			}
		}
	}
	for _, doc := range ds {
		st := doc.Join
		if stage == "offer" {
			st = doc.Offer
		}
		if st == 2 && doc.applies(typ, dept) {
			total++
			if docs[doc.Key] != nil {
				done++
			}
		}
	}
	return done, total
}

func (s *Server) canSeeOffers(c *Ctx) error {
	if c.Level("onb.pipeline") < 1 && c.Level("onb.raise") < 1 {
		return forbidden("")
	}
	return nil
}

func (s *Server) offerList(r *http.Request, c *Ctx) (any, error) {
	if err := s.canSeeOffers(c); err != nil && c.Level("comp.structure") < 1 {
		return nil, err
	}
	ctx := r.Context()
	where, err := onboardingScope(ctx, s.st.DB, c.Scope)
	if err != nil {
		return nil, err
	}
	if r.URL.Query().Get("legacy") == "1" { // the offers raised before stages, still finishing (Onboarding › Pipeline)
		where = append(where, bson.E{Key: "candidate_id", Value: nil})
	}
	rows, err := onboardingOffers(ctx, s.st.DB, where, 500)
	if err != nil {
		return nil, err
	}
	fs, ds, err := loadSetup(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	want := r.URL.Query().Get("status")
	out := []store.Row{}
	for _, o := range rows {
		o["status"] = offerStatus(o)
		if want != "" && o["status"] != want {
			continue
		}
		d := rowData(o)
		kind, owner := offerDocOwner(o)
		docs, _ := docsOf(ctx, s.st.DB, kind, owner)
		od, ot := progress(nil, ds, o, d, docs, "offer")
		jd, jt := progress(fs, ds, o, d, docs, "join")
		o["offerDocs"] = map[string]int{"done": od, "total": ot}
		o["joining"] = map[string]int{"done": jd, "total": jt}
		delete(o, "data")
		out = append(out, o)
	}
	return out, nil
}

func (s *Server) loadOffer(ctx context.Context, q store.Q, c *Ctx, id string) (store.Row, error) {
	rows, err := onboardingOffers(ctx, q, bson.D{{Key: "_id", Value: id}}, 1)
	if err != nil || len(rows) == 0 {
		return nil, orNotFound(err)
	}
	o := rows[0]
	if err := c.MustInScope(ctx, q, id); err != nil {
		return nil, err
	}
	o["status"] = offerStatus(o)
	return o, nil
}

func (s *Server) offerGet(r *http.Request, c *Ctx) (any, error) {
	if c.Level("onb.pipeline") < 1 && c.Level("onb.raise") < 1 && c.Level("appr.queue") < 1 && c.Level("comp.structure") < 1 {
		return nil, forbidden("")
	}
	ctx := r.Context()
	o, err := s.loadOffer(ctx, s.st.DB, c, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	fs, ds, err := loadSetup(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	d := rowData(o)
	kind, owner := offerDocOwner(o)
	docs, _ := docsOf(ctx, s.st.DB, kind, owner)
	o["docOwner"] = map[string]string{"kind": kind, "id": owner}
	typ, dept := o["type"].(string), o["department"].(string)
	joinFields, joinDocs := []onbField{}, []onbDoc{}
	for _, f := range fs {
		if f.Join > 0 && f.applies(typ, d) {
			joinFields = append(joinFields, f)
		}
	}
	joinFields = append(joinFields, joinRoleSkillFields(ctx, s.st.DB, typ, d)...) // an offer without Role or Skill (hirefix.go)
	for _, doc := range ds {
		if (doc.Join > 0 || doc.Offer > 0) && doc.applies(typ, dept) {
			joinDocs = append(joinDocs, doc)
		}
	}
	jd, jt := progress(fs, ds, o, d, docs, "join")
	o["joining"] = map[string]int{"done": jd, "total": jt}
	o["joinFields"], o["docList"], o["docs"] = joinFields, joinDocs, docs
	j := offerJob{Type: typ, Gross: o["gross"].(float64), LocID: o["location_id"].(string), DOJ: o["doj"].(string)}
	res := offerPay(ctx, s.st.DB, j, d)
	o["breakup"], o["ctcYear"] = res, calc.Round2(res.CTC*12)
	o["accommodation"] = accomLine(ctx, s.st.DB, o) // the accommodation tag, no pay effect (hst_auto.go)
	o["letter"], _ = s.offerLetter(ctx, o, res)
	letterView(ctx, s.st.DB, o)     // a letter held until joining is not generated (hirefix.go)
	o["panCheck"] = offerPanView(o) // the PAN check on the joining details (b1_pan.go)
	delete(o, "pan_check")
	if offerLetterOnly(c) { // Compensation, Offer letter preview: no personal details or documents
		return offerPreviewOnly(o), nil
	}
	o["asked"] = askedDocs(ctx, s.st.DB, owner, "")
	return o, nil
}

// rupees formats an amount the Indian way: ₹1,23,456.
func rupees(v float64) string {
	n := int64(math.Round(v))
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	if len(s) > 3 {
		head, tail := s[:len(s)-3], s[len(s)-3:]
		parts := []string{}
		for len(head) > 2 {
			parts = append([]string{head[len(head)-2:]}, parts...)
			head = head[:len(head)-2]
		}
		if head != "" {
			parts = append([]string{head}, parts...)
		}
		s = strings.Join(parts, ",") + "," + tail
	}
	if neg {
		s = "-" + s
	}
	return "₹" + s
}

func longDate(d string) string {
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		return d
	}
	return t.Format("02 Jan 2006")
}

var mergeField = regexp.MustCompile(`\{\{(\w+)\}\}`)

// offerLetter fills the company's offer template (spec 8.5).
func (s *Server) offerLetter(ctx context.Context, o store.Row, res calc.Result) (string, error) {
	cs, err := onboardingOne(ctx, s.st.DB, "company_setting", 1)
	if err != nil {
		return "", err
	}
	if cs == nil {
		return "", store.ErrNoRows
	}
	tpl, company := str(cs["offer_template"]), str(cs["name"])
	m := map[string]string{"name": fmt.Sprint(o["candidate_name"]), "designation": fmt.Sprint(o["designation"]), "department": fmt.Sprint(o["department"]),
		"company": company, "location": fmt.Sprint(o["location"]), "city": fmt.Sprint(o["city"]), "grade": fmt.Sprint(o["grade"]), "doj": longDate(fmt.Sprint(o["doj"])),
		"gross": rupees(res.Gross), "fixed": rupees(res.Gross), "variable": rupees(res.Variable), "ctcYear": rupees(res.CTC * 12), "net": rupees(res.Net),
		"probation": offerProbation(ctx, s.st.DB, o),
		// The breakup from the structure (pay components, paycomp.go): {{basic}}, {{hra}}, {{total}} (fixed + variable a
		// month), {{ctcMonth}} and {{breakup}}, one line per component.
		"basic": rupees(res.Basic), "hra": rupees(res.Hra), "total": rupees(res.Gross + res.Variable), "ctcMonth": rupees(res.CTC), "breakup": offerBreakup(res),
		// {{accommodation}}: "Company accommodation: Yes, in <cluster> …", or empty (hst_auto.go).
		"accommodation": accomLine(ctx, s.st.DB, o)}
	return mergeField.ReplaceAllStringFunc(tpl, func(x string) string {
		if v, ok := m[x[2:len(x)-2]]; ok {
			return v
		}
		return x
	}), nil
}

// offerBreakup is the breakup as lines of text, a month: the components of a designed structure, else Basic + DA, HRA
// and Special allowance, then the variable part.
func offerBreakup(res calc.Result) string {
	var l []string
	if res.Fee > 0 { // a retainer: the retainer value only (calc retainerOnly)
		l = append(l, "Retainer fee: "+rupees(res.Fee)+" a month (TDS "+tdsNote(res.TdsRate)+")")
		for _, c := range res.Comps {
			if c.Side == calc.SideDed {
				l = append(l, fmt.Sprintf("%s (deduction): %s a month", c.Name, rupees(calc.Round2(c.Amount))))
			}
		}
		return strings.Join(l, "\n")
	}
	if len(res.Comps) > 0 {
		for _, c := range res.Comps {
			switch {
			case c.Kind == calc.CompVariable:
				l = append(l, fmt.Sprintf("%s (variable, %s): %s a month", c.Name, cycleWord(c.Cycle), rupees(calc.Round2(c.Target))))
			case c.Extra:
				l = append(l, fmt.Sprintf("%s (outside the gross): %s a month", c.Name, rupees(calc.Round2(c.Amount))))
			case c.Side == calc.SideDed:
				l = append(l, fmt.Sprintf("%s (deduction): %s a month", c.Name, rupees(calc.Round2(c.Amount))))
			default:
				l = append(l, fmt.Sprintf("%s: %s a month", c.Name, rupees(calc.Round2(c.Amount))))
			}
		}
	} else {
		l = append(l, "Basic + DA: "+rupees(res.Basic)+" a month")
		if !res.PfExempt || res.Hra != 0 || res.Sa != 0 { // no PF by choice: the whole gross is Basic (pf_offer.go)
			l = append(l, "HRA: "+rupees(res.Hra)+" a month", "Special allowance: "+rupees(res.Sa)+" a month")
		}
		if res.Reimb > 0 {
			l = append(l, "Reimbursement (outside the gross): "+rupees(res.Reimb)+" a month")
		}
		if res.Variable > 0 {
			l = append(l, "Variable pay: "+rupees(res.Variable)+" a month")
		}
	}
	if res.PfExempt {
		l = append(l, "Provident Fund: not deducted (the employee chose no PF)")
	}
	return strings.Join(l, "\n")
}

// ---------- Release, accept, decline ----------

// offerStep releases an offer, or marks it accepted or declined: onboarding actions, for the HR roles that raise
// offers or run the pipeline (spec 8.1, 8.5). Publishing pay changes (comp.approve, held by Payroll and the CHRO)
// does not reach them; Payroll only verifies joining (HIRE-048).
func (s *Server) offerStep(r *http.Request, c *Ctx) (any, error) {
	if c.Level("onb.raise") < 2 && c.Level("onb.pipeline") < 2 {
		return nil, forbidden("")
	}
	var b struct {
		Reason  string `json:"reason"`
		Expires string `json:"expires"` // release, expiry: the date the offer lapses without an answer (hiring.go)
		// release: "Company accommodation: Yes / No" and the cluster, asked again at release (hst_auto.go); "" keeps the offer's.
		Accom        string `json:"accom"`
		AccomCluster string `json:"accomCluster"`
	}
	_ = decode(r, &b)
	b.Reason = strings.TrimSpace(b.Reason)
	ctx, step := r.Context(), r.PathValue("step")
	var msg string
	err := s.st.Tx(ctx, func(tx store.Q) error {
		o, err := s.loadOffer(ctx, tx, c, r.PathValue("id"))
		if err != nil {
			return err
		}
		st, now := o["status"], tx.Now()
		thisOffer := bson.D{{Key: "_id", Value: o["id"]}}
		cand := str(o["candidate_id"])
		expires := b.Expires
		switch step {
		case "release":
			if st != "Approved" {
				return bad("The offer can be released once it is approved, and only once")
			}
			if o["at_joining"] == true { // offer_at_joining.go
				return bad("An offer at joining is not released: Mark joined records the acceptance and gives the letter")
			}
			if err := offerPfRelease(ctx, tx, o); err != nil { // no PF on a gross up to the threshold (pf_offer.go)
				return err
			}
			set := bson.D{{Key: "released_at", Value: now}}
			if cand != "" {
				if expires == "" {
					expires = dayAdd(store.DateOf(now), loadHireSettings(ctx, tx).OfferDays)
				}
				if !isDate(expires) || expires < store.DateOf(now) {
					return bad("The answer is due on today or a later date")
				}
				set = append(set, bson.E{Key: "expires_on", Value: expires})
			}
			if b.Accom != "" { // the accommodation answer given at release
				d := rowData(o)
				before := map[string]string{"accom": d["accom"], "accomCluster": d["accomCluster"]}
				d["accom"], d["accomCluster"] = b.Accom, b.AccomCluster
				errs := []string{}
				if err := accomProblems(ctx, tx, d, &errs); err != nil {
					return err
				}
				if len(errs) > 0 {
					return bad("%s", strings.Join(errs, "; "))
				}
				if _, err := tx.C("offer").Update(ctx, thisOffer, bson.D{{Key: "$set", Value: bson.D{{Key: "data.accom", Value: d["accom"]},
					{Key: "data.accomCluster", Value: nullIfEmpty(d["accomCluster"])}}}}); err != nil {
					return err
				}
				if err := audit(ctx, tx, c, fmt.Sprintf("Company accommodation on offer %s: %s", o["id"], d["accom"]), "offer", str(o["id"]), before,
					map[string]string{"accom": d["accom"], "accomCluster": d["accomCluster"]}); err != nil {
					return err
				}
			}
			held := cand != "" && letterAtJoining(loadHireSettings(ctx, tx), str(o["department"]), str(o["type"])) // hirefix.go
			if held {
				set = append(set, bson.E{Key: "letter_at", Value: "joining"})
			}
			_, err = tx.C("offer").Update(ctx, thisOffer, bson.D{{Key: "$set", Value: set}})
			msg = "Offer released to " + o["candidate_name"].(string)
			if held {
				msg += ". The offer letter is released when they are marked joined"
			}
		case "query":
			if cand == "" || st != "Released" {
				return bad("A question is recorded on a sent offer that has not been answered")
			}
			if b.Reason == "" || len(b.Reason) > 1000 {
				return bad("Write what the candidate asked about the breakup (up to 1,000 characters)")
			}
			_, err = tx.C("offer").Update(ctx, thisOffer, bson.D{{Key: "$set", Value: bson.D{{Key: "query_at", Value: now}, {Key: "query_text", Value: b.Reason}}}})
			msg = "Question recorded. Revise the offer to send a new version"
		case "expiry":
			if cand == "" || st != "Released" {
				return bad("The expiry date is moved on a sent offer that has not been answered")
			}
			if !isDate(expires) || expires < store.DateOf(now) {
				return bad("Enter the new date: today or later")
			}
			if b.Reason == "" {
				return bad("Give the reason")
			}
			_, err = tx.C("offer").Update(ctx, thisOffer, bson.D{{Key: "$set", Value: bson.D{{Key: "expires_on", Value: expires}}}})
			msg = "The offer now lapses after " + longDate(expires)
		case "accept":
			if st != "Released" && st != "Question" {
				return bad("Release the offer first")
			}
			_, err = tx.C("offer").Update(ctx, thisOffer, bson.D{{Key: "$set", Value: bson.D{{Key: "accepted_at", Value: now}}}})
			msg = o["candidate_name"].(string) + " accepted the offer"
			if cand != "" { // the Offer ID is issued now (hiring.go)
				oi := str(o["offer_no"])
				if x, _ := onboardingOne(ctx, tx, "candidate", cand); oi == "" && x != nil {
					oi = stageID("OI", toInt(x["no"]))
				}
				msg += ". Offer ID " + oi
			}
		case "decline":
			if st != "Released" && st != "Approved" && st != "Accepted" && st != "Question" {
				return bad("Only an approved offer that has not been verified can be marked declined")
			}
			if strings.TrimSpace(b.Reason) == "" {
				return bad("Say why the offer was declined")
			}
			if cand := str(o["candidate_id"]); cand != "" {
				x, err := tx.C("candidate").Lock(ctx, bson.D{{Key: "_id", Value: cand}})
				if err != nil {
					return err
				}
				switch onboardingCol(x, "stage") {
				case "Closed":
					return bad("%s is closed", cand)
				case "Joined":
					return bad("%s has already joined. Use separation instead", o["candidate_name"])
				}
				if _, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: cand}}, bson.D{{Key: "$set", Value: bson.D{{Key: "stage", Value: "Closed"},
					{Key: "closed_kind", Value: "Declined"}, {Key: "closed_reason", Value: strings.TrimSpace(b.Reason)}, {Key: "closed_at", Value: now}, {Key: "closed_by", Value: c.Name}}}}); err != nil {
					return err
				}
			}
			if _, err = tx.C("offer").Update(ctx, thisOffer, bson.D{{Key: "$set", Value: bson.D{{Key: "declined_at", Value: now}, {Key: "data.declineReason", Value: b.Reason}}}}); err != nil {
				return err
			}
			if err := stopJoinerLink(ctx, tx, str(o["id"]), c.Name+" ("+c.EmployeeID+")", "The offer was declined"); err != nil {
				return err
			}
			// The pre-boarding record stays for history, inactive.
			if emp := str(o["employee_id"]); emp != "" {
				_, err = tx.C("employee").Update(ctx, bson.D{{Key: "_id", Value: emp}, {Key: "status", Value: "Pre-boarding"}},
					bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "Inactive"}, {Key: "exit_reason", Value: "Offer declined"}}}})
			}
			msg = o["candidate_name"].(string) + " declined the offer"
		default:
			return notFound("")
		}
		if err != nil {
			return err
		}
		if err := offerAnswer(ctx, tx, c, o, step, b.Reason, expires); err != nil { // versions, Offer ID, timeline (hiring.go)
			return err
		}
		return audit(ctx, tx, c, msg, "offer", o["id"].(string), nil, b)
	})
	return map[string]string{"result": msg}, err
}

// ---------- Joining details and Payroll verification ----------

func (s *Server) offerJoining(r *http.Request, c *Ctx) (any, error) {
	if c.Level("onb.raise") < 2 && c.Level("onb.pipeline") < 2 {
		return nil, forbidden("")
	}
	var b struct {
		Data map[string]any `json:"data"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	in := cleanData(b.Data)
	errs := []string{}
	checkFormat(in, &errs)
	uniformFormErrors(ctx, s.st.DB, in, &errs)
	if len(errs) > 0 {
		return nil, badD(errs[0], errs)
	}
	var out map[string]any
	var offerID string
	err := s.st.Tx(ctx, func(tx store.Q) error {
		out = nil // the transaction may run again
		o, err := s.loadOffer(ctx, tx, c, r.PathValue("id"))
		if err != nil {
			return err
		}
		if st := o["status"]; st != "Accepted" && st != "Returned by Payroll" && st != "Released" {
			return bad("Joining details are collected after the offer is released, until Payroll verifies them")
		}
		if cand := str(o["candidate_id"]); cand != "" {
			if x, _ := onboardingOne(ctx, tx, "candidate", cand); onboardingCol(x, "stage") == "Closed" {
				return bad("%s is closed", cand)
			}
		}
		fs, _, err := loadSetup(ctx, tx)
		if err != nil {
			return err
		}
		d := rowData(o)
		for k, v := range in {
			if k == "uanAvail" || k == "marital" { // the answers other fields depend on (onbField.asked)
				d[k] = v
			}
		}
		keep := map[string]any{}
		for _, f := range fs {
			if f.Join > 0 && f.applies(o["type"].(string), d) {
				if v, ok := in[f.Key]; ok {
					if f.Key == "pan" || f.Key == "ifsc" {
						v = strings.ToUpper(v)
					}
					keep[f.Key] = v
				}
			}
		}
		// Shift, attendance mode, qualification and managers must be real (HIRE-064), as Payroll's corrections check.
		mErrs := []string{}
		for _, f := range fs {
			v, ok := keep[f.Key]
			if !ok || fmt.Sprint(v) == "" {
				continue
			}
			mErrs = append(mErrs, joinMasterErrors(ctx, tx, f, fmt.Sprint(v))...)
		}
		rs, rsErrs := joinRoleSkill(ctx, tx, o, in) // Role and Skill of an offer that has none yet (hirefix.go)
		mErrs = append(mErrs, rsErrs...)
		if len(mErrs) > 0 {
			return badD(mErrs[0], mErrs)
		}
		if v, ok := keep["pfChoice"]; ok { // the PF answer given in the joining details (pf_offer.go)
			if err := joinPfEvent(ctx, tx, c, o, fmt.Sprint(v), "the joining details"); err != nil {
				return err
			}
		}
		// The details sent are merged into the offer's form data; the other keys stay.
		merge := bson.D{}
		for k, v := range keep {
			merge = append(merge, bson.E{Key: "data." + k, Value: v})
		}
		merge = append(merge, rs...)
		if len(merge) > 0 {
			if _, err := tx.C("offer").Update(ctx, bson.D{{Key: "_id", Value: o["id"]}}, bson.D{{Key: "$set", Value: merge}}); err != nil {
				return err
			}
		}
		if len(rs) > 0 {
			for _, x := range rs {
				keep[strings.TrimPrefix(x.Key, "data.")] = x.Value
			}
			if o2, _ := onboardingOne(ctx, tx, "offer", str(o["id"])); o2 != nil {
				if err := joinRoleSkillEmployee(ctx, tx, o2); err != nil {
					return err
				}
			}
		}
		// The audit log names the fields, with the Aadhaar and account number masked, as the joiner's own save does
		// (HIRE-219).
		logged := map[string]any{}
		for k, v := range keep {
			logged[k] = v
			if verifyMaskKeys[k] {
				logged[k] = verifyShow(k, fmt.Sprint(v), nil)
			}
		}
		offerID = o["id"].(string)
		if err := audit(ctx, tx, c, "Joining details updated for "+o["candidate_name"].(string), "offer", o["id"].(string), nil, logged); err != nil {
			return err
		}
		if cand := str(o["candidate_id"]); cand != "" { // the last mandatory detail may complete the joining
			code, err := s.maybeEmployeeID(ctx, tx, c, cand)
			if code != "" {
				out = map[string]any{"employeeId": code}
			}
			return err
		}
		return nil
	})
	if err == nil && offerID != "" {
		s.panCheckOffer(ctx, c, offerID) // a new PAN is checked when verification is on (b1_pan.go)
	}
	return out, err
}

func (s *Server) offerSend(r *http.Request, c *Ctx) (any, error) {
	if c.Level("onb.raise") < 2 && c.Level("onb.pipeline") < 2 {
		return nil, forbidden("")
	}
	ctx := r.Context()
	var out map[string]any
	err := s.st.Tx(ctx, func(tx store.Q) error {
		o, err := s.loadOffer(ctx, tx, c, r.PathValue("id"))
		if err != nil {
			return err
		}
		if st := o["status"]; st != "Accepted" && st != "Returned by Payroll" {
			return bad("The candidate must accept the offer first")
		}
		fs, ds, err := loadSetup(ctx, tx)
		if err != nil {
			return err
		}
		d := rowData(o)
		kind, owner := offerDocOwner(o)
		docs, _ := docsOf(ctx, tx, kind, owner)
		if done, total := progress(fs, ds, o, d, docs, "join"); done < total {
			return bad("Complete the mandatory joining details first (%d of %d done)", done, total)
		}
		if owner != o["id"] && str(o["employee_id"]) == "" {
			if x, _ := onboardingOne(ctx, tx, "candidate", owner); onboardingCol(x, "stage") != "Joined" {
				return bad("Mark the candidate joined first")
			}
			// Joined with everything in: the Employee ID is due now (the setting changed since, for example).
			code, err := createEmployeeID(ctx, tx, c, owner)
			if err != nil {
				return err
			}
			o["employee_id"] = code
		}
		errs := []string{}
		checkFormat(d, &errs)
		if m := jobMasterMissing(d); m != "" { // an offer made before Role and Skill were asked (hirefix.go)
			errs = append(errs, m+": fill it in the joining details")
		}
		if d["uanAvail"] == "Yes" && d["uan"] == "" {
			errs = append(errs, "UAN is required when an existing UAN is available")
		}
		errs = append(errs, joinAskedErrors(fs, str(o["type"]), d)...)
		if len(errs) > 0 {
			return badD(errs[0], errs)
		}
		// One medical insurance plan for every new joiner (owner, 19 Nov 2026): nothing to choose on the form.
		if d["medical"] != store.MedicalPlan {
			d["medical"] = store.MedicalPlan
			if _, err := tx.C("offer").Update(ctx, bson.D{{Key: "_id", Value: o["id"]}}, bson.D{{Key: "$set", Value: bson.D{{Key: "data.medical", Value: store.MedicalPlan}}}}); err != nil {
				return err
			}
		}
		// A Joining-ID record (hiring.go) takes the joining details now, as the Employee ID made at documents did:
		// Payroll verifies against the record.
		if owner != o["id"] {
			if x, _ := onboardingOne(ctx, tx, "candidate", owner); x != nil && x["ji_at"] != nil && x["approved_at"] == nil {
				emp := str(o["employee_id"])
				e, _ := onboardingOne(ctx, tx, "employee", emp)
				if err := applyPersonal(ctx, tx, emp, str(onboardingCol(e, "doj")), d, "Joining details sent to Payroll"); err != nil {
					return err
				}
				if err := applyOfferExtras(ctx, tx, emp, o, d); err != nil {
					return err
				}
			}
		}
		// Payroll verifies against the employee record created at offer approval.
		req, err := s.createRequestTx(ctx, tx, c, "verify", o["employee_id"].(string), approval.Facts{}, map[string]string{"offerId": o["id"].(string)})
		if err != nil {
			return err
		}
		if _, err := tx.C("offer").Update(ctx, bson.D{{Key: "_id", Value: o["id"]}}, bson.D{{Key: "$set", Value: bson.D{{Key: "verify_request_id", Value: req["id"]}}}}); err != nil {
			return err
		}
		out = req
		return audit(ctx, tx, c, "Joining details of "+o["candidate_name"].(string)+" sent to Payroll", "offer", o["id"].(string), nil, nil)
	})
	return out, err
}

// ---------- Finalizers: offer approved → pre-boarding employee; verified → active ----------

func strPtr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// applyPersonal writes the personal, ID and bank details collected so far onto the employee.
func applyPersonal(ctx context.Context, tx store.Q, empID, doj string, d map[string]string, by string) error {
	// A detail left blank keeps what the record already has (or stays empty on a new record).
	if err := onboardingKeepUpsert(ctx, tx, "employee_personal", empID, map[string]string{"dob": d["dob"], "gender": d["gender"],
		"mobile": strings.ReplaceAll(d["phone"], " ", ""), "email": d["email"], "address": d["addr"], "marital": d["marital"],
		"ec_name": d["ecName"], "ec_relation": d["ecRel"], "ec_mobile": d["ecPhone"],
		"father_spouse": d["fatherSpouse"], "child1": d["child1"], "child2": d["child2"], "spouse": d["spouse"]}); err != nil {
		return err
	}
	if err := onboardingKeepUpsert(ctx, tx, "employee_gov_id", empID, map[string]string{"aadhaar": strings.ReplaceAll(d["aadhaar"], " ", ""),
		"pan": strings.ToUpper(d["pan"]), "uan": d["uan"], "esic": d["esiIp"]}); err != nil {
		return err
	}
	if err := applyJoiningSizes(ctx, tx, empID, d, by); err != nil {
		return err
	}
	if d["acct"] != "" && d["ifsc"] != "" && d["bankName"] != "" {
		exists, err := tx.C("employee_bank").Exists(ctx, bson.D{{Key: "employee_id", Value: empID}, {Key: "account_no", Value: d["acct"]}, {Key: "ifsc", Value: strings.ToUpper(d["ifsc"])}})
		if err != nil {
			return err
		}
		if !exists {
			if _, err := tx.C("employee_bank").Insert(ctx, bson.D{{Key: "employee_id", Value: empID}, {Key: "bank_name", Value: d["bankName"]}, {Key: "account_no", Value: d["acct"]},
				{Key: "ifsc", Value: strings.ToUpper(d["ifsc"])}, {Key: "from_month", Value: doj[:7]}, {Key: "created_by", Value: by}}); err != nil {
				return err
			}
		}
	}
	return nil
}

// onboardingKeepUpsert creates or updates an employee's one row in table (employee_personal, employee_gov_id) with the
// values given; an empty value is not written, so the row keeps what it has (or stays empty when it is new).
func onboardingKeepUpsert(ctx context.Context, tx store.Q, table, empID string, vals map[string]string) error {
	set := bson.M{}
	for col, v := range vals {
		if v != "" {
			set[col] = v
		}
	}
	_, err := tx.C(table).Upsert(ctx, bson.D{{Key: "employee_id", Value: empID}}, set, nil)
	return err
}

// finalizeOffer: the offer is approved. An offer raised before stages (OFR-1001) makes the pre-boarding employee
// now, as it always did; a candidate's offer (OI-0001) makes nothing: the Employee ID comes after joining.
func finalizeOffer(ctx context.Context, tx store.Q, r *Request) (string, error) {
	o, err := onboardingLockOffer(ctx, tx, bson.D{{Key: "_id", Value: r.EmployeeID}})
	if err != nil || o == nil {
		return "", orNotFound(err)
	}
	if cand := str(o["candidate_id"]); cand != "" {
		v := max(1, toInt(o["version"]))
		if err := lifeEvent(ctx, tx, nil, lifeEv{Cand: cand, Action: fmt.Sprintf("Offer v%d approved", v), From: "Pending approval", To: "Approved", Reason: "Approval " + r.ID}); err != nil {
			return "", err
		}
		if o["at_joining"] == true { // offer_at_joining.go
			return fmt.Sprintf("Offer at joining v%d for %s (%s) approved. Mark them joined from Onboarding: joining is the acceptance and the Offer ID is issued then.", v, o["candidate_name"], cand), nil
		}
		return fmt.Sprintf("Offer v%d for %s (%s) approved. Release it from Onboarding. The Offer ID is issued when the candidate accepts.", v, o["candidate_name"], cand), nil
	}
	code, same, err := employeeFromOffer(ctx, tx, o, o["doj"].(string), "Offer "+o["id"].(string))
	if err != nil {
		return "", err
	}
	if same {
		return fmt.Sprintf("%s rejoins on the same code %s as pre-boarding. Release the offer from Onboarding.", o["candidate_name"], code), nil
	}
	return fmt.Sprintf("%s created as pre-boarding. Release the offer from Onboarding, then collect joining details and send them to Payroll.", code), nil
}

// onboardingLockOffer locks an offer for the rest of the transaction (the whole offer) and adds the names
// employeeFromOffer reads: gross_f, dept_id, desig_id and loc_id. nil when there is no such offer.
func onboardingLockOffer(ctx context.Context, tx store.Q, filter bson.D) (store.Row, error) {
	o, err := tx.C("offer").Lock(ctx, filter)
	if err != nil || o == nil {
		return nil, err
	}
	o["gross_f"], o["dept_id"], o["desig_id"], o["loc_id"] = o["gross"], o["department_id"], o["designation_id"], o["location_id"]
	return o, nil
}

// employeeFromOffer creates the employee record from an offer as Pre-boarding, or reopens a rejoiner's old record
// when the code is kept: job, pay, personal, ID and bank details, and the documents' employee_id. doj is the date on
// the record (the offer's for offers before stages, the actual joining date for a candidate). same is true when a
// rejoiner keeps the old code.
func employeeFromOffer(ctx context.Context, tx store.Q, o store.Row, doj, by string) (code string, same bool, err error) {
	return employeeFromOfferWith(ctx, tx, o, doj, by, empOpts{})
}

// empOpts change what employeeFromOffer makes (hiring.go): Code is the new record's id instead of the next code of
// the series (a Joining ID JI-0042, or a contract series number), Status and Stage what it starts as.
type empOpts struct{ Code, Status, Stage string }

func employeeFromOfferWith(ctx context.Context, tx store.Q, o store.Row, doj, by string, opt empOpts) (code string, same bool, err error) {
	if opt.Status == "" {
		opt.Status = "Pre-boarding"
	}
	d := rowData(o)
	typ := o["type"].(string)
	shift, mode := d["shift"], d["attMode"]
	if shift == "" {
		shift = "G"
	}
	if mode == "" {
		mode = store.SysName(ctx, tx, "mode.default")
	}
	rejoin, _ := o["rejoin_of"].(string)
	settings, err := onboardingOne(ctx, tx, "employee_code_settings", 1)
	if err != nil {
		return "", false, err
	}
	mode2 := str(onboardingCol(settings, "rejoin"))
	if cm := d["codeMode"]; rejoin != "" && (cm == "same" || cm == "new") {
		mode2 = cm // chosen in the rejoin dialog
	}
	// The job on the record, as the offer has it.
	job := bson.D{{Key: "name", Value: o["candidate_name"]}, {Key: "type", Value: typ}, {Key: "doj", Value: doj}, {Key: "department_id", Value: o["dept_id"]},
		{Key: "designation_id", Value: o["desig_id"]}, {Key: "grade", Value: o["grade"]}, {Key: "location_id", Value: o["loc_id"]}, {Key: "shift_code", Value: shift},
		{Key: "manager_id", Value: o["manager_id"]}, {Key: "functional_manager_id", Value: o["functional_manager_id"]}, {Key: "attendance_mode", Value: mode},
		{Key: "status", Value: opt.Status}, {Key: "verified", Value: false}}
	if opt.Stage != "" {
		job = append(job, bson.E{Key: "stage", Value: opt.Stage}, bson.E{Key: "stage_since", Value: doj})
	}
	job = append(job, jobMasterJob(ctx, tx, d)...) // Role and Skill (b1_masters.go)
	if rejoin != "" && mode2 != "new" {
		// Keep the old code: record the earlier service period, then reopen the record.
		code = rejoin
		old, err := onboardingOne(ctx, tx, "employee", code)
		if err != nil {
			return "", false, err
		}
		if old != nil {
			if _, err := tx.C("employee_service").Insert(ctx, bson.D{{Key: "employee_id", Value: old["id"]}, {Key: "code", Value: old["id"]}, {Key: "doj", Value: old["doj"]},
				{Key: "lwd", Value: old["lwd"]}, {Key: "exit_reason", Value: old["exit_reason"]}}); err != nil {
				return "", false, err
			}
		}
		reopen := append(job, bson.E{Key: "exit_status", Value: nil}, bson.E{Key: "lwd", Value: nil}, bson.E{Key: "resign_at", Value: nil}, bson.E{Key: "exit_reason", Value: nil})
		if _, err := tx.C("employee").Update(ctx, bson.D{{Key: "_id", Value: code}}, bson.D{{Key: "$set", Value: reopen}}); err != nil {
			return "", false, err
		}
		if _, err := tx.C("employee_change").Insert(ctx, bson.D{{Key: "employee_id", Value: code}, {Key: "field", Value: "Status"}, {Key: "old_value", Value: "Inactive"},
			{Key: "new_value", Value: "Rejoined as pre-boarding"}, {Key: "reason", Value: "Rejoin"}, {Key: "effective_from", Value: doj}, {Key: "source", Value: "Onboarding"},
			{Key: "changed_by", Value: by}}); err != nil {
			return "", false, err
		}
	} else {
		// The next code of the series; the code is the record's id for good (a Joining ID until the Employee ID).
		if code = opt.Code; code == "" {
			if code, err = NextEmployeeCode(ctx, tx, typ); err != nil {
				return "", false, err
			}
		}
		if _, err := tx.C("employee").Insert(ctx, append(bson.D{{Key: "_id", Value: code}}, append(job, bson.E{Key: "prev_code", Value: strPtr(rejoin)})...)); err != nil {
			return "", false, err
		}
	}
	if _, err := tx.C("gross_history").Upsert(ctx, bson.D{{Key: "employee_id", Value: code}, {Key: "from_month", Value: doj[:7]}},
		bson.M{"gross": o["gross_f"], "reason": "Joining"}, bson.M{"created_by": by}); err != nil {
		return "", false, err
	}
	if d["structId"] != "" {
		if _, err := tx.C("structure_map").Upsert(ctx, bson.D{{Key: "employee_id", Value: code}, {Key: "from_date", Value: doj}},
			bson.M{"structure_id": d["structId"]}, nil); err != nil {
			return "", false, err
		}
	}
	// The offer's variable target (paycomp.go), in % of the fixed gross or ₹ a month, from the joining month.
	t := calc.RulesFor(calc.EmploymentType(typ))
	designed := false
	vmode, vval := offerVar(d)
	{
		sid := d["structId"]
		if sid == "" {
			sid = compStructureDefault(ctx, tx)
		}
		if _, st, err := structureDoc(ctx, tx, sid); err == nil && st.Designed {
			designed = true
			if t.Variable && vval != nil && *vval > 0 {
				set := bson.M{"mode": vmode, "pct": nil, "total": calc.Round2(*vval), "reason": "Joining", "created_by": by}
				if vmode == "pct" {
					g, _ := o["gross_f"].(float64)
					set["pct"], set["total"] = *vval, calc.Round2(g**vval/100)
				}
				if _, err := tx.C(store.VarTargetColl).Upsert(ctx, bson.D{{Key: "employee_id", Value: code}, {Key: "from_month", Value: doj[:7]}}, set, nil); err != nil {
					return "", false, err
				}
			}
		}
	}
	pay := [5]any{nil, nil, 0.0, 0.0, nil}
	if t.Variable && !designed && vmode == "pct" { // a structure still on percentage rates
		pay[0], pay[1] = vval, intOr(d["varCycle"])
	}
	if t.Ded == "all" {
		if v := numOr(d["dMeal"]); v != nil {
			pay[2] = *v
		}
		if v := numOr(d["dAccom"]); v != nil {
			pay[3] = *v
		}
	}
	if t.Ded == "tds" {
		pay[4] = numOr(d["tdsPct"])
	}
	paySet := bson.M{"var_pct": pay[0], "var_cycle": pay[1], "meal": pay[2], "accommodation": pay[3], "tds_pct": pay[4], "comp_amts": nil}
	if r := offerReimb(d); r > 0 && calc.EmploymentType(typ) != calc.Retainer { // the offer's Reimbursement (reimb.go)
		paySet["comp_amts"] = bson.M{calc.ReimbCode: calc.Round2(r)}
	}
	if _, err := tx.C("employee_pay").Upsert(ctx, bson.D{{Key: "employee_id", Value: code}}, paySet, nil); err != nil {
		return "", false, err
	}
	if err := applyPersonal(ctx, tx, code, doj, d, by); err != nil {
		return "", false, err
	}
	if err := applyOfferExtras(ctx, tx, code, o, d); err != nil { // Existing UAN?, the PAN check (b1_employee.go)
		return "", false, err
	}
	if _, err := tx.C("offer").Update(ctx, bson.D{{Key: "_id", Value: o["id"]}}, bson.D{{Key: "$set", Value: bson.D{{Key: "employee_id", Value: code}}}}); err != nil {
		return "", false, err
	}
	// Documents gathered for the offer (before stages) or for the candidate (at every stage) belong to the record.
	if _, err := tx.C("document").UpdateMany(ctx, bson.D{{Key: "$or", Value: bson.A{
		bson.D{{Key: "owner_kind", Value: "offer"}, {Key: "owner_id", Value: o["id"]}},
		bson.D{{Key: "owner_kind", Value: "candidate"}, {Key: "owner_id", Value: str(o["candidate_id"])}},
	}}}, bson.D{{Key: "$set", Value: bson.D{{Key: "employee_id", Value: code}}}}); err != nil {
		return "", false, err
	}
	return code, rejoin != "" && mode2 != "new", nil
}

func finalizeVerify(ctx context.Context, tx store.Q, r *Request) (string, error) {
	today := store.DateOf(tx.Now()) // today, as the transaction sees it
	o, err := tx.C("offer").Lock(ctx, bson.D{{Key: "verify_request_id", Value: r.ID}})
	if err != nil {
		return "", err
	}
	if o == nil {
		// A verification raised without an offer (imported staff) just marks the record verified, and active from the
		// joining date.
		e, err := onboardingOne(ctx, tx, "employee", r.EmployeeID)
		if err != nil || e == nil {
			return "Verified by Payroll.", err
		}
		set := bson.D{{Key: "verified", Value: true}}
		if doj := str(e["doj"]); doj != "" && doj <= today {
			set = append(set, bson.E{Key: "status", Value: "Active"})
		}
		_, err = tx.C("employee").Update(ctx, bson.D{{Key: "_id", Value: r.EmployeeID}}, bson.D{{Key: "$set", Value: set}})
		return "Verified by Payroll.", err
	}
	d := rowData(o)
	cand := str(o["candidate_id"])
	if cand != "" { // closed meanwhile (early exit, for example): the record stays as it is
		if x, _ := onboardingOne(ctx, tx, "candidate", cand); onboardingCol(x, "stage") == "Closed" {
			return fmt.Sprintf("%s is closed (%s); nothing changed.", cand, onboardingCol(x, "closed_kind")), nil
		}
	}
	empID, doj := o["employee_id"].(string), o["doj"].(string)
	// A candidate's record starts on the actual joining date, not the offer's.
	if e, err := onboardingOne(ctx, tx, "employee", empID); err == nil && e != nil {
		doj = str(e["doj"])
	}
	if err := applyPersonal(ctx, tx, empID, doj, d, "Verification "+r.ID); err != nil {
		return "", err
	}
	if err := applyOfferExtras(ctx, tx, empID, o, d); err != nil { // Existing UAN?, the PAN check (b1_employee.go)
		return "", err
	}
	// Reporting and shift collected at joining.
	set := bson.D{}
	for _, x := range [][2]string{{"shift_code", d["shift"]}, {"attendance_mode", d["attMode"]}, {"manager_id", d["l1"]}, {"functional_manager_id", d["l2"]}} {
		if x[1] != "" {
			set = append(set, bson.E{Key: x[0], Value: x[1]})
		}
	}
	if len(set) > 0 {
		if _, err := tx.C("employee").Update(ctx, bson.D{{Key: "_id", Value: empID}}, bson.D{{Key: "$set", Value: set}}); err != nil {
			return "", err
		}
	}
	// Verified: active from the joining date (today or before), else pre-boarding until then.
	e, err := onboardingOne(ctx, tx, "employee", empID)
	if err != nil {
		return "", err
	}
	if e == nil {
		return "", store.ErrNoRows
	}
	status, name := "Pre-boarding", str(e["name"])
	if str(e["doj"]) <= today {
		status = "Active"
	}
	if _, err := tx.C("employee").Update(ctx, bson.D{{Key: "_id", Value: empID}}, bson.D{{Key: "$set", Value: bson.D{{Key: "verified", Value: true}, {Key: "status", Value: status}}}}); err != nil {
		return "", err
	}
	if _, err := tx.C("offer").Update(ctx, bson.D{{Key: "_id", Value: o["id"]}}, bson.D{{Key: "$set", Value: bson.D{{Key: "verified_at", Value: tx.Now()}}}}); err != nil {
		return "", err
	}
	if err := stopJoinerLink(ctx, tx, str(o["id"]), "Verification "+r.ID, "Payroll verified the joining details"); err != nil {
		return "", err
	}
	// The prototype's wording (finalize, verify).
	msg := name + " is verified by Payroll and is now active for payroll."
	if status != "Active" {
		msg = fmt.Sprintf("%s is verified by Payroll. They become active on the joining date, %s.", name, longDate(doj))
	}
	if cand != "" { // the last approval issues the Employee ID; the Joining ID record moves to it (hiring.go)
		code, err := approveHire(ctx, tx, nil, cand)
		if err != nil {
			return "", err
		}
		switch x, _ := onboardingOne(ctx, tx, "candidate", cand); {
		case code != "" && code != empID:
			msg += fmt.Sprintf(" Employee ID %s issued; %s stays as an alias.", code, empID)
		case code != "":
			msg += " Approved on " + code + "."
		case x != nil && x["ji_at"] != nil && x["approved_at"] == nil:
			msg += " The Employee ID waits for a clear background check."
		}
	}
	return msg, nil
}

// ---------- Rejoin ----------

// rejoinPrefill fills the offer form from an inactive record (spec 8.7).
func (s *Server) rejoinPrefill(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("onb.raise", 2); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
		return nil, err
	}
	e, err := onboardingLeaver(ctx, s.st.DB, id)
	if err != nil || e == nil {
		return nil, orNotFound(err)
	}
	if e["status"] != "Inactive" {
		return nil, bad("Only someone who has left can rejoin")
	}
	first, last, _ := strings.Cut(e["name"].(string), " ")
	g, _ := grossFor(ctx, s.st.DB, id, today()[:7])
	data := map[string]any{"first": first, "last": last, "type": e["type"], "dept": e["dept"], "desig": e["desig"], "grade": e["grade"], "city": e["city"], "loc": e["loc"],
		"gross": g, "phone": e["mobile"], "email": e["email"], "gender": e["gender"], "dob": e["dob"], "addr": e["address"], "pan": e["pan"], "aadhaar": e["aadhaar"],
		"uan": e["uan"], "rejoinOf": id}
	for k, v := range data {
		if v == nil || v == "" {
			delete(data, k)
		}
	}
	rehire, _ := e["rehire"].(string)
	if rehire == "" {
		rehire = "Yes"
	}
	settings, _ := onboardingOne(ctx, s.st.DB, "employee_code_settings", 1)
	mode := str(onboardingCol(settings, "rejoin"))
	return map[string]any{"data": data, "rehire": rehire, "exitReason": e["exit_reason"], "lwd": e["lwd"],
		"codeMode": map[bool]string{true: "A new code linked to " + id, false: "The same code " + id + " with a new service period"}[mode == "new"]}, nil
}

// onboardingLeaver is the record a rejoin starts from: the employee (id, name, type, status, grade, rehire,
// exit_reason, doj, lwd, manager_id) with the names of their department (dept) and designation (desig), their city
// and location ids (city, loc), their manager's status (manager_status), and their personal and ID details (mobile,
// email, gender, dob, address, pan, aadhaar, uan; null where there are none). nil when there is no such employee or
// their department, designation or place is missing (the old inner joins).
func onboardingLeaver(ctx context.Context, q store.Q, id string) (store.Row, error) {
	e, err := onboardingOne(ctx, q, "employee", id)
	if err != nil || e == nil {
		return nil, err
	}
	d, err := onboardingOne(ctx, q, "department", e["department_id"])
	if err != nil || d == nil {
		return nil, err
	}
	g, err := onboardingOne(ctx, q, "designation", e["designation_id"])
	if err != nil || g == nil {
		return nil, err
	}
	loc, city, err := onboardingPlace(ctx, q, str(e["location_id"]))
	if err != nil || loc == nil {
		return nil, err
	}
	var mgr store.Row
	if m := str(e["manager_id"]); m != "" {
		if mgr, err = onboardingOne(ctx, q, "employee", m); err != nil {
			return nil, err
		}
	}
	p, err := q.C("employee_personal").FindOne(ctx, bson.D{{Key: "employee_id", Value: id}})
	if err != nil {
		return nil, err
	}
	gi, err := q.C("employee_gov_id").FindOne(ctx, bson.D{{Key: "employee_id", Value: id}})
	if err != nil {
		return nil, err
	}
	return store.Row{"id": e["id"], "name": e["name"], "type": e["type"], "status": e["status"], "grade": e["grade"], "rehire": e["rehire"],
		"exit_reason": e["exit_reason"], "doj": e["doj"], "lwd": e["lwd"], "manager_id": e["manager_id"], "manager_status": onboardingCol(mgr, "status"),
		"dept": d["name"], "desig": g["name"], "city": city, "loc": e["location_id"],
		"mobile": onboardingCol(p, "mobile"), "email": onboardingCol(p, "email"), "gender": onboardingCol(p, "gender"), "dob": onboardingCol(p, "dob"),
		"address": onboardingCol(p, "address"), "pan": onboardingCol(gi, "pan"), "aadhaar": onboardingCol(gi, "aadhaar"), "uan": onboardingCol(gi, "uan")}, nil
}

// ---------- Company details ----------

type companyRow struct {
	Name          string `json:"name"`
	LegalName     string `json:"legalName"`
	Address       string `json:"address"`
	PAN           string `json:"pan"`
	TAN           string `json:"tan"`
	GSTIN         string `json:"gstin"`
	PFCode        string `json:"pfCode"`
	ESICode       string `json:"esiCode"`
	OfferTemplate string `json:"offerTemplate"`
}

func (s *Server) companyGet(r *http.Request, c *Ctx) (any, error) {
	var x companyRow
	cs, err := onboardingOne(r.Context(), s.st.DB, "company_setting", 1)
	if err == nil && cs == nil {
		err = store.ErrNoRows
	}
	if err != nil {
		return x, err
	}
	x = companyRow{Name: str(cs["name"]), LegalName: str(cs["legal_name"]), Address: str(cs["address"]), PAN: str(cs["pan"]), TAN: str(cs["tan"]),
		GSTIN: str(cs["gstin"]), PFCode: str(cs["pf_code"]), ESICode: str(cs["esi_code"]), OfferTemplate: str(cs["offer_template"])}
	return x, nil
}

var reTAN = regexp.MustCompile(`^[A-Z]{4}\d{5}[A-Z]$`)

func (s *Server) companyPut(r *http.Request, c *Ctx) (any, error) {
	if c.Role != "HR Admin" {
		return nil, forbidden("Only HR Admin can change company details")
	}
	var x companyRow
	if err := decode(r, &x); err != nil {
		return nil, err
	}
	x.PAN, x.TAN, x.GSTIN = strings.ToUpper(strings.TrimSpace(x.PAN)), strings.ToUpper(strings.TrimSpace(x.TAN)), strings.ToUpper(strings.TrimSpace(x.GSTIN))
	switch {
	case strings.TrimSpace(x.Name) == "" || strings.TrimSpace(x.LegalName) == "":
		return nil, bad("Enter the company name and legal name")
	case x.PAN != "" && !rePAN.MatchString(x.PAN):
		return nil, bad("Company PAN format looks wrong (ABCDE1234F)")
	case x.TAN != "" && !reTAN.MatchString(x.TAN):
		return nil, bad("TAN format looks wrong (ABCD12345E)")
	case !strings.Contains(x.OfferTemplate, "{{name}}"):
		return nil, bad("The offer letter template must contain {{name}}")
	}
	return nil, s.st.Tx(r.Context(), func(tx store.Q) error {
		if _, err := tx.C("company_setting").Update(r.Context(), bson.D{{Key: "_id", Value: 1}}, bson.D{{Key: "$set", Value: bson.D{
			{Key: "name", Value: strings.TrimSpace(x.Name)}, {Key: "legal_name", Value: strings.TrimSpace(x.LegalName)}, {Key: "address", Value: strings.TrimSpace(x.Address)},
			{Key: "pan", Value: x.PAN}, {Key: "tan", Value: x.TAN}, {Key: "gstin", Value: x.GSTIN}, {Key: "pf_code", Value: strings.TrimSpace(x.PFCode)},
			{Key: "esi_code", Value: strings.TrimSpace(x.ESICode)}, {Key: "offer_template", Value: x.OfferTemplate}, {Key: "updated_at", Value: tx.Now()}}}}); err != nil {
			return err
		}
		return audit(r.Context(), tx, c, "Company details updated", "company_setting", "1", nil, x)
	})
}
