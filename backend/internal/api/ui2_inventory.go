package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/approval"
	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// Employee dues, round 2 (prototype vAst, vInv, vHold, vDed): uniform setup (recovery split, refund
// delay, items, kits), the uniform issues with their salary plan, counted write-offs, the store an
// employee belongs to, salary holds for people absent without notice, and waiving a deduction's
// balance. The issue, return and stock handlers in inventory.go use the helpers here.

// ---------- Helpers ----------

// moveDate checks an optional movement date (today or earlier) and returns it with the time the
// movement is recorded at: nil (now) for today, midday of the date in the business zone for an earlier day.
func moveDate(d string) (string, any, error) {
	d = strings.TrimSpace(d)
	if d == "" || d == today() {
		return today(), nil, nil
	}
	if !isDate(d) || d > today() {
		return "", nil, bad("The date must be today or earlier")
	}
	if d < policy.AddDays(today(), -366) {
		return "", nil, bad("The date must be within the last year")
	}
	at, _ := time.ParseInLocation("2006-01-02 15:04", d+" 12:00", store.Loc())
	return d, at, nil
}

func loadUniformSetting(ctx context.Context, q store.Q) uniformSetting {
	us := uniformSetting{FirstPct: 50, RefundAfter: 6}
	if r, err := q.C("company_setting").FindOne(ctx, bson.M{"_id": 1}, options.FindOne().SetProjection(bson.D{{Key: "uniform", Value: 1}})); err == nil && r != nil {
		if raw, err := json.Marshal(r["uniform"]); err == nil {
			_ = json.Unmarshal(raw, &us)
		}
	}
	if us.RefundBasis != refundByService {
		us.RefundBasis = refundByDeduction
	}
	if us.ServiceMonths <= 0 {
		us.ServiceMonths = 6
	}
	return us
}

// scheduleUniform writes a uniform issue's salary plan (spec 15.2, with Setup's split): firstPct% in the issue
// month (the open payroll month if that is later), the rest in the next month, and the full amount refunded in
// the month the refund rule gives (uniformPlan): refundAfter months after the last deduction, or once the employee
// (joined on doj) completes serviceMonths of employment. It returns the refund month.
func scheduleUniform(ctx context.Context, tx store.Q, empID, doj, id, issued string, amount float64) (string, error) {
	us := loadUniformSetting(ctx, tx)
	parts, refund := uniformPlan(us, maxYm(issued[:7], dedLineMonth(ctx, tx)), amount, doj)
	for i, p := range parts {
		label := "Uniform recovery"
		if len(parts) > 1 {
			label += fmt.Sprintf(" (%d of %d)", i+1, len(parts))
		}
		if err := addLedger(ctx, tx, empID, p.ym, "ded", label, p.amt, id, "Uniform issued "+longDate(issued)); err != nil {
			return "", err
		}
	}
	return refund, addLedger(ctx, tx, empID, refund, "earn", uniRefundLabel, amount, id, "Refund of uniform recovered earlier")
}

// storeForEmp is the store an employee is served from (prototype invStoreOf): an HRBP's own store
// for an HRBP, else the store of the employee's HRBP, else the central store.
func (s *Server) storeForEmp(ctx context.Context, q store.Q, c *Ctx, empID string) (string, error) {
	if c.Role == "HRBP" {
		l, err := s.storesFor(ctx, q, c)
		if err != nil || len(l) == 0 {
			return "", err
		}
		return l[0]["id"].(string), nil
	}
	sub, err := loadSubject(ctx, q, empID)
	if err != nil {
		return "", err
	}
	p := resolvePerson(ctx, q, "HRBP", sub, approval.Facts{})
	if p.ID == "" {
		return "central", nil
	}
	if ok, _ := q.C("employee").Exists(ctx, bson.M{"_id": p.ID, "status": bson.M{"$ne": "Inactive"}}); !ok {
		return "central", nil
	}
	if err := invEnsureStore(ctx, q, "hrbp:"+p.ID, p.Name+"'s store", p.ID); err != nil {
		return "", err
	}
	return "hrbp:" + p.ID, nil
}

// stockOf is an item's balance in a store from its movements.
func stockOf(ctx context.Context, q store.Q, storeID string, item int) int64 {
	moves, _ := q.C("inv_move").Find(ctx, bson.M{"store_id": storeID, "item_id": item}, options.Find().SetProjection(bson.D{{Key: "kind", Value: 1}, {Key: "qty", Value: 1}}))
	var have int64
	for _, m := range moves {
		if containsStr(invInbound, str(m["kind"])) {
			have += repInt64(m["qty"])
		} else {
			have -= repInt64(m["qty"])
		}
	}
	return have
}

// canInvSetup: the uniform and asset setup belongs to HR Admin (prototype: the Setup tab).
func canInvSetup(c *Ctx) error {
	if c.Level("emp.assets") >= 2 && c.Role == "HR Admin" {
		return nil
	}
	return forbidden("HR Admin changes the uniform and asset setup")
}

// uniformLine is one line of a uniform issue; ret is how many have come back.
type uniformLine struct {
	ItemID int     `json:"itemId"`
	Name   string  `json:"name"`
	Qty    int     `json:"qty"`
	Price  float64 `json:"price"`
	Ret    int     `json:"ret,omitempty"`
	Size   string  `json:"size,omitempty"` // the size issued (uniform.go); none on issues made before sizes
}

// returnUniform takes uniform back from an employee into a store, by item and quantity, from the
// oldest issue first (prototype invRetSave), or everything left of one issue (only). An issue whose
// every line has come back is marked returned. The recovery and refund already scheduled stay.
func returnUniform(ctx context.Context, tx store.Q, c *Ctx, emp, storeID string, want map[int]int, only, note string) (int, error) {
	filter := bson.M{"employee_id": emp, "returned": false}
	if only != "" {
		filter["_id"] = only
	}
	rows, err := tx.C("uniform_issue").LockAll(ctx, filter)
	if err != nil {
		return 0, err
	}
	// Oldest first: by the day issued, then when recorded, then id.
	sort.SliceStable(rows, func(i, j int) bool {
		if di, dj := invIssueDate(rows[i]), invIssueDate(rows[j]); di != dj {
			return di < dj
		}
		ai, _ := rows[i]["at"].(time.Time)
		aj, _ := rows[j]["at"].(time.Time)
		if !ai.Equal(aj) {
			return ai.Before(aj)
		}
		return str(rows[i]["id"]) < str(rows[j]["id"])
	})
	type issue struct {
		id    string
		lines []uniformLine
	}
	list := []issue{}
	held, names := map[int]int{}, map[int]string{}
	for _, r := range rows {
		x := issue{id: str(r["id"])}
		if raw, err := json.Marshal(r["lines"]); err == nil {
			_ = json.Unmarshal(raw, &x.lines)
		}
		for _, l := range x.lines {
			held[l.ItemID] += l.Qty - l.Ret
			names[l.ItemID] = l.Name
		}
		list = append(list, x)
	}
	if only != "" {
		if len(list) == 0 {
			return 0, bad("That uniform issue is already returned")
		}
		want = map[int]int{}
		for k, v := range held {
			if v > 0 {
				want[k] = v
			}
		}
	}
	for item, n := range want {
		if n < 0 {
			return 0, bad("Enter the quantities returned")
		}
		if n > held[item] {
			name := names[item]
			if name == "" {
				if it, _ := tx.C("inv_item").FindOne(ctx, bson.M{"_id": item}); it != nil {
					name = str(it["name"])
				}
			}
			return 0, bad("The employee holds only %d %s", held[item], name)
		}
	}
	units := 0
	for _, x := range list {
		changed, all := false, true
		for i := range x.lines {
			l := &x.lines[i]
			if t := min(l.Qty-l.Ret, want[l.ItemID]); t > 0 {
				l.Ret += t
				want[l.ItemID] -= t
				units += t
				changed = true
				if err := invMove(ctx, tx, bson.M{"store_id": storeID, "item_id": l.ItemID, "kind": "Return", "qty": t, "employee_id": emp, "ref": x.id, "note": note,
					"by_name": c.Name}, nil); err != nil {
					return 0, err
				}
			}
			all = all && l.Ret >= l.Qty
		}
		if changed {
			if _, err := tx.C("uniform_issue").Update(ctx, bson.M{"_id": x.id}, bson.M{"$set": bson.M{"lines": x.lines, "returned": all}}); err != nil {
				return 0, err
			}
		}
	}
	if units == 0 {
		return 0, bad("Choose what was returned")
	}
	return units, nil
}

// ---------- Uniform setup: recovery, items, kits ----------

func (s *Server) invSetupGet(r *http.Request, c *Ctx) (any, error) {
	if err := s.canInv(c, 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	us := loadUniformSetting(ctx, s.st.DB)
	items, err := s.st.DB.C("inv_item").Find(ctx, nil, options.Find().SetSort(bson.D{{Key: "kind", Value: -1}, {Key: "_id", Value: 1}}).
		SetProjection(bson.D{{Key: "kind", Value: 1}, {Key: "name", Value: 1}, {Key: "price", Value: 1}, {Key: "status", Value: 1}, {Key: "inactive_since", Value: 1},
			{Key: "inactive_reason", Value: 1}, {Key: "size_type", Value: 1}}))
	if err != nil {
		return nil, err
	}
	kits, err := s.st.DB.C("inv_kit").Find(ctx, nil, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).
		SetProjection(bson.D{{Key: "name", Value: 1}, {Key: "lines", Value: 1}, {Key: "status", Value: 1}}))
	if err != nil {
		return nil, err
	}
	return map[string]any{"firstPct": us.FirstPct, "refundAfter": us.RefundAfter, "refundBasis": us.RefundBasis, "serviceMonths": us.ServiceMonths,
		"rule": refundRuleText(us), "sizes": store.UniformSizeLists(ctx, s.st.DB), "items": items, "kits": kits, "canEdit": canInvSetup(c) == nil}, nil
}

func (s *Server) invSetupPut(r *http.Request, c *Ctx) (any, error) {
	if err := canInvSetup(c); err != nil {
		return nil, err
	}
	var b uniformSetting
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.FirstPct < 0 || b.FirstPct > 100 {
		return nil, bad("Recover between 0%% and 100%% in the issue month")
	}
	if b.RefundAfter != 6 && b.RefundAfter != 12 {
		return nil, bad("Refund after 6 or 12 months")
	}
	if b.RefundBasis != "" && b.RefundBasis != refundByDeduction && b.RefundBasis != refundByService {
		return nil, bad("Refund after the last deduction, or after months of employment")
	}
	if b.ServiceMonths < 0 || b.ServiceMonths > 60 {
		return nil, bad("Refund after 1 to 60 months of employment")
	}
	ctx := r.Context()
	return b, s.st.Tx(ctx, func(tx store.Q) error {
		before := loadUniformSetting(ctx, tx)
		// What was not sent stays: the refund basis and months (a caller from before them), and the size lists.
		if b.RefundBasis == "" {
			b.RefundBasis = before.RefundBasis
		}
		if b.ServiceMonths == 0 {
			b.ServiceMonths = before.ServiceMonths
		}
		b.Sizes = before.Sizes
		if _, err := tx.C("company_setting").Update(ctx, bson.M{"_id": 1}, bson.M{"$set": bson.M{"uniform": b, "updated_at": tx.Now()}}); err != nil {
			return err
		}
		before.Sizes, b.Sizes = nil, nil // the audit records the rule; size list changes have their own entry
		return audit(ctx, tx, c, "Uniform recovery changed", "company_setting", "1", before, b)
	})
}

// invNameTaken reports whether a document of coll matching filter already has this name in any letter case
// (lower(name)=lower($n)).
func invNameTaken(ctx context.Context, q store.Q, coll string, filter bson.M, name string) (bool, error) {
	rows, err := q.C(coll).Find(ctx, filter, options.Find().SetProjection(bson.D{{Key: "name", Value: 1}}))
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		if strings.ToLower(str(r["name"])) == strings.ToLower(name) {
			return true, nil
		}
	}
	return false, nil
}

// invOther narrows filter to the documents other than the one with this path id (id::text<>$n).
func invOther(filter bson.M, id string) bson.M {
	if n, ok := invNumericID(id); ok {
		filter["_id"] = bson.M{"$ne": n}
	}
	return filter
}

func (s *Server) invItemAdd(r *http.Request, c *Ctx) (any, error) {
	if err := canInvSetup(c); err != nil {
		return nil, err
	}
	var b struct {
		Kind     string  `json:"kind"`
		Name     string  `json:"name"`
		Price    float64 `json:"price"`
		SizeType string  `json:"sizeType"` // a uniform item: shirt, trouser, shoes or other (blank: from the name, else other)
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Name = strings.TrimSpace(b.Name)
	var sizeType any
	if b.Kind == "uniform" {
		if b.SizeType == "" {
			b.SizeType = store.GuessSizeType(b.Name)
		}
		st, err := cleanSizeType(b.SizeType)
		if err != nil {
			return nil, err
		}
		b.SizeType, sizeType = st, st
	} else {
		b.SizeType = ""
	}
	switch {
	case b.Kind != "uniform" && b.Kind != "asset":
		return nil, bad("An item is a uniform item or an asset type")
	case len(b.Name) < 2 || len(b.Name) > 60:
		return nil, bad("Enter the name")
	case b.Kind == "uniform" && b.Price <= 0:
		return nil, bad("Enter the name and price")
	case b.Price < 0:
		return nil, bad("The price cannot be negative")
	}
	if b.Kind == "asset" {
		b.Price = 0
	}
	ctx := r.Context()
	var id int
	err := s.st.Tx(ctx, func(tx store.Q) error {
		if ok, _ := invNameTaken(ctx, tx, "inv_item", bson.M{"kind": b.Kind}, b.Name); ok {
			return bad("%s is already on the list", b.Name)
		}
		v, err := tx.C("inv_item").Insert(ctx, bson.M{"kind": b.Kind, "name": b.Name, "price": b.Price, "size_type": sizeType})
		if err != nil {
			return err
		}
		id = int(repInt64(v))
		return audit(ctx, tx, c, "Inventory item added", "inv_item", itoa(id), nil, b)
	})
	return map[string]int{"id": id}, err
}

// invItemPatch changes an item's name, price or status. Items are made inactive, never deleted;
// the kind changes only while nothing has been stocked.
func (s *Server) invItemPatch(r *http.Request, c *Ctx) (any, error) {
	if err := canInvSetup(c); err != nil {
		return nil, err
	}
	var b struct {
		Name   *string  `json:"name"`
		Kind   *string  `json:"kind"`
		Price  *float64 `json:"price"`
		Status *string  `json:"status"`
		Reason string   `json:"reason"`
		// SizeType: a uniform item's size type (shirt, trouser, shoes, other).
		SizeType *string `json:"sizeType"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	n, numeric := invNumericID(id)
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		var cur store.Row
		if numeric {
			var err error
			if cur, err = tx.C("inv_item").Lock(ctx, bson.M{"_id": n}); err != nil {
				return err
			}
		}
		if cur == nil {
			return notFound("Item not found")
		}
		before := store.Row{"id": cur["id"], "kind": cur["kind"], "name": cur["name"], "price": cur["price"], "status": cur["status"]}
		kind, name, price, status := before["kind"].(string), before["name"].(string), before["price"].(float64), before["status"].(string)
		if b.Kind != nil && *b.Kind != kind {
			if *b.Kind != "uniform" && *b.Kind != "asset" {
				return bad("An item is a uniform item or an asset type")
			}
			moved, _ := tx.C("inv_move").Exists(ctx, bson.M{"item_id": n})
			owned, _ := tx.C("asset").Exists(ctx, bson.M{"item_id": n})
			if moved || owned {
				return bad("This item has stock on record, so its kind cannot change. Add a new item instead")
			}
			kind = *b.Kind
		}
		if b.Name != nil {
			name = strings.TrimSpace(*b.Name)
			if len(name) < 2 || len(name) > 60 {
				return bad("Enter the name")
			}
			if ok, _ := invNameTaken(ctx, tx, "inv_item", invOther(bson.M{"kind": kind}, id), name); ok {
				return bad("%s is already on the list", name)
			}
		}
		if b.Price != nil {
			if *b.Price < 0 || (kind == "uniform" && *b.Price == 0) {
				return bad("Enter a price above zero")
			}
			price = *b.Price
		}
		if kind == "asset" {
			price = 0
		} else if price <= 0 {
			return bad("Enter a price above zero")
		}
		since, reason := any(nil), any(nil)
		if b.Status != nil {
			if *b.Status != "Active" && *b.Status != "Inactive" {
				return bad("Status is Active or Inactive")
			}
			status = *b.Status
		}
		if status == "Inactive" {
			since, reason = today(), strings.TrimSpace(b.Reason)
			if before["status"] == "Inactive" { // already inactive: since and why stay
				since, reason = cur["inactive_since"], cur["inactive_reason"]
			}
		}
		set := bson.M{"kind": kind, "name": name, "price": price, "status": status, "inactive_since": since, "inactive_reason": reason}
		after := map[string]any{"kind": kind, "name": name, "price": price, "status": status}
		if b.SizeType != nil || (kind != "uniform" && cur["size_type"] != nil) {
			st := any(nil)
			if kind == "uniform" {
				v, err := cleanSizeType(*b.SizeType)
				if err != nil {
					return err
				}
				st = v
			}
			if st != cur["size_type"] {
				set["size_type"], after["sizeType"], before["sizeType"] = st, st, cur["size_type"]
			}
		}
		if _, err := tx.C("inv_item").Update(ctx, bson.M{"_id": n}, bson.M{"$set": set}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Inventory item changed", "inv_item", id, before, after)
	})
}

type kitBody struct {
	Name   string         `json:"name"`
	Lines  map[string]int `json:"lines"`
	Status string         `json:"status"`
}

// cleanKit checks a kit: a name, and uniform items with whole quantities (zeros are dropped).
func cleanKit(ctx context.Context, q store.Q, b *kitBody) error {
	b.Name = strings.TrimSpace(b.Name)
	if len(b.Name) < 2 || len(b.Name) > 60 {
		return bad("Name the kit")
	}
	out := map[string]int{}
	for k, n := range b.Lines {
		if n < 0 || n > 99 {
			return bad("Kit quantities are 0 to 99")
		}
		if n == 0 {
			continue
		}
		item, ok := invNumericID(k)
		if ok {
			ok, _ = q.C("inv_item").Exists(ctx, bson.M{"_id": item, "kind": "uniform"})
		}
		if !ok {
			return bad("A kit holds uniform items only")
		}
		out[k] = n
	}
	b.Lines = out
	if b.Status == "" {
		b.Status = "Active"
	}
	if b.Status != "Active" && b.Status != "Inactive" {
		return bad("Status is Active or Inactive")
	}
	return nil
}

func (s *Server) invKits(r *http.Request, c *Ctx) (any, error) {
	if err := s.canInv(c, 1); err != nil {
		return nil, err
	}
	return s.st.DB.C("inv_kit").Find(r.Context(), bson.M{"status": "Active"}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).
		SetProjection(bson.D{{Key: "name", Value: 1}, {Key: "lines", Value: 1}}))
}

func (s *Server) invKitAdd(r *http.Request, c *Ctx) (any, error) {
	if err := canInvSetup(c); err != nil {
		return nil, err
	}
	var b kitBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	var id int
	err := s.st.Tx(ctx, func(tx store.Q) error {
		if err := cleanKit(ctx, tx, &b); err != nil {
			return err
		}
		if ok, _ := invNameTaken(ctx, tx, "inv_kit", bson.M{}, b.Name); ok {
			return bad("A kit called %s already exists", b.Name)
		}
		v, err := tx.C("inv_kit").Insert(ctx, bson.M{"name": b.Name, "lines": b.Lines, "status": b.Status, "by_name": c.Name})
		if err != nil {
			return err
		}
		id = int(repInt64(v))
		return audit(ctx, tx, c, "Uniform kit added", "inv_kit", itoa(id), nil, b)
	})
	return map[string]int{"id": id}, err
}

func (s *Server) invKitPut(r *http.Request, c *Ctx) (any, error) {
	if err := canInvSetup(c); err != nil {
		return nil, err
	}
	var b kitBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	n, numeric := invNumericID(id)
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		var cur store.Row
		if numeric {
			var err error
			if cur, err = tx.C("inv_kit").Lock(ctx, bson.M{"_id": n}); err != nil {
				return err
			}
		}
		if cur == nil {
			return notFound("Kit not found")
		}
		before := store.Row{"id": cur["id"], "name": cur["name"], "lines": cur["lines"], "status": cur["status"]}
		if err := cleanKit(ctx, tx, &b); err != nil {
			return err
		}
		if ok, _ := invNameTaken(ctx, tx, "inv_kit", invOther(bson.M{}, id), b.Name); ok {
			return bad("A kit called %s already exists", b.Name)
		}
		if _, err := tx.C("inv_kit").Update(ctx, bson.M{"_id": n}, bson.M{"$set": bson.M{"name": b.Name, "lines": b.Lines, "status": b.Status, "by_name": c.Name,
			"at": tx.Now()}}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Uniform kit changed", "inv_kit", id, before, b)
	})
}

// ---------- Uniform issues with their salary plan ----------

// uniformIssues lists uniform issued to employees in scope (newest first, up to 500) with the recovery
// schedule and the refund month and state from the pay lines (prototype UNI.issues; uniformIssueRows).
func (s *Server) uniformIssues(r *http.Request, c *Ctx) (any, error) {
	if err := s.canInv(c, 1); err != nil {
		return nil, err
	}
	ctx, q := r.Context(), s.st.DB
	scope, err := c.ScopeFilter(ctx, q, "")
	if err != nil {
		return nil, err
	}
	emps, err := q.C("employee").Find(ctx, scope, options.Find().SetProjection(bson.D{{Key: "name", Value: 1}, {Key: "department_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	depts, err := repByID(ctx, q, "department", repCol(emps, "department_id"))
	if err != nil {
		return nil, err
	}
	only := r.URL.Query().Get("employeeId")
	names, ids := map[string]any{}, []string{}
	for _, e := range emps {
		if id := str(e["id"]); depts[str(e["department_id"])] != nil && (only == "" || id == only) {
			names[id], ids = e["name"], append(ids, id)
		}
	}
	issues, err := q.C("uniform_issue").Find(ctx, bson.D{{Key: "employee_id", Value: repIn(ids)}})
	if err != nil {
		return nil, err
	}
	return uniformIssueRows(ctx, q, issues, names)
}

// ---------- Counted write-off ----------

// invWriteOff writes off stock in a store: counted items by quantity, assets by serial or the
// first available ones (marked Damaged). An HRBP writes off in their own store; HR Admin and HRBP
// Corporate in any store.
func (s *Server) invWriteOff(r *http.Request, c *Ctx) (any, error) {
	if err := s.canInv(c, 2); err != nil {
		return nil, err
	}
	if c.Role != "HRBP" && c.Role != "HR Admin" && c.Role != "HRBP Corporate" {
		return nil, forbidden("An HRBP writes off stock in their store; HRBP Corporate in any store")
	}
	var b struct {
		Store string    `json:"store"`
		Lines []invLine `json:"lines"`
		Note  string    `json:"note"`
		Date  string    `json:"date"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Note = strings.TrimSpace(b.Note); len(b.Note) < 3 {
		return nil, bad("Enter the quantity and a reason")
	}
	_, at, err := moveDate(b.Date)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	units := 0
	err = s.st.Tx(ctx, func(tx store.Q) error {
		units = 0
		if err := s.storeAllowed(ctx, tx, c, b.Store); err != nil {
			return err
		}
		sname := ""
		if st, _ := tx.C("inv_store").FindOne(ctx, bson.M{"_id": b.Store}); st != nil {
			sname = str(st["name"])
		}
		for _, l := range b.Lines {
			serials := cleanSerials(l.Serials)
			if l.Qty <= 0 && len(serials) == 0 {
				continue
			}
			it, err := tx.C("inv_item").FindOne(ctx, bson.M{"_id": l.ItemID})
			if err != nil || it == nil {
				return bad("Unknown item")
			}
			kind, name := str(it["kind"]), str(it["name"])
			if kind == "asset" {
				if len(serials) == 0 {
					rows, err := tx.C("asset").Find(ctx, bson.M{"item_id": l.ItemID, "store_id": b.Store, "status": "Available"},
						options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).SetLimit(int64(l.Qty)).SetProjection(bson.D{{Key: "serial", Value: 1}}))
					if err != nil {
						return err
					}
					if len(rows) < l.Qty {
						return bad("%s has only %d %s available", sname, len(rows), name)
					}
					for _, x := range rows {
						serials = append(serials, x["serial"].(string))
					}
				}
				for _, sn := range serials {
					hit, err := tx.C("asset").Update(ctx, bson.M{"serial": sn, "item_id": l.ItemID, "store_id": b.Store, "status": "Available"},
						bson.M{"$set": bson.M{"status": "Damaged", "note": b.Note}})
					if err != nil {
						return err
					}
					if hit == 0 {
						return bad("%s %s is not available in %s", name, sn, sname)
					}
				}
				l.Qty = len(serials)
			} else if have := stockOf(ctx, tx, b.Store, l.ItemID); int64(l.Qty) > have {
				return bad("%s has only %d %s", sname, have, name)
			}
			if err := invMove(ctx, tx, bson.M{"store_id": b.Store, "item_id": l.ItemID, "kind": "Write-off", "qty": l.Qty, "serials": serials, "note": b.Note,
				"by_name": c.Name}, at); err != nil {
				return err
			}
			units += l.Qty
		}
		if units == 0 {
			return bad("Enter the quantity and a reason")
		}
		return audit(ctx, tx, c, "Stock written off", "inv_store", b.Store, nil, b)
	})
	return map[string]int{"units": units}, err
}

// ---------- Salary holds: not at work without notice ----------

// holdsAbsent lists active employees in scope, without a hold, who had no punch, no leave and no
// regularization on at least min(days, 5) of the last `days` days (yesterday back; prototype
// absentDays). Week offs and holidays do not count.
func (s *Server) holdsAbsent(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.claims", 1); err != nil {
		return nil, err
	}
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days == 0 {
		days = 10
	}
	if days < 1 || days > 31 {
		return nil, bad("Look back 1 to 31 days")
	}
	ctx := r.Context()
	held, err := s.st.DB.C("salary_hold").Distinct(ctx, "employee_id", bson.M{"status": "Active"})
	if err != nil {
		return nil, err
	}
	emps, err := scopedEmps(ctx, s.st.DB, c, bson.M{"status": "Active", "_id": bson.M{"$nin": held}})
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	if len(emps) == 0 {
		return out, nil
	}
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	to := policy.AddDays(today(), -1)
	from := policy.AddDays(to, -(days - 1))
	m, err := loadMonth(ctx, s.st.DB, ts, ids(emps), from, to)
	if err != nil {
		return nil, err
	}
	need := min(days, 5)
	for _, e := range emps {
		p, _ := ts.policyFor(e)
		n := 0
		for d := from; d <= to; d = policy.AddDays(d, 1) {
			if d < e.DOJ || m.leaveOn(e, d, true) != nil || m.regs[e.ID][d] != "" {
				continue
			}
			if st, _ := m.status(e, p, d); st == "No punch" || strings.HasPrefix(st, "Absent") {
				n++
			}
		}
		if n >= need {
			out = append(out, map[string]any{"employeeId": e.ID, "name": e.Name, "department": e.Dept, "location": e.Location, "days": n})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i]["days"].(int) > out[j]["days"].(int) })
	return out, nil
}

// ---------- Deductions: waive the balance ----------

// deductionWaive voids the instalments of an approved deduction not yet deducted (the open month
// on) and records who waived how much and why. Instalments already deducted stay.
func (s *Server) deductionWaive(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.claims", 2); err != nil {
		return nil, err
	}
	var b struct {
		Note string `json:"note"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Note = strings.TrimSpace(b.Note); len(b.Note) < 3 {
		return nil, bad("Enter the reason")
	}
	ctx, id := r.Context(), r.PathValue("id")
	var total float64
	err := s.st.Tx(ctx, func(tx store.Q) error {
		total = 0
		req, err := loadRequest(ctx, tx, id, true)
		if store.Transient(err) {
			return err
		}
		if err != nil || req.WF != "p_ded" {
			return notFound("Deduction not found")
		}
		if req.Status != "Approved" {
			return bad("Only an approved deduction has a balance to waive")
		}
		if err := c.MustInScope(ctx, tx, req.EmployeeID); err != nil {
			return err
		}
		if req.EmployeeID == c.EmployeeID {
			return forbidden("You cannot waive a deduction from your own salary")
		}
		left, err := tx.C("ledger").LockAll(ctx, bson.M{"source_id": id, "kind": "ded", "void": false, "ym": bson.M{"$gte": openYm(ctx, tx)}},
			options.Find().SetSort(bson.D{{Key: "ym", Value: 1}, {Key: "_id", Value: 1}}))
		if err != nil {
			return err
		}
		if len(left) == 0 {
			return bad("Nothing is left to deduct")
		}
		why := "Waived by " + c.Name + ": " + b.Note
		for _, l := range left {
			total += l["amount"].(float64)
			if _, err := tx.C("ledger").Update(ctx, bson.M{"_id": l["id"]}, bson.M{"$set": bson.M{"void": true, "void_reason": why, "voided_at": tx.Now()}}); err != nil {
				return err
			}
		}
		total = calc.Round2(total)
		// One waiver record per deduction: a later waiver adds to the amount and takes the latest note.
		amount := total
		old, err := tx.C("deduction_waiver").FindOne(ctx, bson.M{"request_id": id})
		if err != nil {
			return err
		}
		if old != nil {
			amount += old["amount"].(float64)
		}
		if _, err := tx.C("deduction_waiver").Upsert(ctx, bson.M{"request_id": id}, bson.M{"amount": amount, "note": b.Note, "by_name": c.Name, "at": tx.Now()}, nil); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Deduction balance waived", "approval_request", id, nil, map[string]any{"amount": total, "note": b.Note})
	})
	return map[string]any{"waived": total, "message": fmt.Sprintf("Balance of %s waived.", calc.Inr(total))}, err
}

// ---------- Deduction proof (documents of owner kind 'approval') ----------

// canApprovalFile: owner is "<employee id>/<key>". Upload by whoever raises deductions (pay.claims
// or emp.assets at 2), never for themselves; view by pay and dues viewers and approvers. Both
// within the caller's data scope.
func (s *Server) canApprovalFile(ctx context.Context, c *Ctx, owner string, write bool) error {
	emp, key, _ := strings.Cut(owner, "/")
	if emp == "" || len(owner) > 80 || (write && key == "") {
		return bad("Say which employee the proof is for")
	}
	if write {
		if c.Level("pay.claims") < 2 && c.Level("emp.assets") < 2 {
			return forbidden("")
		}
		if emp == c.EmployeeID {
			return forbidden("Raise this for another employee; your own goes through your HR")
		}
	} else if s.canDues(c, 1) != nil && c.Level("pay.run") < 1 && c.Level("appr.queue") < 1 {
		return forbidden("")
	}
	return c.MustInScope(ctx, s.st.DB, emp)
}

// ---------- Payslip setup: company email and phone ----------

// cleanSlipContact checks the email and phone shown in the payslip header.
func cleanSlipContact(p *payslipSetup) error {
	p.Email, p.Phone = strings.TrimSpace(p.Email), strings.TrimSpace(p.Phone)
	if p.Email != "" {
		if a, err := mail.ParseAddress(p.Email); err != nil || a.Address != p.Email {
			return bad("The email looks wrong")
		}
	}
	if len(p.Phone) > 30 {
		return bad("Keep the phone under 30 characters")
	}
	return nil
}
