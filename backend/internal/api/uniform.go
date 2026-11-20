package api

// Uniform sizes and the uniform refund policy (owner's request of 13 Nov 2026: "add uniform size: Shirt / T-shirt,
// Trouser, Shoes, Length of trouser, so we track uniform assets, deduct from employee salary and reverse after some
// months of employment as per policy").
//
//   - Each employee's sizes (uniform_size): shirt or T-shirt, trouser waist and length (inches), shoes (UK). HR keeps
//     them on the employee record (Employees › record, the payroll file) or by bulk upload; a new joiner gives them
//     on the joining form (the Uniform section of Form setup), and they are copied to the record on joining.
//   - The size lists are HR Admin's, in Inventory › Setup (company_setting.uniform.sizes); so is each uniform item's
//     size type (inv_item.size_type), which pre-fills the size when uniform is issued.
//   - The refund of the uniform recovery: N months after the last deduction (the rule so far, the default) or after
//     N months of employment (the month the employee completes them, or the month after the last deduction when
//     that is later). Paid only to someone still employed then: a leaver's scheduled refund is cancelled when they
//     leave (or by their full and final), and the recovery stands.

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

const (
	// uniRefundLabel is the label of a uniform refund pay line.
	uniRefundLabel = "Uniform deposit refund"
	// uniRefundVoid starts the void reason of a refund cancelled because the employee left before it was due.
	uniRefundVoid = "No uniform refund: left before the refund month"
	// Refund bases.
	refundByDeduction = "deduction"
	refundByService   = "service"
)

// sizeKeys are the size kinds an employee has, in display order; sizeLabels their names.
var (
	sizeKeys   = []string{"shirt", "waist", "length", "shoe"}
	sizeLabels = map[string]string{"shirt": "Shirt / T-shirt size", "waist": "Trouser waist", "length": "Trouser length", "shoe": "Shoe size (UK)"}
	// sizeOfField is the size kind of each Form setup field of the Uniform section.
	sizeOfField = map[string]string{}
	// reSizeValue: a size is letters, digits, spaces, dots and dashes, 1 to 12 characters (no commas: the joining
	// form keeps a list as comma separated text).
	reSizeValue = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 .\-]{0,11}$`)
)

func init() {
	for _, f := range store.UniformFields {
		sizeOfField[f.Key] = f.List
	}
}

// ---------- Settings: refund rule and size lists ----------

// refundRuleText is the refund rule in words.
func refundRuleText(us uniformSetting) string {
	if us.RefundBasis == refundByService {
		return fmt.Sprintf("after %d months of employment", us.ServiceMonths)
	}
	return fmt.Sprintf("%d months after the last deduction", us.RefundAfter)
}

// serviceDoneYm is the month in which someone who joined on doj completes n months of employment (the day before
// the same date n months later: joined 1 March, six months are complete on 31 August).
func serviceDoneYm(doj string, n int) string {
	if !isDate(doj) {
		return ""
	}
	if doj[8:] == "01" {
		return addYm(doj[:7], n-1)
	}
	return addYm(doj[:7], n)
}

type uniPart struct {
	ym  string
	amt float64
}

// uniformPlan is a uniform issue's salary plan: firstPct% in the first month, the rest the month after, and the
// refund month by the refund rule. doj is the employee's joining date (for the employment basis).
func uniformPlan(us uniformSetting, first string, amount float64, doj string) (parts []uniPart, refund string) {
	p1 := calc.Round2(amount * us.FirstPct / 100)
	last := first
	for _, p := range []uniPart{{first, p1}, {nextYm(first), calc.Round2(amount - p1)}} {
		if p.amt > 0 {
			parts = append(parts, p)
			last = p.ym
		}
	}
	return parts, uniformRefundYm(us, last, doj)
}

// uniformRefundYm is the refund month by the refund rule, from the month of the last deduction: RefundAfter months
// after it, or the month the employee (joined on doj) completes ServiceMonths of employment, but never before the
// month after the last deduction.
func uniformRefundYm(us uniformSetting, last, doj string) string {
	if us.RefundBasis == refundByService {
		return maxYm(serviceDoneYm(doj, us.ServiceMonths), nextYm(last))
	}
	return addYm(last, us.RefundAfter)
}

// cleanSizeList checks one size list: 1 to 40 values, each a short size (no commas), no repeats.
func cleanSizeList(label string, in []string) ([]string, error) {
	out := []string{}
	for _, v := range in {
		v = strings.Join(strings.Fields(v), " ")
		if v == "" {
			continue
		}
		if !reSizeValue.MatchString(v) {
			return nil, bad("%s: “%s” is not a size. Use letters and numbers, up to 12 characters", label, v)
		}
		for _, x := range out {
			if strings.EqualFold(x, v) {
				return nil, bad("%s: %s is listed twice", label, v)
			}
		}
		out = append(out, v)
	}
	if len(out) == 0 || len(out) > 40 {
		return nil, bad("%s: keep 1 to 40 sizes", label)
	}
	return out, nil
}

// invSizeListsPut saves the size lists (HR Admin, Inventory › Setup) and the options of the joining form's Uniform
// fields with them. An employee's size that is no longer listed stays on their record.
func (s *Server) invSizeListsPut(r *http.Request, c *Ctx) (any, error) {
	if err := canInvSetup(c); err != nil {
		return nil, err
	}
	var b map[string][]string
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	for k := range b {
		if sizeLabels[k] == "" {
			return nil, bad("Unknown size list %s", k)
		}
	}
	ctx := r.Context()
	var out map[string][]string
	err := s.st.Tx(ctx, func(tx store.Q) error {
		before := store.UniformSizeLists(ctx, tx)
		out = map[string][]string{}
		for _, k := range sizeKeys {
			out[k] = before[k]
			if l, ok := b[k]; ok {
				cl, err := cleanSizeList(sizeLabels[k], l)
				if err != nil {
					return err
				}
				out[k] = cl
			}
		}
		us := loadUniformSetting(ctx, tx)
		us.Sizes = out
		if _, err := tx.C("company_setting").Update(ctx, bson.M{"_id": 1}, bson.M{"$set": bson.M{"uniform": us, "updated_at": tx.Now()}}); err != nil {
			return err
		}
		for _, f := range store.UniformFields {
			if _, err := tx.C("onb_field").Update(ctx, bson.M{"key": f.Key}, bson.M{"$set": bson.M{"options": strings.Join(out[f.List], ",")}}); err != nil {
				return err
			}
		}
		return audit(ctx, tx, c, "Uniform size lists changed", "company_setting", "1", before, out)
	})
	return out, err
}

// ---------- An employee's sizes ----------

// sizesIn: nil keeps the current value, "" clears it.
type sizesIn struct {
	Shirt  *string `json:"shirt"`
	Waist  *string `json:"waist"`
	Length *string `json:"length"`
	Shoe   *string `json:"shoe"`
}

func (p sizesIn) values() map[string]*string {
	return map[string]*string{"shirt": p.Shirt, "waist": p.Waist, "length": p.Length, "shoe": p.Shoe}
}

// sizeMatch is the listed spelling of v (any letter case), or "" when v is not in the list.
func sizeMatch(list []string, v string) string {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return x
		}
	}
	return ""
}

// sizesOf reads the sizes on record ("" for none) for the employees given, by employee.
func sizesOf(ctx context.Context, q store.Q, emps []string) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	if len(emps) == 0 {
		return out, nil
	}
	rows, err := q.C(store.UniformSizeColl).Find(ctx, bson.M{"employee_id": bson.M{"$in": emps}})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		m := map[string]string{}
		for _, k := range sizeKeys {
			m[k] = str(r[k])
		}
		out[str(r["employee_id"])] = m
	}
	return out, nil
}

// saveSizes checks new sizes against the lists and saves the changes, each in the change history and the audit
// log. A size kept as it is need not be listed any more (the list changed since). It returns the problems instead
// when there are any.
func saveSizes(ctx context.Context, tx store.Q, c *Ctx, id string, in sizesIn, source string) ([]bulkChange, []string, error) {
	saved, err := tx.C(store.UniformSizeColl).FindOne(ctx, bson.M{"employee_id": id})
	if err != nil {
		return nil, nil, err
	}
	lists := store.UniformSizeLists(ctx, tx)
	cur, next := map[string]string{}, map[string]string{}
	for _, k := range sizeKeys {
		cur[k] = str(saved[k])
		next[k] = cur[k]
	}
	var errs []string
	for k, v := range in.values() {
		if v == nil {
			continue
		}
		val := strings.Join(strings.Fields(*v), " ")
		if val == "" || strings.EqualFold(val, cur[k]) {
			next[k] = map[bool]string{true: "", false: cur[k]}[val == ""]
			continue
		}
		if m := sizeMatch(lists[k], val); m != "" {
			next[k] = m
		} else {
			errs = append(errs, fmt.Sprintf("%s “%s” is not on the list (%s)", sizeLabels[k], val, strings.Join(lists[k], ", ")))
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return nil, errs, nil
	}
	changes := []bulkChange{}
	for _, k := range sizeKeys {
		if next[k] != cur[k] {
			changes = append(changes, bulkChange{Field: "Uniform: " + sizeLabels[k], From: cur[k], To: next[k]})
		}
	}
	if len(changes) == 0 {
		return changes, nil, nil
	}
	set := bson.M{"updated_at": tx.Now(), "updated_by": c.Name + " (" + c.Role + ")"}
	for _, k := range sizeKeys {
		set[k] = employeesNullIf(next[k])
	}
	if _, err := tx.C(store.UniformSizeColl).Upsert(ctx, bson.M{"employee_id": id}, set, nil); err != nil {
		return nil, nil, err
	}
	for _, ch := range changes {
		if _, err := tx.C("employee_change").Insert(ctx, bson.M{"employee_id": id, "field": ch.Field, "old_value": ch.From, "new_value": ch.To,
			"reason": "Uniform sizes", "effective_from": today(), "source": source, "changed_by": c.Name + " (" + c.Role + ")"}); err != nil {
			return nil, nil, err
		}
	}
	return changes, nil, audit(ctx, tx, c, "Uniform sizes changed", "employee", id, cur, next)
}

// canEditSizes: whoever edits employee details or manages inventory (emp.assets at Edit).
func canEditSizes(c *Ctx) bool {
	return c.Level("emp.directory") >= 2 || c.Level("emp.personal") >= 2 || c.Level("emp.assets") >= 2
}

// empUniformGet is an employee's uniform: sizes on record with the lists, and the uniform issued with its
// recovery and refund. For the person themself, and for those who see the employee directory, inventory or
// payroll, within their data access.
func (s *Server) empUniformGet(r *http.Request, c *Ctx) (any, error) {
	ctx, id := r.Context(), r.PathValue("id")
	if id != c.EmployeeID {
		if c.Level("emp.directory") < 1 && c.Level("emp.assets") < 1 && c.Level("pay.run") < 1 {
			return nil, forbidden("")
		}
		if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
			return nil, err
		}
	}
	e, err := s.st.DB.C("employee").FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(bson.M{"name": 1, "type": 1, "status": 1, "doj": 1, "lwd": 1}))
	if err != nil || e == nil {
		return nil, orNotFound(err)
	}
	return s.uniformView(ctx, s.st.DB, c, e)
}

// uniformView is what empUniformGet and My view show.
func (s *Server) uniformView(ctx context.Context, q store.Q, c *Ctx, e store.Row) (map[string]any, error) {
	id := str(e["id"])
	sizes := map[string]any{}
	for _, k := range sizeKeys {
		sizes[k] = ""
	}
	var updated any
	if row, err := q.C(store.UniformSizeColl).FindOne(ctx, bson.M{"employee_id": id}); err != nil {
		return nil, err
	} else if row != nil {
		for _, k := range sizeKeys {
			sizes[k] = str(row[k])
		}
		if at, ok := row["updated_at"].(time.Time); ok {
			updated = map[string]any{"at": at.In(store.Loc()).Format("02 Jan 2006"), "by": row["updated_by"]}
		}
	}
	issues, err := q.C("uniform_issue").Find(ctx, bson.M{"employee_id": id})
	if err != nil {
		return nil, err
	}
	rows, err := uniformIssueRows(ctx, q, issues, map[string]any{id: e["name"]})
	if err != nil {
		return nil, err
	}
	us := loadUniformSetting(ctx, q)
	return map[string]any{"employee": map[string]any{"id": id, "name": e["name"], "type": e["type"], "status": e["status"], "doj": e["doj"], "lwd": e["lwd"]},
		"sizes": sizes, "updated": updated, "lists": store.UniformSizeLists(ctx, q), "labels": sizeLabels,
		"canEdit": id != c.EmployeeID && canEditSizes(c) || id == c.EmployeeID && c.Role == "HR Admin",
		"issues":  rows, "rule": refundRuleText(us), "firstPct": us.FirstPct}, nil
}

// empUniformPut saves an employee's sizes.
func (s *Server) empUniformPut(r *http.Request, c *Ctx) (any, error) {
	if !canEditSizes(c) {
		return nil, forbidden("")
	}
	ctx, id := r.Context(), r.PathValue("id")
	if id == c.EmployeeID && c.Role != "HR Admin" {
		return nil, forbidden("Ask HR to change your uniform sizes")
	}
	if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
		return nil, err
	}
	var b sizesIn
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	var out []bulkChange
	err := s.st.Tx(ctx, func(tx store.Q) error {
		if ok, err := tx.C("employee").Exists(ctx, bson.M{"_id": id}); err != nil || !ok {
			return orNotFound(err)
		}
		ch, errs, err := saveSizes(ctx, tx, c, id, b, "Manual")
		if err == nil && len(errs) > 0 {
			return badD(errs[0], errs)
		}
		out = ch
		return err
	})
	return map[string]any{"changes": out}, err
}

// applyJoiningSizes copies the sizes a joiner gave on the joining form to their record (a blank keeps what the
// record has, as for the other joining details).
func applyJoiningSizes(ctx context.Context, tx store.Q, empID string, d map[string]string, by string) error {
	set := bson.M{}
	for _, f := range store.UniformFields {
		if v := strings.TrimSpace(d[f.Key]); v != "" {
			set[f.List] = v
		}
	}
	if len(set) == 0 {
		return nil
	}
	set["updated_at"], set["updated_by"] = tx.Now(), by
	_, err := tx.C(store.UniformSizeColl).Upsert(ctx, bson.M{"employee_id": empID}, set, nil)
	return err
}

// uniformFormErrors checks the Uniform fields of joining details against the size lists.
func uniformFormErrors(ctx context.Context, q store.Q, d map[string]string, errs *[]string) {
	var lists map[string][]string
	for _, f := range store.UniformFields {
		v := strings.TrimSpace(d[f.Key])
		if v == "" {
			continue
		}
		if lists == nil {
			lists = store.UniformSizeLists(ctx, q)
		}
		if sizeMatch(lists[f.List], v) == "" {
			*errs = append(*errs, f.Label+": choose one of the options")
		}
	}
}

// ---------- Issuing by size ----------

// issueSize checks the size of an issued line by the item's size type and gives it as listed: a shirt size, a
// trouser waist with an optional length ("32/30"), a shoe size, or any short text for other items. Blank is allowed.
func issueSize(lists map[string][]string, sizeType, name, v string) (string, error) {
	v = strings.Join(strings.Fields(v), " ")
	if v == "" {
		return "", nil
	}
	switch sizeType {
	case store.SizeShirt, store.SizeShoes:
		key := map[string]string{store.SizeShirt: "shirt", store.SizeShoes: "shoe"}[sizeType]
		if m := sizeMatch(lists[key], v); m != "" {
			return m, nil
		}
		return "", bad("%s: size %s is not on the %s list", name, v, strings.ToLower(sizeLabels[key]))
	case store.SizeTrouser:
		w, l, _ := strings.Cut(strings.ReplaceAll(v, " ", ""), "/")
		mw := sizeMatch(lists["waist"], w)
		if mw == "" {
			return "", bad("%s: waist %s is not on the list", name, w)
		}
		if l == "" {
			return mw, nil
		}
		ml := sizeMatch(lists["length"], l)
		if ml == "" {
			return "", bad("%s: length %s is not on the list", name, l)
		}
		return mw + "/" + ml, nil
	}
	if len(v) > 20 {
		return "", bad("%s: keep the size under 20 characters", name)
	}
	return v, nil
}

// cleanSizeType checks a uniform item's size type ("" or "other" for an item without a size).
func cleanSizeType(v string) (string, error) {
	if v == "" {
		return store.SizeOther, nil
	}
	if !containsStr(store.SizeTypes, v) {
		return "", bad("The size type is shirt, trouser, shoes or other")
	}
	return v, nil
}

// ---------- Recovery and refund views ----------

// uniformIssueRows are uniform issues (newest first) with the recovery, from the pay lines: the schedule, what is
// recovered and what is left, the refund month and the refund's state: scheduled, paid, cancelled (left before
// it), in the full and final, or none (no recovery).
func uniformIssueRows(ctx context.Context, q store.Q, issues []store.Row, names map[string]any) ([]store.Row, error) {
	sort.SliceStable(issues, func(i, j int) bool {
		if di, dj := invIssueDate(issues[i]), invIssueDate(issues[j]); di != dj {
			return di > dj
		}
		ai, _ := issues[i]["at"].(time.Time)
		aj, _ := issues[j]["at"].(time.Time)
		return ai.After(aj)
	})
	if len(issues) > 500 {
		issues = issues[:500]
	}
	open := openYm(ctx, q)
	rows, idx, uids := []store.Row{}, map[string]store.Row{}, []string{}
	for _, u := range issues {
		id := str(u["id"])
		x := store.Row{"id": u["id"], "employeeId": u["employee_id"], "name": names[str(u["employee_id"])], "date": invIssueDate(u), "lines": u["lines"], "total": u["amount"],
			"recover": u["recover"], "returned": u["returned"], "storeId": u["store_id"], "by": u["by_name"], "schedule": []map[string]any{}, "refundYm": nil,
			"refundStatus": "none", "refundNote": nil, "refundRule": u["refund_rule"], "recovered": 0.0, "toRecover": 0.0}
		if u["opening"] == true {
			// Issued before the HRMS (Bulk update › Opening uniform): the instalments before the HRMS's first deduction
			// month were recovered outside it and have no pay line; they lead the schedule, marked as such.
			x["opening"], x["note"], x["issuedYm"] = true, u["note"], u["issued_ym"]
			sched := []map[string]any{}
			for _, p := range openingBefore(u["before_hrms"]) {
				sched = append(sched, map[string]any{"ym": p.ym, "amount": p.amt, "before": true})
				x["recovered"] = calc.Round2(x["recovered"].(float64) + p.amt)
			}
			x["schedule"] = sched
		}
		rows, idx[id], uids = append(rows, x), x, append(uids, id)
	}
	if len(uids) == 0 {
		return rows, nil
	}
	lines, err := q.C("ledger").Find(ctx, bson.D{{Key: "source_id", Value: repIn(uids)}}, options.Find().SetSort(bson.D{{Key: "ym", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	for _, l := range lines {
		u := idx[str(l["source_id"])]
		if u == nil {
			continue
		}
		ym, amt, void := str(l["ym"]), repFloat(l["amount"]), l["void"] == true
		why := str(l["void_reason"])
		if l["kind"] == "ded" {
			if void {
				if strings.HasPrefix(why, "Brought forward to full and final") {
					u["toRecover"] = calc.Round2(u["toRecover"].(float64) + amt)
				}
				continue
			}
			u["schedule"] = append(u["schedule"].([]map[string]any), map[string]any{"ym": ym, "amount": l["amount"]})
			if ym < open {
				u["recovered"] = calc.Round2(u["recovered"].(float64) + amt)
			} else {
				u["toRecover"] = calc.Round2(u["toRecover"].(float64) + amt)
			}
			continue
		}
		switch {
		case !void:
			u["refundYm"] = ym
			u["refundStatus"] = map[bool]string{true: "paid", false: "scheduled"}[ym < open]
			u["refundNote"] = nil
		case strings.HasPrefix(why, uniRefundVoid):
			if u["refundStatus"] != "scheduled" && u["refundStatus"] != "paid" {
				u["refundYm"], u["refundStatus"], u["refundNote"] = ym, "cancelled", why
			}
		case strings.HasPrefix(why, "Brought forward to full and final"):
			if u["refundStatus"] == "none" {
				u["refundYm"], u["refundStatus"], u["refundNote"] = ym, "fnf", why
			}
		}
	}
	return rows, nil
}

func repFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	}
	return 0
}

// ---------- Leavers: no refund ----------

// uniformRefundCancel cancels the uniform refunds scheduled after the month of an employee's last working day
// (policy: the refund is paid only to staff still employed in the refund month; the recovery stands). It returns
// how many it cancelled.
func uniformRefundCancel(ctx context.Context, tx store.Q, emp, lwd string) (int, error) {
	if !isDate(lwd) {
		return 0, nil
	}
	rows, err := tx.C("ledger").Find(ctx, bson.M{"employee_id": emp, "kind": "earn", "void": false, "source_id": bson.M{"$regex": "^UNI-"}, "ym": bson.M{"$gt": lwd[:7]}})
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	why := fmt.Sprintf("%s (last working day %s)", uniRefundVoid, longDate(lwd))
	for _, l := range rows {
		if _, err := tx.C("ledger").Update(ctx, bson.M{"_id": l["id"]}, bson.M{"$set": bson.M{"void": true, "void_reason": why, "voided_at": tx.Now()}}); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}

// uniformRefundRestore puts back the refunds cancelled for leaving, when the leaving is undone (marked inactive by
// mistake).
func uniformRefundRestore(ctx context.Context, tx store.Q, emp string) error {
	_, err := tx.C("ledger").UpdateMany(ctx, bson.M{"employee_id": emp, "kind": "earn", "void": true, "source_id": bson.M{"$regex": "^UNI-"},
		"void_reason": bson.M{"$regex": "^" + regexp.QuoteMeta(uniRefundVoid)}}, bson.M{"$set": bson.M{"void": false, "void_reason": nil, "voided_at": nil}})
	return err
}

// uniformPolicyFrom is the day the leaver rule started: the day migration 008 was applied ("" before it). People
// who left before it keep the refund schedule they had.
func uniformPolicyFrom(ctx context.Context, st *store.Store) string {
	var m struct {
		At time.Time `bson:"applied_at"`
	}
	if err := st.DB.Database().Collection(store.MigrationsColl).FindOne(ctx, bson.M{"_id": "008_uniform_sizes"}).Decode(&m); err != nil || m.At.IsZero() {
		return ""
	}
	return store.DateOf(m.At)
}

// sweepUniformRefunds cancels the refunds still scheduled for people whose last working day has passed (the
// periodic maintenance: leavers marked by a separation, a bulk upload or the daily status change). Only last working
// days from the day the rule started (uniformPolicyFrom): the refund schedule of earlier leavers is left as it was.
func sweepUniformRefunds(ctx context.Context, st *store.Store) (int, error) {
	from := uniformPolicyFrom(ctx, st)
	if from == "" {
		return 0, nil
	}
	emps, err := st.DB.C("ledger").Distinct(ctx, "employee_id", bson.M{"kind": "earn", "void": false, "source_id": bson.M{"$regex": "^UNI-"}})
	if err != nil || len(emps) == 0 {
		return 0, err
	}
	gone, err := st.DB.C("employee").Find(ctx, bson.M{"_id": bson.M{"$in": emps}, "lwd": bson.M{"$ne": nil, "$lt": store.Today(), "$gte": from}},
		options.Find().SetProjection(bson.M{"lwd": 1}))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range gone {
		err := st.Tx(ctx, func(tx store.Q) error {
			k, err := uniformRefundCancel(ctx, tx, str(e["id"]), str(e["lwd"]))
			if err == nil && k > 0 {
				n += k
				return audit(ctx, tx, nil, "Uniform refund cancelled: left before the refund month", "employee", str(e["id"]), nil, map[string]any{"lines": k, "lwd": e["lwd"]})
			}
			return err
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// fnfUniformRefund is the full and final's line for uniform refunds the leaver will not get: those due after the
// last working day's month, still scheduled (Src: voided on approval) or already cancelled. nil when there are none.
func fnfUniformRefund(ctx context.Context, q store.Q, id, lwd string) (*fnfLine, error) {
	fm := lwd[:7]
	rows, err := q.C("ledger").Find(ctx, bson.M{"employee_id": id, "kind": "earn", "source_id": bson.M{"$regex": "^UNI-"}, "ym": bson.M{"$gt": fm},
		"$or": bson.A{bson.M{"void": false}, bson.M{"void_reason": bson.M{"$regex": "^" + regexp.QuoteMeta(uniRefundVoid)}}}},
		options.Find().SetSort(bson.D{{Key: "ym", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	l := &fnfLine{Key: "unirefund", Kind: "info", Label: "Uniform deposit refund not paid"}
	months := []string{}
	for _, r := range rows {
		l.Amount += repFloat(r["amount"])
		if r["void"] == false {
			l.Src = append(l.Src, repInt64(r["id"]))
		}
		if m := ymLabel(str(r["ym"])); !containsStr(months, m) {
			months = append(months, m)
		}
	}
	l.Note = fmt.Sprintf("Due in %s, after the last working day (%s). As per policy the refund is paid only to staff still employed then; the uniform recovery stands",
		strings.Join(months, ", "), longDate(lwd))
	return l, nil
}

// ---------- Sizes summary for ordering ----------

type sizeCount struct {
	Size  string `json:"size"`
	Count int    `json:"count"`
}

// invSizesData counts the active employees in scope by size, per kind (shirt, waist, length, shoes, and trouser
// waist with length), narrowed to a location and a department, and lists those with no size at all.
func (s *Server) invSizesData(ctx context.Context, c *Ctx, loc, dept string) (map[string]any, []*emp, map[string]map[string]string, error) {
	q := s.st.DB
	all, err := scopedEmps(ctx, q, c, bson.D{{Key: "status", Value: "Active"}})
	if err != nil {
		return nil, nil, nil, err
	}
	locs, depts := map[string]string{}, map[string]bool{}
	emps := []*emp{}
	for _, e := range all {
		locs[e.LocID], depts[e.Dept] = e.Location, true
		if (loc == "" || e.LocID == loc) && (dept == "" || e.Dept == dept) {
			emps = append(emps, e)
		}
	}
	sizes, err := sizesOf(ctx, q, ids(emps))
	if err != nil {
		return nil, nil, nil, err
	}
	lists := store.UniformSizeLists(ctx, q)
	groups := []map[string]any{}
	for _, k := range sizeKeys {
		count, extra := map[string]int{}, []string{}
		none := 0
		for _, e := range emps {
			v := sizes[e.ID][k]
			if v == "" {
				none++
				continue
			}
			if count[v] == 0 && sizeMatch(lists[k], v) == "" && !containsStr(extra, v) {
				extra = append(extra, v)
			}
			count[v]++
		}
		rows := []sizeCount{}
		for _, v := range append(append([]string{}, lists[k]...), extra...) {
			rows = append(rows, sizeCount{v, count[v]})
		}
		groups = append(groups, map[string]any{"key": k, "label": sizeLabels[k], "rows": rows, "notRecorded": none})
	}
	pairs := map[string]int{}
	for _, e := range emps {
		if w := sizes[e.ID]["waist"]; w != "" {
			pairs[w+"/"+or(sizes[e.ID]["length"], "—")]++
		}
	}
	trousers := []map[string]any{}
	for p, n := range pairs {
		w, l, _ := strings.Cut(p, "/")
		trousers = append(trousers, map[string]any{"waist": w, "length": l, "count": n})
	}
	pos := func(list []string, v string) int {
		for i, x := range list {
			if x == v {
				return i
			}
		}
		return len(list)
	}
	sort.Slice(trousers, func(i, j int) bool {
		a, b := trousers[i], trousers[j]
		if wa, wb := pos(lists["waist"], a["waist"].(string)), pos(lists["waist"], b["waist"].(string)); wa != wb {
			return wa < wb
		}
		return pos(lists["length"], a["length"].(string)) < pos(lists["length"], b["length"].(string))
	})
	missing := []map[string]any{}
	recorded := 0
	for _, e := range emps {
		m := sizes[e.ID]
		if m["shirt"] == "" && m["waist"] == "" && m["length"] == "" && m["shoe"] == "" {
			missing = append(missing, map[string]any{"id": e.ID, "name": e.Name, "department": e.Dept, "designation": e.Desig, "location": e.Location})
		} else {
			recorded++
		}
	}
	locList := []map[string]string{}
	for id, name := range locs {
		locList = append(locList, map[string]string{"id": id, "name": name})
	}
	sort.Slice(locList, func(i, j int) bool { return locList[i]["name"] < locList[j]["name"] })
	deptList := []string{}
	for d := range depts {
		deptList = append(deptList, d)
	}
	sort.Strings(deptList)
	return map[string]any{"total": len(emps), "recorded": recorded, "groups": groups, "trousers": trousers, "missing": missing,
		"locations": locList, "departments": deptList, "loc": loc, "dept": dept}, emps, sizes, nil
}

// invSizesGet is the Uniform sizes summary (Employee dues › Uniform sizes): for ordering stock.
func (s *Server) invSizesGet(r *http.Request, c *Ctx) (any, error) {
	if err := s.canInv(c, 1); err != nil {
		return nil, err
	}
	out, _, _, err := s.invSizesData(r.Context(), c, r.URL.Query().Get("loc"), r.URL.Query().Get("dept"))
	return out, err
}

// invSizesSheet is the summary as an Excel file: the counts by size, trousers by waist and length, every active
// employee with their sizes, and those with none.
func (s *Server) invSizesSheet(w http.ResponseWriter, r *http.Request) {
	c, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.canInv(c, 1); err != nil {
		writeErr(w, err)
		return
	}
	if !c.CanDownload {
		writeErr(w, forbidden("Your role cannot download files"))
		return
	}
	ctx := r.Context()
	d, emps, sizes, err := s.invSizesData(ctx, c, r.URL.Query().Get("loc"), r.URL.Query().Get("dept"))
	if err != nil {
		writeErr(w, err)
		return
	}
	f := excelize.NewFile()
	defer f.Close()
	head, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}, Fill: excelize.Fill{Type: "pattern", Color: []string{"E1E5E8"}, Pattern: 1}})
	put := func(sh string, cols []string, rows [][]any, widths ...float64) {
		for i, h := range cols {
			cell, _ := excelize.CoordinatesToCellName(i+1, 1)
			_ = f.SetCellValue(sh, cell, h)
			col, _ := excelize.ColumnNumberToName(i + 1)
			wd := 16.0
			if i < len(widths) {
				wd = widths[i]
			}
			_ = f.SetColWidth(sh, col, col, wd)
		}
		end, _ := excelize.ColumnNumberToName(len(cols))
		_ = f.SetCellStyle(sh, "A1", end+"1", head)
		_ = f.SetPanes(sh, &excelize.Panes{Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft"})
		for i, row := range rows {
			_ = f.SetSheetRow(sh, fmt.Sprintf("A%d", i+2), &row)
		}
	}
	_ = f.SetSheetName("Sheet1", "By size")
	rows := [][]any{}
	for _, g := range d["groups"].([]map[string]any) {
		for _, x := range g["rows"].([]sizeCount) {
			rows = append(rows, []any{g["label"], x.Size, x.Count})
		}
		rows = append(rows, []any{g["label"], "Not recorded", g["notRecorded"]})
	}
	put("By size", []string{"Item", "Size", "Active employees"}, rows, 24, 14, 18)
	_, _ = f.NewSheet("Trousers")
	rows = [][]any{}
	for _, x := range d["trousers"].([]map[string]any) {
		rows = append(rows, []any{x["waist"], x["length"], x["count"]})
	}
	put("Trousers", []string{"Waist (inches)", "Length (inches)", "Active employees"}, rows, 16, 16, 18)
	_, _ = f.NewSheet("Employees")
	rows = [][]any{}
	for _, e := range emps {
		m := sizes[e.ID]
		rows = append(rows, []any{e.ID, e.Name, e.Dept, e.Desig, e.Location, m["shirt"], m["waist"], m["length"], m["shoe"]})
	}
	put("Employees", []string{"Employee ID", "Name", "Department", "Designation", "Location", "Shirt / T-shirt", "Trouser waist", "Trouser length", "Shoe size (UK)"}, rows,
		14, 26, 20, 22, 24, 14, 14, 14, 14)
	_, _ = f.NewSheet("No sizes")
	rows = [][]any{}
	for _, x := range d["missing"].([]map[string]any) {
		rows = append(rows, []any{x["id"], x["name"], x["department"], x["designation"], x["location"]})
	}
	put("No sizes", []string{"Employee ID", "Name", "Department", "Designation", "Location"}, rows, 14, 26, 20, 22, 24)
	f.SetActiveSheet(0)
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="uniform-sizes-%s.xlsx"`, today()))
	if err := f.Write(w); err != nil {
		return
	}
	_ = audit(ctx, s.st.DB, c, "Downloaded uniform sizes", "uniform_size", "summary", nil, map[string]any{"rows": len(emps), "loc": d["loc"], "dept": d["dept"]})
}

// ---------- Bulk: Uniform sizes ----------

var uniBulkCols = []string{"Employee ID", "Shirt / T-shirt Size", "Trouser Waist", "Trouser Length", "Shoe Size (UK)"}

// uniBulkKey is the size kind of each template column.
var uniBulkKey = map[string]string{"Shirt / T-shirt Size": "shirt", "Trouser Waist": "waist", "Trouser Length": "length", "Shoe Size (UK)": "shoe"}

func uniBulkList(k string) bulkList {
	return func(ctx context.Context, q store.Q) ([]string, error) { return store.UniformSizeLists(ctx, q)[k], nil }
}

func init() {
	lists := map[string]bulkList{}
	for col, k := range uniBulkKey {
		lists[col] = uniBulkList(k)
	}
	bulkTemplates = append(bulkTemplates, &bulkTemplate{Key: "uni", Name: "Uniform sizes", Data: true, Cols: uniBulkCols,
		Hint: "Each employee's uniform sizes, used to issue uniform by size and to order stock. Download the current data, fill the sizes, and upload. Sizes must be on the lists in Employee dues › Setup. Blank cells keep the current value. Every change is kept in the employee's change history.",
		// HR Admin always has it (owner, 13 Nov 2026), whatever the role's rights say.
		allow: func(c *Ctx) bool { return c.Level("bulk.update") >= 2 && canEditSizes(c) || c.Role == "HR Admin" },
		lists: lists, row: bulkUni})
}

// bulkUni is one row of the Uniform sizes template: blank cells keep the current value.
func bulkUni(s *Server, ctx context.Context, tx store.Q, c *Ctx, r bulkRow, apply bool) (bulkResult, error) {
	var res bulkResult
	if _, ok := bulkEmployee(ctx, tx, c, r, &res); !ok {
		return res, nil
	}
	in := sizesIn{}
	for col, dst := range map[string]**string{"Shirt / T-shirt Size": &in.Shirt, "Trouser Waist": &in.Waist, "Trouser Length": &in.Length, "Shoe Size (UK)": &in.Shoe} {
		if v := strings.TrimSpace(r.V[col]); v != "" {
			*dst = &v
		}
	}
	ch, errs, err := saveSizes(ctx, tx, c, res.ID, in, "Bulk update")
	res.Changes, res.Errors = ch, errs
	return res, err
}

// bulkUniCurrent is the Uniform sizes download with the current data: active and pre-boarding employees in scope.
func bulkUniCurrent(ctx context.Context, q store.Q, c *Ctx, t *bulkTemplate) ([]string, [][]any, error) {
	emps, err := scopedEmps(ctx, q, c, bson.D{{Key: "status", Value: bson.D{{Key: "$ne", Value: "Inactive"}}}})
	if err != nil {
		return nil, nil, err
	}
	sizes, err := sizesOf(ctx, q, ids(emps))
	if err != nil {
		return nil, nil, err
	}
	out := [][]any{}
	for _, e := range emps {
		m := sizes[e.ID]
		out = append(out, []any{e.ID, m["shirt"], m["waist"], m["length"], m["shoe"], e.Name, e.Dept, e.Location, e.Status})
	}
	return append(append([]string{}, t.Cols...), "Name", "Department", "Location", "Status"), out, nil
}

// importSizes checks the optional size columns of an employee import (empty when none is given).
func importSizes(ctx context.Context, q store.Q, v map[string]string) (map[string]string, []string) {
	out, errs := map[string]string{}, []string{}
	var lists map[string][]string
	for _, k := range sizeKeys {
		x := strings.TrimSpace(v[k])
		if x == "" {
			continue
		}
		if lists == nil {
			lists = store.UniformSizeLists(ctx, q)
		}
		if m := sizeMatch(lists[k], x); m != "" {
			out[k] = m
		} else {
			errs = append(errs, fmt.Sprintf("%s “%s” is not on the list", sizeLabels[k], x))
		}
	}
	return out, errs
}

// sizesDoc is a uniform_size document for an imported employee, or nil when no size is given.
func sizesDoc(id string, m map[string]string, by string, now time.Time) bson.M {
	if len(m) == 0 {
		return nil
	}
	d := bson.M{"employee_id": id, "updated_at": now, "updated_by": by}
	for k, v := range m {
		d[k] = v
	}
	return d
}
