package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

// Inventory, assets and uniform (spec 19.1). A central store and one store per HRBP. HRBP
// Corporate adds stock centrally and transfers it to HRBPs; an HRBP issues to employees and takes
// returns. Uniform for permanent staff is recovered 50% in the issue month and 50% the next, and
// refunded in full 6 months after the last deduction; a return does not reverse it. Assets are
// serial numbered and never deleted.

var moveSign = map[string]int{"Addition": 1, "Transfer in": 1, "Return": 1, "Issue": -1, "Transfer out": -1, "Write-off": -1}

// invInbound are the movements that bring stock into a store; every other one takes it out.
var invInbound = []string{"Addition", "Transfer in", "Return"}

func (s *Server) canInv(c *Ctx, min int) error {
	if c.Level("emp.assets") >= min {
		return nil
	}
	return forbidden("")
}

// storesFor: the central store for HR Admin and HRBP Corporate, every HRBP store for them, and
// an HRBP's own store (made the first time it is needed).
func (s *Server) storesFor(ctx context.Context, q store.Q, c *Ctx) ([]store.Row, error) {
	if c.Role == "HRBP" {
		id := "hrbp:" + c.EmployeeID
		if err := invEnsureStore(ctx, q, id, c.Name+"'s store", c.EmployeeID); err != nil {
			return nil, err
		}
		return q.C("inv_store").Find(ctx, bson.M{"_id": id})
	}
	// Every mapped HRBP gets a store.
	maps, err := q.C("user_role_map").Find(ctx, bson.M{"role_name": "HRBP", "active": true}, options.Find().SetProjection(bson.D{{Key: "employee_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	names, err := repNames(ctx, q, repCol(maps, "employee_id"))
	if err != nil {
		return nil, err
	}
	have, err := q.C("inv_store").Distinct(ctx, "_id", nil)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, id := range have {
		known[str(id)] = true
	}
	for _, m := range maps {
		emp := str(m["employee_id"])
		name, ok := names[emp]
		if id := "hrbp:" + emp; ok && !known[id] {
			if err := invEnsureStore(ctx, q, id, name+"'s store", emp); err != nil {
				return nil, err
			}
			known[id] = true
		}
	}
	list, err := q.C("inv_store").Find(ctx, nil)
	if err != nil {
		return nil, err
	}
	// The central store first, then by name.
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if ca, cb := a["id"] == "central", b["id"] == "central"; ca != cb {
			return ca
		}
		if str(a["name"]) != str(b["name"]) {
			return str(a["name"]) < str(b["name"])
		}
		return str(a["id"]) < str(b["id"])
	})
	return list, nil
}

// invEnsureStore makes a store unless it exists.
func invEnsureStore(ctx context.Context, q store.Q, id, name, owner string) error {
	_, err := q.C("inv_store").Upsert(ctx, bson.M{"_id": id}, nil, bson.M{"name": name, "owner_id": owner})
	return err
}

func (s *Server) storeAllowed(ctx context.Context, q store.Q, c *Ctx, id string) error {
	list, err := s.storesFor(ctx, q, c)
	if err != nil {
		return err
	}
	for _, x := range list {
		if x["id"] == id {
			return nil
		}
	}
	return forbidden("That store is not yours")
}

// invNumericID reads a numeric id from a path, as id::text=$1 matched it: only its plain decimal form.
func invNumericID(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && strconv.FormatInt(n, 10) == s
}

// invAvailable counts the assets available in a store, by item.
func invAvailable(ctx context.Context, q store.Q, storeID string) (map[int64]int64, error) {
	rows, err := q.C("asset").Find(ctx, bson.M{"store_id": storeID, "status": "Available"}, options.Find().SetProjection(bson.D{{Key: "item_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	out := map[int64]int64{}
	for _, a := range rows {
		out[repInt64(a["item_id"])]++
	}
	return out, nil
}

// invMove records a stock movement; at is when it happened (nil: now).
func invMove(ctx context.Context, tx store.Q, doc bson.M, at any) error {
	if at != nil {
		doc["at"] = at
	}
	_, err := tx.C("inv_move").Insert(ctx, doc)
	return err
}

// invSummary: per store and item, opening for the month, additions, issued, returns, closing,
// and assets on hand (closing matched with serials).
func (s *Server) invSummary(ctx context.Context, q store.Q, storeID, ym string) ([]map[string]any, error) {
	first := repDayStart(ym + "-01")
	items, err := q.C("inv_item").Find(ctx, bson.M{"status": "Active"}, options.Find().SetSort(bson.D{{Key: "kind", Value: -1}, {Key: "name", Value: 1}}).
		SetProjection(bson.D{{Key: "kind", Value: 1}, {Key: "name", Value: 1}, {Key: "price", Value: 1}}))
	if err != nil {
		return nil, err
	}
	moves, err := q.C("inv_move").Find(ctx, bson.M{"store_id": storeID}, options.Find().SetProjection(bson.D{{Key: "item_id", Value: 1}, {Key: "kind", Value: 1},
		{Key: "qty", Value: 1}, {Key: "at", Value: 1}}))
	if err != nil {
		return nil, err
	}
	type tally struct{ opening, additions, issued, returns, out int64 }
	by := map[int64]*tally{}
	for _, m := range moves {
		id := repInt64(m["item_id"])
		t := by[id]
		if t == nil {
			t = &tally{}
			by[id] = t
		}
		qty, kind := repInt64(m["qty"]), str(m["kind"])
		if at, _ := m["at"].(time.Time); at.Before(first) {
			if containsStr(invInbound, kind) {
				t.opening += qty
			} else {
				t.opening -= qty
			}
			continue
		}
		switch kind {
		case "Addition", "Transfer in":
			t.additions += qty
		case "Issue":
			t.issued += qty
		case "Return":
			t.returns += qty
		case "Transfer out", "Write-off":
			t.out += qty
		}
	}
	hand, err := invAvailable(ctx, q, storeID)
	if err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	for _, it := range items {
		id := repInt64(it["id"])
		t := by[id]
		if t == nil {
			t = &tally{}
		}
		rows = append(rows, map[string]any{"id": it["id"], "kind": it["kind"], "name": it["name"], "price": it["price"], "opening": t.opening, "additions": t.additions,
			"issued": t.issued, "returns": t.returns, "out": t.out, "on_hand": hand[id], "closing": t.opening + t.additions + t.returns - t.issued - t.out})
	}
	return rows, nil
}

func (s *Server) invGet(r *http.Request, c *Ctx) (any, error) {
	if err := s.canInv(c, 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	stores, err := s.storesFor(ctx, s.st.DB, c)
	if err != nil {
		return nil, err
	}
	ym := r.URL.Query().Get("ym")
	if !isYm(ym) {
		ym = today()[:7]
	}
	sid := r.URL.Query().Get("store")
	if sid == "" && len(stores) > 0 {
		sid = stores[0]["id"].(string)
	}
	if err := s.storeAllowed(ctx, s.st.DB, c, sid); err != nil {
		return nil, err
	}
	sum, err := s.invSummary(ctx, s.st.DB, sid, ym)
	if err != nil {
		return nil, err
	}
	items, _ := s.st.DB.C("inv_item").Find(ctx, bson.M{"status": "Active"}, options.Find().SetSort(bson.D{{Key: "kind", Value: -1}, {Key: "name", Value: 1}}).
		SetProjection(bson.D{{Key: "kind", Value: 1}, {Key: "name", Value: 1}, {Key: "price", Value: 1}}))
	assets, _ := invStoreAssets(ctx, s.st.DB, sid)
	moves, _ := invStoreMoves(ctx, s.st.DB, sid)
	return map[string]any{"stores": stores, "store": sid, "ym": ym, "summary": sum, "items": items, "assets": assets, "moves": moves,
		"canCentral": c.Role == "HR Admin" || c.Role == "HRBP Corporate", "canEdit": c.Level("emp.assets") >= 2, "canAddAsset": canAddAssets(c)}, nil
}

// invItems loads inventory items by id.
func invItems(ctx context.Context, q store.Q, ids []any) (map[int64]store.Row, error) {
	out := map[int64]store.Row{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.C("inv_item").Find(ctx, bson.M{"_id": bson.M{"$in": ids}})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[repInt64(r["id"])] = r
	}
	return out, nil
}

// invStoreAssets lists a store's assets by type and serial, with whom each is issued to.
func invStoreAssets(ctx context.Context, q store.Q, sid string) ([]store.Row, error) {
	rows, err := q.C("asset").Find(ctx, bson.M{"store_id": sid})
	if err != nil {
		return nil, err
	}
	items, err := invItems(ctx, q, invColAny(rows, "item_id"))
	if err != nil {
		return nil, err
	}
	names, err := repNames(ctx, q, repCol(rows, "employee_id"))
	if err != nil {
		return nil, err
	}
	out := []store.Row{}
	for _, a := range rows {
		it := items[repInt64(a["item_id"])]
		if it == nil {
			continue
		}
		var who any
		if n, ok := names[str(a["employee_id"])]; ok {
			who = n
		}
		out = append(out, store.Row{"id": a["id"], "type": it["name"], "name": a["name"], "serial": a["serial"], "status": a["status"], "employee_id": a["employee_id"],
			"employee": who, "issued_at": a["issued_at"], "note": a["note"]})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if str(out[i]["type"]) != str(out[j]["type"]) {
			return str(out[i]["type"]) < str(out[j]["type"])
		}
		return str(out[i]["serial"]) < str(out[j]["serial"])
	})
	return out, nil
}

// invStoreMoves lists a store's last 100 movements, newest first.
func invStoreMoves(ctx context.Context, q store.Q, sid string) ([]store.Row, error) {
	rows, err := q.C("inv_move").Find(ctx, bson.M{"store_id": sid}, options.Find().SetSort(bson.D{{Key: "_id", Value: -1}}).SetLimit(100))
	if err != nil {
		return nil, err
	}
	items, err := invItems(ctx, q, invColAny(rows, "item_id"))
	if err != nil {
		return nil, err
	}
	names, err := repNames(ctx, q, repCol(rows, "employee_id"))
	if err != nil {
		return nil, err
	}
	out := []store.Row{}
	for _, m := range rows {
		it := items[repInt64(m["item_id"])]
		if it == nil {
			continue
		}
		var who any
		if n, ok := names[str(m["employee_id"])]; ok {
			who = n
		}
		at, _ := m["at"].(time.Time)
		at = at.In(store.Loc())
		out = append(out, store.Row{"id": m["id"], "kind": m["kind"], "item": it["name"], "qty": m["qty"], "employee_id": m["employee_id"], "employee": who,
			"serials": m["serials"], "note": m["note"], "by_name": m["by_name"], "at": at.Format("02 Jan 2006"), "date": at.Format("2006-01-02"), "ref": m["ref"]})
	}
	return out, nil
}

// invColAny is the non-null values of field k in rows.
func invColAny(rows []store.Row, k string) []any {
	out := []any{}
	for _, r := range rows {
		if v := r[k]; v != nil {
			out = append(out, v)
		}
	}
	return out
}

type invLine struct {
	ItemID  int      `json:"itemId"`
	Qty     int      `json:"qty"`
	Serials []string `json:"serials"`
	Name    string   `json:"name"` // adding assets: the name or model of these serials
	Size    string   `json:"size"` // issuing uniform: the size given (uniform.go)
}

func cleanSerials(l []string) []string {
	out := []string{}
	for _, s := range l {
		if s = strings.ToUpper(strings.TrimSpace(s)); s != "" && !containsStr(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// invAdd adds stock to the central store (HRBP Corporate, HR Admin) or transfers it to an HRBP store.
func (s *Server) invAddOrTransfer(r *http.Request, c *Ctx) (any, error) {
	if err := s.canInv(c, 2); err != nil {
		return nil, err
	}
	// Others with dues edit rights (Payroll, Payroll Manager; not an HRBP) add assets to the central store only.
	assetsOnly := c.Role != "HR Admin" && c.Role != "HRBP Corporate"
	if assetsOnly && (r.PathValue("kind") == "transfer" || !canAddAssets(c)) {
		return nil, forbidden("HRBP Corporate adds stock and transfers it to HRBPs")
	}
	var b struct {
		To    string    `json:"to"`
		From  string    `json:"from"`
		Lines []invLine `json:"lines"`
		Note  string    `json:"note"`
		Date  string    `json:"date"` // optional: the day received or sent, today or earlier
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	_, at, derr := moveDate(b.Date)
	if derr != nil {
		return nil, derr
	}
	transfer := r.PathValue("kind") == "transfer"
	if !transfer {
		b.From = ""
	}
	if assetsOnly && b.To != "central" {
		return nil, forbidden("Assets are added to the central store")
	}
	ctx := r.Context()
	n := 0
	err := s.st.Tx(ctx, func(tx store.Q) error {
		n = 0
		if err := s.storeAllowed(ctx, tx, c, b.To); err != nil {
			return err
		}
		if transfer {
			if err := s.storeAllowed(ctx, tx, c, b.From); err != nil {
				return err
			}
			if b.From == b.To {
				return bad("Choose two different stores")
			}
		}
		ref := fmt.Sprintf("%s:%d", map[bool]string{true: "TRF", false: "ADD"}[transfer], len(b.Lines))
		for _, l := range b.Lines {
			if l.Qty <= 0 && len(l.Serials) == 0 {
				continue
			}
			it, err := tx.C("inv_item").FindOne(ctx, bson.M{"_id": l.ItemID, "status": "Active"})
			if err != nil || it == nil {
				return bad("Unknown item")
			}
			kind, name := str(it["kind"]), str(it["name"])
			if assetsOnly && kind != "asset" {
				return forbidden("HRBP Corporate adds uniform stock to the central store")
			}
			serials := cleanSerials(l.Serials)
			if kind == "asset" {
				if len(serials) == 0 {
					return bad("Enter the serial numbers for %s", name)
				}
				l.Qty = len(serials)
				for _, sn := range serials {
					if transfer {
						moved, err := tx.C("asset").Update(ctx, bson.M{"serial": sn, "store_id": b.From, "status": "Available"}, bson.M{"$set": bson.M{"store_id": b.To}})
						if err != nil {
							return err
						}
						if moved == 0 {
							return bad("%s %s is not available in the sending store", name, sn)
						}
						continue
					}
					// Serial numbers are unique: a known one is refused before writing (a failed write would end the transaction).
					if taken, err := tx.C("asset").Exists(ctx, bson.M{"serial": sn}); err != nil || taken {
						return bad("Serial %s already exists", sn)
					}
					if _, err := tx.C("asset").Insert(ctx, bson.M{"item_id": l.ItemID, "serial": sn, "store_id": b.To, "name": strings.TrimSpace(l.Name)}); err != nil {
						return bad("Serial %s already exists", sn)
					}
				}
			}
			if transfer {
				if have := stockOf(ctx, tx, b.From, l.ItemID); int64(l.Qty) > have {
					return bad("Only %d %s in the sending store", have, name)
				}
				if err := invMove(ctx, tx, bson.M{"store_id": b.From, "item_id": l.ItemID, "kind": "Transfer out", "qty": l.Qty, "ref": ref, "serials": serials,
					"note": b.Note, "by_name": c.Name}, at); err != nil {
					return err
				}
			}
			kindIn := map[bool]string{true: "Transfer in", false: "Addition"}[transfer]
			if err := invMove(ctx, tx, bson.M{"store_id": b.To, "item_id": l.ItemID, "kind": kindIn, "qty": l.Qty, "ref": ref, "serials": serials, "note": b.Note,
				"by_name": c.Name}, at); err != nil {
				return err
			}
			n++
		}
		if n == 0 {
			return bad("Enter at least one quantity")
		}
		return audit(ctx, tx, c, map[bool]string{true: "Stock transferred", false: "Stock added"}[transfer], "inv_store", b.To, nil, b)
	})
	return map[string]int{"lines": n}, err
}

type uniformSetting struct {
	FirstPct    float64 `json:"firstPct"`
	RefundAfter int     `json:"refundAfter"`
	// RefundBasis: "deduction" (RefundAfter months after the last deduction, the default) or "service" (after
	// ServiceMonths months of employment, uniform.go).
	RefundBasis   string `json:"refundBasis,omitempty"`
	ServiceMonths int    `json:"serviceMonths,omitempty"`
	// Sizes: the size lists HR Admin keeps (store.UniformSizeLists reads them with their defaults).
	Sizes map[string][]string `json:"sizes,omitempty"`
}

// invIssue issues uniform (with recovery for permanent staff) or an asset to an employee. Without a
// store it comes from the employee's store (an asset: the store holding it); the date is optional.
func (s *Server) invIssue(r *http.Request, c *Ctx) (any, error) {
	if err := s.canInv(c, 2); err != nil {
		return nil, err
	}
	var b struct {
		Store      string    `json:"store"`
		EmployeeID string    `json:"employeeId"`
		Lines      []invLine `json:"lines"`
		AssetID    int64     `json:"assetId"`
		Recover    *bool     `json:"recover"`
		Date       string    `json:"date"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	date, at, err := moveDate(b.Date)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	if err := c.MustInScope(ctx, s.st.DB, b.EmployeeID); err != nil {
		return nil, err
	}
	var out map[string]any
	err = s.st.Tx(ctx, func(tx store.Q) error {
		out = nil
		storeID := b.Store
		e, err := loadEmp(ctx, tx, b.EmployeeID)
		if err != nil {
			return err
		}
		if e.Status != "Active" {
			return bad("%s is not active", e.Name)
		}
		if date < e.DOJ {
			return bad("%s joined on %s. Choose an issue date from then", e.Name, longDate(e.DOJ))
		}
		if storeID == "" && b.AssetID > 0 {
			if a, _ := tx.C("asset").FindOne(ctx, bson.M{"_id": b.AssetID}); a != nil {
				storeID = str(a["store_id"])
			}
		}
		if storeID == "" {
			if storeID, err = s.storeForEmp(ctx, tx, c, e.ID); err != nil {
				return err
			}
		}
		if err := s.storeAllowed(ctx, tx, c, storeID); err != nil {
			if b.AssetID > 0 && c.Role == "HRBP" {
				return forbidden("This asset is not in your store. Ask HRBP Corporate to transfer it")
			}
			return err
		}
		if b.AssetID > 0 {
			a, err := tx.C("asset").FindOneAndUpdate(ctx, bson.M{"_id": b.AssetID, "store_id": storeID, "status": "Available"},
				bson.M{"$set": bson.M{"status": "Issued", "employee_id": e.ID, "issued_at": date}})
			if store.Transient(err) {
				return err // a concurrent write to the asset: Tx runs this again once it commits (the UPDATE waited)
			}
			if err != nil || a == nil {
				return bad("That asset is not available in this store")
			}
			serial := str(a["serial"])
			if err := invMove(ctx, tx, bson.M{"store_id": storeID, "item_id": a["item_id"], "kind": "Issue", "qty": 1, "employee_id": e.ID, "serials": []string{serial},
				"by_name": c.Name}, at); err != nil {
				return err
			}
			out = map[string]any{"asset": serial, "store": storeID}
			return audit(ctx, tx, c, "Asset issued", "asset", serial, nil, e.ID)
		}
		sname := ""
		if st, _ := tx.C("inv_store").FindOne(ctx, bson.M{"_id": storeID}); st != nil {
			sname = str(st["name"])
			if storeID == "central" {
				sname = "The central store"
			}
		}
		// One line per item and size; the stock is checked per item.
		type key struct {
			item int
			size string
		}
		qty, order, total, items := map[key]int{}, []key{}, map[int]int{}, map[int]store.Row{}
		lists := store.UniformSizeLists(ctx, tx)
		for _, l := range b.Lines {
			if l.Qty <= 0 {
				continue
			}
			it := items[l.ItemID]
			if it == nil {
				if it, err = tx.C("inv_item").FindOne(ctx, bson.M{"_id": l.ItemID, "kind": "uniform", "status": "Active"}); err != nil || it == nil {
					return bad("Unknown uniform item")
				}
				items[l.ItemID] = it
			}
			size, err := issueSize(lists, str(it["size_type"]), str(it["name"]), l.Size)
			if err != nil {
				return err
			}
			k := key{l.ItemID, size}
			if _, ok := qty[k]; !ok {
				order = append(order, k)
			}
			qty[k] += l.Qty
			total[l.ItemID] += l.Qty
		}
		lines, amount := []uniformLine{}, 0.0
		for _, k := range order {
			it := items[k.item]
			name, price := str(it["name"]), it["price"].(float64)
			if have := stockOf(ctx, tx, storeID, k.item); int64(total[k.item]) > have {
				return bad("%s has only %d of %s. Ask HRBP Corporate to transfer stock first", sname, have, name)
			}
			lines = append(lines, uniformLine{ItemID: k.item, Name: name, Qty: qty[k], Price: price, Size: k.size})
			amount += price * float64(qty[k])
		}
		if len(lines) == 0 {
			return bad("Add at least one item")
		}
		amount = calc.Round2(amount)
		recover := e.Type == "Permanent" && amount > 0 && (b.Recover == nil || *b.Recover)
		seq, err := store.NextSeq(ctx, tx, "uniform_seq")
		if err != nil {
			return err
		}
		id := fmt.Sprintf("UNI-%d", seq)
		var rule any
		if recover {
			rule = refundRuleText(loadUniformSetting(ctx, tx))
		}
		if _, err := tx.C("uniform_issue").Insert(ctx, bson.M{"_id": id, "employee_id": e.ID, "store_id": storeID, "lines": lines, "amount": amount, "recover": recover,
			"by_name": c.Name, "issued_on": date, "refund_rule": rule}); err != nil {
			return err
		}
		// The movements carry the issue id, so the inventory shows the salary deduction plan.
		for _, l := range lines {
			if err := invMove(ctx, tx, bson.M{"store_id": storeID, "item_id": l.ItemID, "kind": "Issue", "qty": l.Qty, "employee_id": e.ID, "ref": id, "by_name": c.Name}, at); err != nil {
				return err
			}
		}
		if recover {
			refund, err := scheduleUniform(ctx, tx, e.ID, e.DOJ, id, date, amount)
			if err != nil {
				return err
			}
			out = map[string]any{"refundYm": refund}
		}
		if out == nil {
			out = map[string]any{}
		}
		out["id"], out["amount"], out["recover"], out["store"], out["lines"] = id, amount, recover, storeID, lines
		return audit(ctx, tx, c, "Uniform issued", "uniform_issue", id, nil, out)
	})
	return out, err
}

// invReturn takes back from an employee: an asset (Good, or Damaged: written off, with an optional
// recovery from salary for permanent staff), uniform by item and quantity (lines, oldest issue
// first), or what is left of one uniform issue. Without a store it goes into the employee's store.
// A return never reverses the uniform recovery or refund already scheduled.
func (s *Server) invReturn(r *http.Request, c *Ctx) (any, error) {
	if err := s.canInv(c, 2); err != nil {
		return nil, err
	}
	var b struct {
		Store      string    `json:"store"`
		AssetID    int64     `json:"assetId"`
		Damaged    bool      `json:"damaged"`
		Recover    float64   `json:"recover"`
		UniformID  string    `json:"uniformId"`
		EmployeeID string    `json:"employeeId"`
		Lines      []invLine `json:"lines"`
		Note       string    `json:"note"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Recover < 0 {
		return nil, bad("The recovery cannot be negative")
	}
	b.Note = strings.TrimSpace(b.Note)
	ctx := r.Context()
	var out map[string]any
	err := s.st.Tx(ctx, func(tx store.Q) error {
		out = nil
		emp, storeID := b.EmployeeID, b.Store
		var serial, iname, aname string
		var item any
		switch {
		case b.AssetID > 0:
			a, err := tx.C("asset").Lock(ctx, bson.M{"_id": b.AssetID, "status": "Issued"})
			if store.Transient(err) {
				return err // another transaction holds the asset: Tx runs this again once it commits (FOR UPDATE waited)
			}
			if err != nil || a == nil || a["employee_id"] == nil {
				return bad("That asset is not issued")
			}
			it, err := tx.C("inv_item").FindOne(ctx, bson.M{"_id": a["item_id"]})
			if err != nil || it == nil {
				return bad("That asset is not issued")
			}
			item, serial, emp, iname, aname = a["item_id"], str(a["serial"]), str(a["employee_id"]), str(it["name"]), str(a["name"])
		case b.UniformID != "":
			u, err := tx.C("uniform_issue").FindOne(ctx, bson.M{"_id": b.UniformID})
			if err != nil || u == nil {
				return notFound("Uniform issue not found")
			}
			emp = str(u["employee_id"])
		case len(b.Lines) > 0:
			if emp == "" {
				return bad("Choose the employee")
			}
		default:
			return bad("Choose what was returned")
		}
		if err := c.MustInScope(ctx, tx, emp); err != nil {
			return err
		}
		if storeID == "" {
			var err error
			if storeID, err = s.storeForEmp(ctx, tx, c, emp); err != nil {
				return err
			}
		}
		if err := s.storeAllowed(ctx, tx, c, storeID); err != nil {
			return err
		}
		out = map[string]any{"store": storeID}
		if b.AssetID == 0 {
			want := map[int]int{}
			for _, l := range b.Lines {
				want[l.ItemID] += l.Qty
			}
			n, err := returnUniform(ctx, tx, c, emp, storeID, want, b.UniformID, b.Note)
			if err != nil {
				return err
			}
			out["units"] = n
			return audit(ctx, tx, c, "Uniform returned", "uniform_issue", b.UniformID, nil, b)
		}
		if b.Recover > 0 && !b.Damaged {
			return bad("A recovery from salary is only for a damaged or lost asset")
		}
		status := "Available"
		if b.Damaged {
			status = "Damaged"
		}
		if _, err := tx.C("asset").Update(ctx, bson.M{"_id": b.AssetID}, bson.M{"$set": bson.M{"status": status, "store_id": storeID, "employee_id": nil, "issued_at": nil,
			"note": b.Note}}); err != nil {
			return err
		}
		if err := invMove(ctx, tx, bson.M{"store_id": storeID, "item_id": item, "kind": "Return", "qty": 1, "employee_id": emp, "serials": []string{serial}, "note": b.Note,
			"by_name": c.Name}, nil); err != nil {
			return err
		}
		if b.Damaged {
			// A damaged asset comes back and is written off the store's stock (prototype invRetSave).
			if err := invMove(ctx, tx, bson.M{"store_id": storeID, "item_id": item, "kind": "Write-off", "qty": 1, "employee_id": emp, "serials": []string{serial},
				"note": "Returned damaged", "by_name": c.Name}, nil); err != nil {
				return err
			}
			if b.Recover > 0 {
				typ := ""
				if e, _ := tx.C("employee").FindOne(ctx, bson.M{"_id": emp}, options.FindOne().SetProjection(bson.D{{Key: "type", Value: 1}})); e != nil {
					typ = str(e["type"])
				}
				if typ != "Permanent" {
					return bad("Recovery for a damaged asset is for permanent staff")
				}
				label := "Asset damaged: " + strings.Join(strings.Fields(iname+" "+aname+" "+serial), " ")
				why := b.Note
				if why == "" {
					why = "Asset returned damaged"
				}
				if err := addLedger(ctx, tx, emp, dedLineMonth(ctx, tx), "ded", label, b.Recover, "AST-"+itoa(int(b.AssetID)), why); err != nil {
					return err
				}
				out["recover"] = b.Recover
			}
		}
		return audit(ctx, tx, c, "Asset returned", "asset", serial, nil, b)
	})
	return out, err
}

// invLost marks an asset lost; for permanent staff an optional recovery goes to the open month.
func (s *Server) invLost(r *http.Request, c *Ctx) (any, error) {
	if err := s.canInv(c, 2); err != nil {
		return nil, err
	}
	var b struct {
		Recover float64 `json:"recover"`
		Note    string  `json:"note"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(b.Note)) < 3 {
		return nil, bad("Say what happened")
	}
	ctx, id := r.Context(), r.PathValue("id")
	aid, ok := invNumericID(id)
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		if !ok {
			return notFound("Asset not found")
		}
		a, err := tx.C("asset").Lock(ctx, bson.M{"_id": aid, "status": bson.M{"$in": bson.A{"Issued", "Available", "Damaged"}}})
		if store.Transient(err) {
			return err // another transaction holds the asset: Tx runs this again once it commits (FOR UPDATE waited)
		}
		if err != nil || a == nil {
			return notFound("Asset not found")
		}
		item, serial, sid := a["item_id"], str(a["serial"]), str(a["store_id"])
		emp, held := a["employee_id"].(string)
		if err := s.storeAllowed(ctx, tx, c, sid); err != nil {
			return err
		}
		if _, err := tx.C("asset").Update(ctx, bson.M{"_id": aid}, bson.M{"$set": bson.M{"status": "Lost", "note": b.Note}}); err != nil {
			return err
		}
		if !held {
			// Lost from the store: a write-off.
			if err := invMove(ctx, tx, bson.M{"store_id": sid, "item_id": item, "kind": "Write-off", "qty": 1, "serials": []string{serial}, "note": b.Note, "by_name": c.Name}, nil); err != nil {
				return err
			}
		}
		if held && b.Recover > 0 {
			typ, name := "", ""
			e, _ := tx.C("employee").FindOne(ctx, bson.M{"_id": emp}, options.FindOne().SetProjection(bson.D{{Key: "type", Value: 1}}))
			it, _ := tx.C("inv_item").FindOne(ctx, bson.M{"_id": item}, options.FindOne().SetProjection(bson.D{{Key: "name", Value: 1}}))
			if e != nil && it != nil {
				typ, name = str(e["type"]), str(it["name"])
			}
			if typ != "Permanent" {
				return bad("Recovery for a lost asset is for permanent staff")
			}
			if err := addLedger(ctx, tx, emp, dedLineMonth(ctx, tx), "ded", "Asset lost: "+name+" "+serial, b.Recover, "AST-"+id, b.Note); err != nil {
				return err
			}
		}
		return audit(ctx, tx, c, "Asset marked lost", "asset", serial, nil, b)
	})
}

// invEmployee lists what an employee holds: assets and uniform issues.
func (s *Server) invEmployee(r *http.Request, c *Ctx) (any, error) {
	ctx, id := r.Context(), r.PathValue("id")
	if id != c.EmployeeID {
		if err := s.canInv(c, 1); err != nil {
			return nil, err
		}
		if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
			return nil, err
		}
	}
	assets, _ := invHeldAssets(ctx, s.st.DB, id)
	uniforms, _ := invUniformsOf(ctx, s.st.DB, id)
	out := map[string]any{"assets": assets, "uniforms": uniforms}
	// The store a return goes back into (the employee's HRBP's, else central).
	if s.canInv(c, 1) == nil {
		if st, err := s.storeForEmp(ctx, s.st.DB, c, id); err == nil {
			name := ""
			if x, _ := s.st.DB.C("inv_store").FindOne(ctx, bson.M{"_id": st}); x != nil {
				name = str(x["name"])
			}
			out["store"], out["storeName"] = st, name
		}
	}
	return out, nil
}

// invHeldAssets lists the assets an employee holds (or held when lost), latest issue first.
func invHeldAssets(ctx context.Context, q store.Q, emp string) ([]store.Row, error) {
	rows, err := q.C("asset").Find(ctx, bson.M{"employee_id": emp}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	items, err := invItems(ctx, q, invColAny(rows, "item_id"))
	if err != nil {
		return nil, err
	}
	out := []store.Row{}
	for _, a := range rows {
		if it := items[repInt64(a["item_id"])]; it != nil {
			out = append(out, store.Row{"id": a["id"], "type": it["name"], "name": a["name"], "serial": a["serial"], "status": a["status"], "issued_at": a["issued_at"],
				"store_id": a["store_id"]})
		}
	}
	// Newest issue date first; one without a date first of all, as a descending sort put NULLs.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i]["issued_at"], out[j]["issued_at"]
		if (a == nil) != (b == nil) {
			return a == nil
		}
		return str(a) > str(b)
	})
	return out, nil
}

// invIssueDate is the day a uniform issue was made: the date given, else the day it was recorded.
func invIssueDate(u store.Row) string {
	if d := str(u["issued_on"]); d != "" {
		return d
	}
	at, _ := u["at"].(time.Time)
	return store.DateOf(at)
}

// invUniformsOf lists an employee's uniform issues, latest first.
func invUniformsOf(ctx context.Context, q store.Q, emp string) ([]store.Row, error) {
	rows, err := q.C("uniform_issue").Find(ctx, bson.M{"employee_id": emp})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if di, dj := invIssueDate(rows[i]), invIssueDate(rows[j]); di != dj {
			return di > dj
		}
		// The old ORDER BY named the output column: `at` was the 'DD Mon YYYY' text, not the timestamp.
		ai, _ := rows[i]["at"].(time.Time)
		aj, _ := rows[j]["at"].(time.Time)
		return ai.In(store.Loc()).Format("02 Jan 2006") > aj.In(store.Loc()).Format("02 Jan 2006")
	})
	out := make([]store.Row, 0, len(rows))
	for _, u := range rows {
		at, _ := u["at"].(time.Time)
		out = append(out, store.Row{"id": u["id"], "lines": u["lines"], "amount": u["amount"], "recover": u["recover"], "returned": u["returned"], "store_id": u["store_id"],
			"at": at.In(store.Loc()).Format("02 Jan 2006"), "date": invIssueDate(u)})
	}
	return out, nil
}

// dedLineMonth is the month a salary recovery raised now (asset lost or damaged, uniform) goes into: the open month,
// or the next one once its Deduction closure is closed, as for approved deductions (payroll test ITM-54).
func dedLineMonth(ctx context.Context, tx store.Q) string {
	return payLineMonth(ctx, tx, "deduction", openYm(ctx, tx))
}
