package api

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"math"
	"net/http"
	"regexp"
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

// Separation, round 2 of the UI (prototype vSep, vSepSet, sepModal, vFnf): the settings that were
// constants, the case history, the rehire decision at any stage, the absconding check for the start
// dialog, approver terms with who set them, the settlement letter and no dues certificate, and the
// line manager's open cases in /me.

// ---------- Settings ----------

type sepSettings struct {
	AbsDays    int      `json:"absDays"`
	AbsLetters int      `json:"absLetters"`
	AbsWait    int      `json:"absWait"`
	ExitReq    []string `json:"exitReq"`
}

// sepCfg reads the separation settings. The defaults are the rules the code had as constants.
func sepCfg(ctx context.Context, q store.Q) sepSettings {
	row, err := q.C("sep_setting").FindOne(ctx, bson.M{"_id": 1})
	if err != nil || row == nil {
		return sepSettings{AbsDays: 7, AbsLetters: 2, AbsWait: 3, ExitReq: []string{"Resignation", "Contract end", "Retirement"}}
	}
	return sepSettings{AbsDays: approvalsInt(row["abs_days"]), AbsLetters: approvalsInt(row["abs_letters"]), AbsWait: approvalsInt(row["abs_wait"]), ExitReq: strs(row["exit_req"])}
}

// exitNeededQ: whether this exit type needs an exit interview (Settings, "Exit interview is required for").
func exitNeededQ(ctx context.Context, q store.Q, t string) bool {
	return containsStr(sepCfg(ctx, q).ExitReq, t)
}

// relievingFor: the exit types that get relieving and experience letters.
func relievingFor(t string) bool {
	return t == "Resignation" || t == "Contract end" || t == "Retirement"
}

var sepOwners = []string{"Reporting manager", "HRBP", "Finance"}
var sepFin = []string{"HR Admin", "Finance", "Finance Head", "Payroll", "Payroll Manager"}

func canSepSettings(c *Ctx) bool { return c.Role == "HR Admin" && c.Level("emp.sep") >= 2 }

func (s *Server) sepSettingsGet(r *http.Request, c *Ctx) (any, error) {
	if c.Level("emp.sep") < 1 {
		return nil, forbidden("")
	}
	ctx := r.Context()
	all, err := s.st.DB.C("clearance_item").Find(ctx, nil)
	if err != nil {
		return nil, err
	}
	// By owner in the checklist's order (reporting manager, HRBP, Finance), then by sort and id.
	rank := func(owner string) int {
		for i, o := range sepOwners {
			if o == owner {
				return i
			}
		}
		return len(sepOwners) // array_position gives NULL, which sorts last
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if ra, rb := rank(str(a["owner"])), rank(str(b["owner"])); ra != rb {
			return ra < rb
		}
		if sa, sb := approvalsInt(a["sort"]), approvalsInt(b["sort"]); sa != sb {
			return sa < sb
		}
		return approvalsInt(a["id"]) < approvalsInt(b["id"])
	})
	items := []store.Row{}
	for _, x := range all {
		items = append(items, store.Row{"id": x["id"], "owner": x["owner"], "name": x["name"], "auto": x["auto"], "sort": x["sort"], "status": x["status"],
			"inactive_since": x["inactive_since"]})
	}
	cfg := sepCfg(ctx, s.st.DB)
	return map[string]any{"absDays": cfg.AbsDays, "absLetters": cfg.AbsLetters, "absWait": cfg.AbsWait, "exitReq": cfg.ExitReq,
		"types": sepTypes, "owners": sepOwners, "items": items, "canEdit": canSepSettings(c)}, nil
}

func (s *Server) sepSettingsPut(r *http.Request, c *Ctx) (any, error) {
	if !canSepSettings(c) {
		return nil, forbidden("HR Admin changes the separation settings")
	}
	var b sepSettings
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.AbsDays < 1 || b.AbsLetters < 1 || b.AbsLetters > 5 || b.AbsWait < 0 || b.AbsDays > 90 || b.AbsWait > 60 {
		return nil, bad("Check the numbers: at least 1 day absent, 1 to 5 letters, and 0 or more days to wait")
	}
	req := []string{}
	for _, t := range b.ExitReq {
		if !containsStr(sepTypes, t) {
			return nil, bad("Unknown exit type %s", t)
		}
		if !containsStr(req, t) {
			req = append(req, t)
		}
	}
	b.ExitReq = req
	ctx := r.Context()
	err := s.st.Tx(ctx, func(tx store.Q) error {
		before := sepCfg(ctx, tx)
		if _, err := tx.C("sep_setting").Upsert(ctx, bson.M{"_id": 1}, bson.M{"abs_days": b.AbsDays, "abs_letters": b.AbsLetters, "abs_wait": b.AbsWait,
			"exit_req": b.ExitReq, "updated_by": c.Name, "updated_at": tx.Now()}, nil); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Separation settings updated", "sep_setting", "1", before, b)
	})
	if err != nil {
		return nil, err
	}
	return b, nil
}

// sepItemAdd adds a clearance checklist item (prototype sepItemAdd). Items are never deleted.
func (s *Server) sepItemAdd(r *http.Request, c *Ctx) (any, error) {
	if !canSepSettings(c) {
		return nil, forbidden("HR Admin changes the clearance checklist")
	}
	var b struct {
		Owner string `json:"owner"`
		Name  string `json:"name"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Name = strings.TrimSpace(b.Name)
	if !containsStr(sepOwners, b.Owner) {
		return nil, bad("Choose who signs the item: reporting manager, HRBP or Finance")
	}
	if b.Name == "" {
		return nil, bad("Type the item")
	}
	ctx := r.Context()
	var out store.Row
	err := s.st.Tx(ctx, func(tx store.Q) error {
		if ui2SepItemTaken(ctx, tx, b.Owner, b.Name, 0) {
			return bad("Already in the list")
		}
		// The new item goes last in the checklist.
		last := 0
		if top, _ := tx.C("clearance_item").FindOne(ctx, nil, options.FindOne().SetSort(bson.D{{Key: "sort", Value: -1}})); top != nil {
			last = approvalsInt(top["sort"])
		}
		newID, err := tx.C("clearance_item").Insert(ctx, bson.M{"owner": b.Owner, "name": b.Name, "sort": last + 1})
		if err != nil {
			return err
		}
		id := approvalsInt(newID)
		if out, err = ui2SepItem(ctx, tx, id); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Clearance item added", "clearance_item", strconv.Itoa(id), nil, b)
	})
	return out, err
}

// ui2SepItemRow is a checklist item as the settings return it.
func ui2SepItemRow(x store.Row) store.Row {
	return store.Row{"id": x["id"], "owner": x["owner"], "name": x["name"], "auto": x["auto"], "sort": x["sort"], "status": x["status"]}
}

// ui2SepItem reads one checklist item (nil when there is none).
func ui2SepItem(ctx context.Context, q store.Q, id int) (store.Row, error) {
	x, err := q.C("clearance_item").FindOne(ctx, bson.M{"_id": id})
	if err != nil || x == nil {
		return nil, err
	}
	return ui2SepItemRow(x), nil
}

// ui2SepItemTaken: the owner already has an item of this name, in any case (other than item except).
func ui2SepItemTaken(ctx context.Context, q store.Q, owner, name string, except int) bool {
	items, _ := q.C("clearance_item").Find(ctx, bson.M{"owner": owner})
	for _, x := range items {
		if approvalsInt(x["id"]) != except && strings.ToLower(str(x["name"])) == strings.ToLower(name) {
			return true
		}
	}
	return false
}

// sepItemPut renames an item or makes it inactive or active again. An automatic item keeps its rule.
func (s *Server) sepItemPut(r *http.Request, c *Ctx) (any, error) {
	if !canSepSettings(c) {
		return nil, forbidden("HR Admin changes the clearance checklist")
	}
	var b struct {
		Name   *string `json:"name"`
		Status string  `json:"status"`
		Reason string  `json:"reason"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		return nil, notFound("Clearance item not found")
	}
	if b.Status != "" && b.Status != "Active" && b.Status != "Inactive" {
		return nil, bad("Status is Active or Inactive")
	}
	ctx := r.Context()
	var out store.Row
	err = s.st.Tx(ctx, func(tx store.Q) error {
		locked, err := tx.C("clearance_item").Lock(ctx, bson.M{"_id": id})
		if err != nil || locked == nil {
			return orNotFound(err)
		}
		before := ui2SepItemRow(locked)
		if b.Name != nil {
			n := strings.TrimSpace(*b.Name)
			if n == "" {
				return bad("Type the item")
			}
			if ui2SepItemTaken(ctx, tx, str(before["owner"]), n, id) {
				return bad("Already in the list")
			}
			if _, err := tx.C("clearance_item").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"name": n}}); err != nil {
				return err
			}
		}
		switch b.Status {
		case "Inactive":
			if before["status"] != "Inactive" {
				if _, err := tx.C("clearance_item").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"status": "Inactive", "inactive_since": today(),
					"inactive_reason": approvalsNullIf(strings.TrimSpace(b.Reason))}}); err != nil {
					return err
				}
			}
		case "Active":
			if _, err := tx.C("clearance_item").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"status": "Active", "inactive_since": nil, "inactive_reason": nil}}); err != nil {
				return err
			}
		}
		if out, err = ui2SepItem(ctx, tx, id); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Clearance item changed", "clearance_item", strconv.Itoa(id), before, out)
	})
	return out, err
}

// ---------- Case events and history ----------

// sepEvent records something the other records do not carry with a date and a name.
func sepEvent(ctx context.Context, q store.Q, c *Ctx, sepID, kind, t string) error {
	by, role := "System", ""
	if c != nil {
		by, role = c.Name, c.Role
	}
	_, err := q.C("sep_event").Insert(ctx, bson.M{"sep_id": sepID, "kind": kind, "t": t, "by_name": by, "by_role": approvalsNullIf(role)})
	return err
}

type sepHistItem struct {
	At   string `json:"at"` // RFC 3339
	By   string `json:"by"`
	Role string `json:"role,omitempty"`
	T    string `json:"t"`
	at   time.Time
	ord  int
}

var reNameRole = regexp.MustCompile(`^(.+) \(([^()]+)\)$`)
var reISODate = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)

// splitBy turns "Meera Kulkarni (HRBP)" into the name and the role.
func splitBy(s string) (string, string) {
	if m := reNameRole.FindStringSubmatch(s); m != nil {
		return m[1], m[2]
	}
	return s, ""
}

var rehireLabels = map[string]string{"Eligible": "Yes", "Only with CHRO approval": "Only with CHRO approval", "Not eligible": "No", "": "not decided"}

func inr0(v float64) string { return calc.Inr(math.Round(v)) }

// sv is the string a nullable column holds, or "".
func sv(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// sepHistory puts the case's story together, oldest first: started, sent for approval, every approval
// step, show-cause letters, clearance ticks, exit interview, full and final, letters, last working day
// changes, terms, rehire decisions, and the end (closed, cancelled, withdrawn, reported back).
func (s *Server) sepHistory(ctx context.Context, q store.Q, sc *sepCase) []sepHistItem {
	h := []sepHistItem{}
	add := func(at time.Time, ord int, by, t string) {
		name, role := splitBy(by)
		h = append(h, sepHistItem{At: at.Format(time.RFC3339), By: name, Role: role, T: t, at: at, ord: ord})
	}
	var started time.Time
	var closedAt *time.Time
	var closedBy, closeNote *string
	if x, _ := q.C("separation").FindOne(ctx, bson.M{"_id": sc.ID}); x != nil {
		started, _ = x["started_at"].(time.Time)
		if t, ok := x["closed_at"].(time.Time); ok {
			closedAt = &t
		}
		closedBy, closeNote = approvalsStrPtr(x["closed_by"]), approvalsStrPtr(x["close_note"])
	}
	var req *Request
	var p requestPayload
	if sc.RequestID != nil {
		if x, err := loadRequest(ctx, q, *sc.RequestID, false); err == nil {
			req, p = x, payloadOf(x)
		}
	}
	// The last working day the case started with (later changes are their own entries).
	orig := sv(sc.LWD)
	if req != nil {
		if sc.Type == "Resignation" && p.NoticeLWD != "" {
			orig = p.NoticeLWD
		} else if sc.Type != "Resignation" && p.LWD != "" {
			orig = p.LWD
		}
	}
	t := sc.Type + " started"
	if _, role := splitBy(sc.StartedBy); sc.Type == "Resignation" && role != "Employee" && role != "" {
		name, _ := splitBy(sc.StartedBy)
		t = fmt.Sprintf("Resignation received on %s, recorded by %s", longDate(sc.GivenOn), name)
		if orig != "" {
			t += ". Last working day " + longDate(orig)
		}
	} else if orig != "" {
		t += ", last working day " + longDate(orig)
	}
	add(started, 0, sc.StartedBy, t)

	if req != nil {
		if sc.Type != "Resignation" {
			if r, _ := q.C("approval_request").FindOne(ctx, bson.M{"_id": req.ID}); r != nil {
				at, _ := r["created_at"].(time.Time)
				by := str(r["raised_by"]) // the raiser's name, or their id when there is no such employee
				if e, _ := q.C("employee").FindOne(ctx, bson.M{"_id": r["raised_by"]}); e != nil && e["name"] != nil {
					by = str(e["name"])
				}
				add(at, 1, by, "Sent for approval as "+req.ID)
			}
		}
		steps, err := q.C("approval_history").Find(ctx, bson.M{"request_id": req.ID}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
		if err == nil {
			type hr struct {
				stage, action, by, note string
				at                      time.Time
			}
			list := []hr{}
			for _, y := range steps {
				x := hr{stage: str(y["stage"]), action: str(y["action"]), by: str(y["by_name"]), note: str(y["note"])}
				x.at, _ = y["at"].(time.Time)
				list = append(list, x)
			}
			for _, x := range list {
				switch {
				case x.action == "Terms changed": // in the case's own events
				case x.action == "Cancelled" && closedAt != nil: // the end of the case says it
				case x.action == "Applied":
					note := reISODate.ReplaceAllStringFunc(x.note, longDate) // "Last working day 2026-12-01." reads as a date
					add(x.at, 2, "Approval flow", strings.TrimSpace("Approved by the last approver. "+note))
				default:
					t := x.action
					if x.stage != "" {
						t = x.stage + ": " + strings.ToLower(x.action)
					}
					if x.note != "" && x.note != "ok" {
						t += ": " + x.note
					}
					add(x.at, 2, x.by, t)
				}
			}
		}
	}
	type row struct {
		at       time.Time
		by, a, b string
	}
	// find reads (at, by, a, b) lines from a collection, in the order given; a line without a by is left out, as
	// the old scan of a NULL into a string dropped it.
	find := func(coll string, filter bson.M, sort bson.D, limit int64, line func(x store.Row) row) []row {
		out := []row{}
		opts := options.Find()
		if sort != nil {
			opts.SetSort(sort)
		}
		if limit > 0 {
			opts.SetLimit(limit)
		}
		list, err := q.C(coll).Find(ctx, filter, opts)
		if err != nil {
			return out
		}
		for _, x := range list {
			y := line(x)
			if y.at, _ = x["at"].(time.Time); x["by_name"] != nil {
				out = append(out, y)
			}
		}
		return out
	}
	byID := bson.D{{Key: "_id", Value: 1}}
	letter := func(x store.Row) row { return row{by: str(x["by_name"]), a: str(x["kind"])} }
	for i, x := range find("sep_letter", bson.M{"sep_id": sc.ID, "kind": "Show-cause notice"}, byID, 0, letter) {
		add(x.at, 3, x.by, fmt.Sprintf("Show-cause letter %d sent", i+1))
	}
	// Clearance ticks with the item's owner and name, oldest first (a tick whose item is missing is left out).
	ticks := []row{}
	if signed, err := q.C("clearance").Find(ctx, bson.M{"sep_id": sc.ID}, options.Find().SetSort(bson.D{{Key: "at", Value: 1}, {Key: "item_id", Value: 1}})); err == nil {
		itemIDs := []int32{}
		for _, x := range signed {
			if id, ok := x["item_id"].(int32); ok {
				itemIDs = append(itemIDs, id)
			}
		}
		items, _ := approvalsByID(ctx, q, "clearance_item", itemIDs)
		for _, x := range signed {
			id, _ := x["item_id"].(int32)
			if it := items[id]; it != nil {
				at, _ := x["at"].(time.Time)
				ticks = append(ticks, row{at, str(x["by_name"]), str(it["owner"]), str(it["name"])})
			}
		}
	}
	for _, x := range ticks {
		add(x.at, 4, x.by, fmt.Sprintf("%s: ticked %q", x.a, x.b))
	}
	plain := func(x store.Row) row { return row{by: str(x["by_name"])} }
	exits := find("exit_interview", bson.M{"sep_id": sc.ID}, nil, 0, plain)
	for _, x := range exits {
		add(x.at, 5, x.by, "Exit interview filled")
	}
	// A part of the clearance that is complete now (prototype "<owner> clearance complete"), as of the
	// last thing that completed it: a tick, the exit interview, or the last asset or uniform return.
	if items, err := clearanceOf(ctx, q, sc); err == nil {
		returns := find("inv_move", bson.M{"employee_id": sc.EmployeeID, "kind": "Return", "at": bson.M{"$gte": started}}, bson.D{{Key: "at", Value: -1}}, 1, plain)
		for _, o := range sepOwners {
			done, ticked := true, false
			var last row
			later := func(x row) {
				if x.at.After(last.at) {
					last = x
				}
			}
			for _, it := range items {
				if it.Owner != o {
					continue
				}
				done = done && it.Done
				switch {
				case it.Auto == "exit" && len(exits) > 0:
					later(exits[0])
				case it.Auto == "assets" && len(returns) > 0:
					later(returns[0])
				}
			}
			for _, x := range ticks {
				if x.a == o {
					ticked = true
					later(x)
				}
			}
			if done && ticked {
				add(last.at, 5, last.by, o+" clearance complete")
			}
		}
	}
	byAt := bson.D{{Key: "at", Value: 1}, {Key: "_id", Value: 1}}
	settlement := func(x store.Row) row { return row{by: str(x["by_name"]), a: str(x["id"]), b: ui2SepWhole(x["net"])} }
	for _, x := range find("fnf", bson.M{"sep_id": sc.ID}, byAt, 0, settlement) {
		n, _ := strconv.ParseFloat(x.b, 64)
		add(x.at, 6, x.by, fmt.Sprintf("Full and final %s sent for approval, net %s", x.a, inr0(n)))
		decision := func(y store.Row) row { return row{by: str(y["by_name"]), a: str(y["action"]), b: str(y["note"])} }
		for _, y := range find("approval_history", bson.M{"request_id": x.a, "action": bson.M{"$in": []string{"Applied", "Rejected"}}}, byID, 0, decision) {
			if y.a == "Applied" {
				add(y.at, 6, "Approval flow", "Full and final "+x.a+" approved")
			} else {
				add(y.at, 6, y.by, "Full and final "+x.a+" rejected: "+y.b)
			}
		}
	}
	for _, x := range find("sep_letter", bson.M{"sep_id": sc.ID, "kind": bson.M{"$ne": "Show-cause notice"}}, byID, 0, letter) {
		add(x.at, 7, x.by, x.a+" issued")
	}
	event := func(x store.Row) row {
		by := str(x["by_name"])
		if role, ok := x["by_role"].(string); ok {
			by += " (" + role + ")"
		}
		return row{by: by, a: str(x["t"])}
	}
	for _, x := range find("sep_event", bson.M{"sep_id": sc.ID}, byID, 0, event) {
		add(x.at, 8, x.by, x.a)
	}
	if closedAt != nil {
		t := sv(closeNote)
		if sc.Status == "Closed" {
			t = "Closed. Eligible for rehire: " + rehireLabels[sv(sc.Rehire)]
			if sv(closeNote) != "" {
				t += ". " + sv(closeNote)
			}
		} else if t == "" {
			t = "Cancelled"
		}
		add(*closedAt, 9, sv(closedBy), t)
	}
	sort.SliceStable(h, func(i, j int) bool {
		if !h[i].at.Equal(h[j].at) {
			return h[i].at.Before(h[j].at)
		}
		return h[i].ord < h[j].ord
	})
	return h
}

// ---------- List and detail additions ----------

// approvalPeople names the person at each stage of a request (as GET /api/approvals/{id}).
func approvalPeople(ctx context.Context, q store.Q, req *Request) []map[string]string {
	people := []map[string]string{}
	subj, err := loadSubject(ctx, q, req.EmployeeID)
	if err != nil {
		return people
	}
	for _, st := range req.Chain {
		people = append(people, map[string]string{"stage": st, "person": resolvePerson(ctx, q, st, subj, req.Facts).Name})
	}
	return people
}

// approvalNext is "Stage: Person" for a request waiting on someone.
func approvalNext(ctx context.Context, q store.Q, req *Request) string {
	if req == nil || req.Status != "Pending" || req.Step >= len(req.Chain) {
		return ""
	}
	subj, err := loadSubject(ctx, q, req.EmployeeID)
	if err != nil {
		return req.Chain[req.Step]
	}
	return req.Chain[req.Step] + ": " + resolvePerson(ctx, q, req.Chain[req.Step], subj, req.Facts).Name
}

// closedNext is the next-step line of a closed case (prototype sepNext: "Closed <when>").
func closedNext(ctx context.Context, q store.Q, id string) string {
	x, _ := q.C("separation").FindOne(ctx, bson.M{"_id": id})
	at, ok := x["closed_at"].(time.Time)
	if !ok {
		return "Closed"
	}
	return "Closed " + at.Format("02 Jan 2006")
}

// sepWaitingForMe: whether the case waits on the caller now (prototype vSep "waiting for you"):
// the current approver, an owner of an open part of the clearance, or HR for show-cause, letters and close.
func (s *Server) sepWaitingForMe(ctx context.Context, q store.Q, c *Ctx, sc *sepCase, st string, mgrID *string, req *Request) bool {
	if sc.EmployeeID == c.EmployeeID {
		return false
	}
	switch st {
	case "Approval":
		return req != nil && canAct(ctx, q, c, req)
	case "Notice", "Clearance":
		items, _ := clearanceOf(ctx, q, sc)
		for _, o := range sepOwners {
			open := false
			for _, it := range items {
				if it.Owner == o && !it.Done {
					open = true
				}
			}
			if !open {
				continue
			}
			switch o {
			case "Reporting manager":
				if mgrID != nil && *mgrID == c.EmployeeID {
					return true
				}
			case "HRBP":
				if isSepHR(c) {
					return true
				}
			case "Finance":
				if containsStr(sepFin, c.Role) && c.Level("emp.sep") >= 1 {
					return true
				}
			}
		}
	case "Show-cause", "Letters", "Ready to close":
		return isSepHR(c)
	}
	return false
}

// sepListPerson is the employee part of a list row: their name, location, department and designation names and
// manager, all blank (manager nil) when the employee or one of the three is missing, as the old JOINs had it.
// sepList reads them in batches for the whole list.
type sepListPerson struct {
	name, loc, dept, desig string
	mgr                    *string
}

// sepListRow is one row of GET /api/separations.
func (s *Server) sepListRow(ctx context.Context, c *Ctx, sc *sepCase, st, next string, p sepListPerson) map[string]any {
	q := s.st.DB
	name, loc, dept, desig, mgr := p.name, p.loc, p.dept, p.desig, p.mgr
	var req *Request
	if st == "Approval" && sc.RequestID != nil {
		if x, err := loadRequest(ctx, q, *sc.RequestID, false); err == nil {
			req = x
			if n := approvalNext(ctx, q, req); n != "" {
				next = n
			}
		}
	}
	if st == "Closed" {
		next = closedNext(ctx, q, sc.ID)
	}
	return map[string]any{"id": sc.ID, "employeeId": sc.EmployeeID, "name": name, "location": loc, "dept": dept, "designation": desig, "managerId": mgr,
		"type": sc.Type, "stage": st, "next": next, "lwd": sc.lwd(), "givenOn": sc.GivenOn, "startedBy": sc.StartedBy,
		"waitingForMe": s.sepWaitingForMe(ctx, q, c, sc, st, mgr, req)}
}

// sepDetailMore adds to GET /api/separations/{id}: history, the named approvers, the HRBP, who closed it,
// who set the terms, and the settings the case follows.
func (s *Server) sepDetailMore(ctx context.Context, c *Ctx, sc *sepCase, out map[string]any) {
	q := s.st.DB
	out["history"] = s.sepHistory(ctx, q, sc)
	var closedAt *time.Time
	var closedBy *string
	if x, _ := q.C("separation").FindOne(ctx, bson.M{"_id": sc.ID}); x != nil {
		if t, ok := x["closed_at"].(time.Time); ok {
			closedAt = &t
		}
		closedBy = approvalsStrPtr(x["closed_by"])
	}
	out["closedBy"], out["closedAt"] = closedBy, nil
	if closedAt != nil {
		out["closedAt"] = closedAt.Format(time.RFC3339)
		if out["stage"] == "Closed" {
			out["next"] = "Closed " + closedAt.Format("02 Jan 2006")
		}
	}
	if subj, err := loadSubject(ctx, q, sc.EmployeeID); err == nil {
		out["hrbp"] = resolvePerson(ctx, q, "HRBP", subj, approval.Facts{}).Name
	}
	out["relieving"] = relievingFor(sc.Type)
	out["settings"] = sepCfg(ctx, q)
	rq, ok := out["request"].(map[string]any)
	if !ok || sc.RequestID == nil {
		return
	}
	req, err := loadRequest(ctx, q, *sc.RequestID, false)
	if err != nil {
		return
	}
	people := approvalPeople(ctx, q, req)
	rq["people"], rq["stepIndex"] = people, req.Step
	var raw map[string]any
	_ = json.Unmarshal(req.Payload, &raw)
	for _, k := range []string{"termsBy", "termsRole", "termsAt"} {
		if v, ok := raw[k]; ok {
			rq[k] = v
		}
	}
	if out["stage"] == "Approval" && req.Status == "Pending" && req.Step < len(people) {
		out["next"] = req.Chain[req.Step] + ": " + people[req.Step]["person"]
	}
}

// ---------- Rehire, terms ----------

// sepRehire saves "Eligible for rehire" at any stage of a running or closed case (prototype sp_rh).
func (s *Server) sepRehire(r *http.Request, c *Ctx) (any, error) {
	var b struct {
		Rehire string `json:"rehire"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Rehire != "" && !containsStr([]string{"Eligible", "Not eligible", "Only with CHRO approval"}, b.Rehire) {
		return nil, bad("Choose Yes, No or Only with CHRO approval")
	}
	ctx := r.Context()
	err := s.st.Tx(ctx, func(tx store.Q) error {
		sc, err := sepLock(ctx, tx, r.PathValue("id"))
		if err != nil {
			return notFound("Separation case not found")
		}
		if err := s.canSepView(ctx, c, sc); err != nil {
			return err
		}
		if !isSepHR(c) || sc.EmployeeID == c.EmployeeID {
			return forbidden("Only HR decides whether this person can be rehired")
		}
		if st, _ := s.stage(ctx, tx, sc); st == "Rejected" || st == "Withdrawn" || st == "Cancelled" {
			return bad("This exit has ended")
		}
		if sv(sc.Rehire) == b.Rehire {
			return nil
		}
		if _, err := tx.C("separation").Update(ctx, bson.M{"_id": sc.ID}, bson.M{"$set": bson.M{"rehire": approvalsNullIf(b.Rehire)}}); err != nil {
			return err
		}
		// Someone who has already left carries the decision on their record (rejoining reads it).
		if _, err := tx.C("employee").Update(ctx, bson.M{"_id": sc.EmployeeID, "status": "Inactive"}, bson.M{"$set": bson.M{"rehire": approvalsNullIf(b.Rehire)}}); err != nil {
			return err
		}
		if err := sepEvent(ctx, tx, c, sc.ID, "rehire", "Eligible for rehire set to "+rehireLabels[b.Rehire]); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Separation rehire", "separation", sc.ID, nil, b)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"rehire": b.Rehire}, nil
}

// sepTerms: whoever can act on the separation request right now (canAct, even without a right to view
// separations) may bring the last working day earlier, never later, and waive or recover the shortfall.
// Who set it, with the role, goes into the request and the case history.
func (s *Server) sepTerms(r *http.Request, c *Ctx) (any, error) {
	var b struct {
		LWD       string `json:"lwd"`
		Shortfall string `json:"shortfall"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	var out map[string]any
	err := s.st.Tx(ctx, func(tx store.Q) error {
		sc, err := sepLock(ctx, tx, r.PathValue("id"))
		if err != nil {
			return notFound("Separation case not found")
		}
		if sc.RequestID == nil {
			return bad("No approval is running")
		}
		req, err := loadRequest(ctx, tx, *sc.RequestID, true)
		if err != nil {
			return err
		}
		if !canAct(ctx, tx, c, req) {
			return forbidden("Only the current approver can change the terms")
		}
		p := payloadOf(req)
		if !isDate(b.LWD) || b.LWD > p.NoticeLWD || b.LWD < today() {
			return bad("The last working day can be brought earlier, not later than %s, and not in the past", longDate(p.NoticeLWD))
		}
		if b.LWD < p.NoticeLWD && b.Shortfall != "waive" && b.Shortfall != "recover" {
			return bad("Choose to waive or recover the notice shortfall")
		}
		p.LWD, p.Shortfall = b.LWD, b.Shortfall
		if b.LWD == p.NoticeLWD {
			p.Shortfall = ""
		}
		// The request's payload becomes the new terms, with who set them, in what role and when.
		payload := map[string]any{}
		if err := approvalsJSON(p, &payload); err != nil {
			return err
		}
		at := time.Now().Format(time.RFC3339)
		payload["termsBy"], payload["termsRole"], payload["termsAt"] = c.Name, c.Role, at
		if _, err := tx.C("approval_request").Update(ctx, bson.M{"_id": req.ID}, bson.M{"$set": bson.M{"payload": payload}}); err != nil {
			return err
		}
		if _, err := tx.C("separation").Update(ctx, bson.M{"_id": sc.ID}, bson.M{"$set": bson.M{"lwd": b.LWD}}); err != nil {
			return err
		}
		who := c.Name + " (" + c.Role + ")"
		t := fmt.Sprintf("Last working day kept as per notice, %s, by %s", longDate(b.LWD), who)
		if b.LWD < p.NoticeLWD {
			n := policy.DayDiff(b.LWD, p.NoticeLWD)
			t = fmt.Sprintf("Last working day brought earlier to %s by %s. Notice shortfall of %d day%s %s", longDate(b.LWD), who, n, plural(n),
				map[string]string{"waive": "waived", "recover": "recovered from the employee"}[p.Shortfall])
		}
		if err := history(ctx, tx, req.ID, "", "Terms changed", c.Name, t); err != nil {
			return err
		}
		if err := sepEvent(ctx, tx, c, sc.ID, "terms", t); err != nil {
			return err
		}
		out = map[string]any{"lwd": p.LWD, "shortfall": p.Shortfall, "termsBy": c.Name, "termsRole": c.Role, "termsAt": at}
		return audit(ctx, tx, c, "Separation terms", "separation", sc.ID, nil, b)
	})
	return out, err
}

// ---------- Absconding check for the start dialog ----------

// absentIn counts the days in the last n completed days with no punch and no leave or regularization.
func absentIn(ctx context.Context, q store.Q, e *emp, n int) int {
	ts, err := loadTimeSet(ctx, q)
	if err != nil {
		return 0
	}
	p, _ := ts.policyFor(e)
	from, to := policy.AddDays(today(), -n), policy.AddDays(today(), -1)
	m, err := loadMonth(ctx, q, ts, []string{e.ID}, from, to)
	if err != nil {
		return 0
	}
	k := 0
	for d := from; d <= to; d = policy.AddDays(d, 1) {
		if d < e.DOJ || m.leaveOn(e, d, true) != nil || m.regs[e.ID][d] != "" {
			continue
		}
		if st, _ := m.status(e, p, d); st == "No punch" || strings.HasPrefix(st, "Absent") {
			k++
		}
	}
	return k
}

// sepAbsent: the last punch and the days absent without leave in the configured window (prototype
// sepStartBody, "Last punch … · N of the last 7 days absent without leave").
func (s *Server) sepAbsent(r *http.Request, c *Ctx) (any, error) {
	if c.Level("emp.sep") < 1 {
		return nil, forbidden("")
	}
	ctx, id := r.Context(), r.URL.Query().Get("employeeId")
	if id == "" {
		return nil, bad("Choose the employee")
	}
	if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
		return nil, err
	}
	e, err := loadEmp(ctx, s.st.DB, id)
	if err != nil {
		return nil, err
	}
	cfg := sepCfg(ctx, s.st.DB)
	// The last day with a punch in the 90 days up to today (nil when there is none).
	var last any
	if d, _ := s.st.DB.C("attendance_day").FindOne(ctx, bson.M{"employee_id": id, "date": bson.M{"$gt": policy.AddDays(today(), -90), "$lte": today()},
		"$or": bson.A{bson.M{"in_time": bson.M{"$nin": bson.A{nil, ""}}}, bson.M{"out_time": bson.M{"$nin": bson.A{nil, ""}}}}},
		options.FindOne().SetSort(bson.D{{Key: "date", Value: -1}})); d != nil {
		last = d["date"]
	}
	return map[string]any{"employeeId": id, "lastPunch": last, "absentDays": absentIn(ctx, s.st.DB, e, cfg.AbsDays), "window": cfg.AbsDays,
		"run": absentRun(ctx, s.st.DB, e), "absDays": cfg.AbsDays, "absLetters": cfg.AbsLetters, "absWait": cfg.AbsWait}, nil
}

// ---------- Full and final: estimates, assets, settlement letter, no dues certificate ----------

// fnfEstimates adds the estimated net to rows without a live settlement (prototype vFnf "Net payable").
func (s *Server) fnfEstimates(ctx context.Context, rows []store.Row) {
	for _, x := range rows {
		if x["fnf_id"] != nil && x["fnf_status"] != "Rejected" {
			continue
		}
		id, _ := x["id"].(string)
		if _, L, err := s.fnfCompute(ctx, s.st.DB, id); err == nil {
			x["estNet"] = fnfNet(L)
		}
	}
}

// issuedAssets lists the assets still with the employee (the clearance note in the full and final).
func issuedAssets(ctx context.Context, q store.Q, id string) []store.Row {
	rows, err := sepIssuedAssets(ctx, q, id)
	if err != nil || rows == nil {
		return []store.Row{}
	}
	sort.SliceStable(rows, func(i, j int) bool { // by item name, then serial
		if a, b := str(rows[i]["name"]), str(rows[j]["name"]); a != b {
			return a < b
		}
		return str(rows[i]["serial"]) < str(rows[j]["serial"])
	})
	return rows
}

type fnfRec struct {
	ID, EmployeeID, LWD, PayYm, ApprovedOn string
	Lines                                  []fnfLine
	Net                                    float64
}

// approvedFnf is the approved settlement, by settlement id or employee id.
func approvedFnf(ctx context.Context, q store.Q, id string) *fnfRec {
	x, err := q.C("fnf").FindOne(ctx, bson.M{"$or": bson.A{bson.M{"_id": id}, bson.M{"employee_id": id}}, "status": "Approved"},
		options.FindOne().SetSort(bson.D{{Key: "at", Value: -1}, {Key: "_id", Value: -1}}))
	if err != nil || x == nil {
		return nil
	}
	f := &fnfRec{ID: str(x["id"]), EmployeeID: str(x["employee_id"]), LWD: str(x["lwd"]), PayYm: str(x["pay_ym"])}
	f.Net, _ = x["net"].(float64)
	if approvalsJSON(x["lines"], &f.Lines) != nil {
		return nil
	}
	// Approved on: the day of the last approval or of the result line.
	if h, _ := q.C("approval_history").FindOne(ctx, bson.M{"request_id": f.ID, "action": bson.M{"$in": []string{"Applied", "Approved"}}},
		options.FindOne().SetSort(bson.D{{Key: "at", Value: -1}})); h != nil {
		if at, ok := h["at"].(time.Time); ok {
			f.ApprovedOn = store.DateOf(at)
		}
	}
	return f
}

type letterWho struct{ name, desig, dept, loc, doj, company, addr string }

func letterWhoOf(ctx context.Context, q store.Q, empID string) (*letterWho, error) {
	w, err := ui2SepWho(ctx, q, empID)
	if err != nil {
		return nil, notFound("Employee not found")
	}
	return w, nil
}

// ui2SepWho reads what a letter says about the employee and the company: the employee's name, designation,
// department, location and joining date, and the company's legal name and address (store.ErrNoRows when one of
// them is missing).
func ui2SepWho(ctx context.Context, q store.Q, empID string) (*letterWho, error) {
	e, err := q.C("employee").FindOne(ctx, bson.M{"_id": empID})
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, store.ErrNoRows
	}
	got := []store.Row{}
	for _, l := range []struct {
		coll string
		id   any
	}{{"designation", e["designation_id"]}, {"department", e["department_id"]}, {"location", e["location_id"]}, {"company_setting", 1}} {
		x, err := q.C(l.coll).FindOne(ctx, bson.M{"_id": l.id})
		if err != nil {
			return nil, err
		}
		if x == nil {
			return nil, store.ErrNoRows
		}
		got = append(got, x)
	}
	g, d, l, c := got[0], got[1], got[2], got[3]
	return &letterWho{str(e["name"]), str(g["name"]), str(d["name"]), str(l["name"]), str(e["doj"]), str(c["legal_name"]), str(c["address"])}, nil
}

// ui2SepWhole is to_char(net,'FM999999990'): the amount rounded to a whole number, or # signs past nine digits.
func ui2SepWhole(v any) string {
	r := math.Round(func() float64 { f, _ := v.(float64); return f }())
	if math.Abs(r) > 999999999 {
		return "#########"
	}
	return strconv.FormatFloat(r, 'f', 0, 64)
}

// noDuesText is the no dues certificate after the letter head (prototype fnfNoDues).
func noDuesText(ctx context.Context, q store.Q, empID, name, desig, dept, company, doj, lwd string) string {
	sub := "."
	if f := approvedFnf(ctx, q, empID); f != nil {
		sub = fmt.Sprintf(", subject to the full and final settlement of %s approved on %s.", inr0(f.Net), longDate(f.ApprovedOn))
	}
	return fmt.Sprintf("Subject: No dues certificate\n\nThis is to certify that %s (%s), %s, %s, who worked with %s from %s to %s, has returned all company assets and has no pending dues to the company%s\n\nFor %s\n\nAuthorised signatory",
		name, empID, desig, dept, company, longDate(doj), longDate(lwd), sub, company)
}

func canFnfDocs(c *Ctx) bool {
	return c.Level("pay.run") >= 1 || c.Level("pay.claims") >= 1 || isSepHR(c)
}

// canFnfLetter: the settlement letter lists every amount, so only Payroll reads it (ACC-09); HR issues the no
// dues certificate.
func canFnfLetter(c *Ctx) bool {
	return c.Level("pay.run") >= 1 || c.Level("pay.claims") >= 1
}

const fnfDocStyle = `body{font:14px/1.6 Arial,sans-serif;max-width:720px;margin:40px auto;padding:0 24px;color:#111}h1{font-size:18px}table{border-collapse:collapse;width:100%;margin:12px 0}td,th{border-bottom:1px solid #ddd;padding:6px 8px;text-align:left}td:last-child,th:last-child{text-align:right}.hd{border-bottom:2px solid #111;padding-bottom:8px;margin-bottom:20px}`

// fnfLetter is the full and final settlement letter (prototype ACT2.fnfLetter), as a standalone HTML
// document; {id} is the employee or the settlement.
func (s *Server) fnfLetter(r *http.Request, c *Ctx) (any, error) {
	if !canFnfLetter(c) {
		return nil, forbidden("")
	}
	ctx, q := r.Context(), s.st.DB
	f := approvedFnf(ctx, q, r.PathValue("id"))
	if f == nil {
		return nil, bad("The settlement letter follows the approved full and final")
	}
	if err := c.MustInScope(ctx, q, f.EmployeeID); err != nil {
		return nil, err
	}
	w, err := letterWhoOf(ctx, q, f.EmployeeID)
	if err != nil {
		return nil, err
	}
	e := html.EscapeString
	var rows strings.Builder
	for _, l := range f.Lines {
		if l.Kind == "info" {
			continue
		}
		sign := ""
		if l.Kind == "ded" {
			sign = "− "
		}
		fmt.Fprintf(&rows, `<tr><td>%s</td><td>%s%s</td></tr>`, e(l.Label), sign, inr0(l.Amount))
	}
	// A settlement below zero is owed by the employee and is not paid through payroll (EXT-42).
	netLabel, payText := "Net payable", "This amount will be credited to your bank account with the "+ymLabel(f.PayYm)+" payroll."
	if f.Net < 0 {
		netLabel, payText = "Recoverable from you", "This amount is payable by you to the company. Please contact Payroll to settle it."
	}
	body := fmt.Sprintf(`<div class="hd"><b>%s</b><br>%s</div><h1>Full and final settlement</h1><p>Date: %s</p><p>Employee: <b>%s</b> (%s)<br>Designation: %s, %s<br>Date of joining: %s<br>Last working day: %s</p>`+
		`<p>Your full and final settlement has been worked out as follows. The salary for your final month is paid in the regular payroll run for %s.</p>`+
		`<table><tbody>%s<tr><td><b>%s</b></td><td><b>%s</b></td></tr></tbody></table><p>%s</p><p>For %s<br><br>Authorised signatory</p>`,
		e(w.company), e(w.addr), longDate(today()), e(w.name), e(f.EmployeeID), e(w.desig), e(w.dept), longDate(w.doj), longDate(f.LWD),
		e(ymLabel(f.LWD[:7])), rows.String(), netLabel, inr0(math.Abs(f.Net)), e(payText), e(w.company))
	doc := `<!doctype html><html><head><meta charset="utf-8"><title>Full and final settlement</title><style>` + fnfDocStyle + `</style></head><body>` + body + `</body></html>`
	return map[string]any{"kind": "Full and final settlement", "id": f.ID, "employeeId": f.EmployeeID, "html": doc, "file": "full-and-final-" + f.EmployeeID + ".html"}, nil
}

// fnfNoDues lets Payroll read the no dues certificate once the full and final is approved: the one HR
// issued in the case when there is one, otherwise the certificate as it reads today (not stored).
func (s *Server) fnfNoDues(r *http.Request, c *Ctx) (any, error) {
	if !canFnfDocs(c) {
		return nil, forbidden("")
	}
	ctx, q := r.Context(), s.st.DB
	f := approvedFnf(ctx, q, r.PathValue("id"))
	if f == nil {
		return nil, bad("The no dues certificate follows the approved full and final")
	}
	if err := c.MustInScope(ctx, q, f.EmployeeID); err != nil {
		return nil, err
	}
	if n := len(issuedAssets(ctx, q, f.EmployeeID)); n > 0 {
		return nil, bad("Assets are still pending: %d still issued", n)
	}
	if err := fnfOwedBlocks(ctx, q, f.EmployeeID); err != nil {
		return nil, err
	}
	// The newest certificate HR issued in one of the employee's cases.
	if seps, err := q.C("separation").Distinct(ctx, "_id", bson.M{"employee_id": f.EmployeeID}); err == nil && len(seps) > 0 {
		if l, _ := q.C("sep_letter").FindOne(ctx, bson.M{"sep_id": bson.M{"$in": seps}, "kind": "No dues certificate"},
			options.FindOne().SetSort(bson.D{{Key: "_id", Value: -1}})); l != nil {
			return store.Row{"id": l["id"], "kind": l["kind"], "body": l["body"], "by_name": l["by_name"], "at": sepDay(l["at"])}, nil
		}
	}
	w, err := letterWhoOf(ctx, q, f.EmployeeID)
	if err != nil {
		return nil, err
	}
	head := fmt.Sprintf("%s\n%s\n\nDate: %s\n\nTo: %s (%s)\n%s, %s, %s\n\n", w.company, w.addr, longDate(today()), w.name, f.EmployeeID, w.desig, w.dept, w.loc)
	return map[string]any{"kind": "No dues certificate", "body": head + noDuesText(ctx, q, f.EmployeeID, w.name, w.desig, w.dept, w.company, w.doj, f.LWD)}, nil
}

// ---------- /me additions ----------

// sepPolicyName is the employee's work policy ("as per <policy>" in the resignation dialog).
func sepPolicyName(ctx context.Context, q store.Q, id string) string {
	ts, err := loadTimeSet(ctx, q)
	if err != nil {
		return ""
	}
	e, err := loadEmp(ctx, q, id)
	if err != nil {
		return ""
	}
	p, _ := ts.policyFor(e)
	return p.Name
}

// meCtx is the caller's context for GET /api/me with what the navigation needs besides the rights:
// open separation cases where the caller is the reporting manager (they sign the manager's clearance),
// and the names of the locations and departments the role view is limited to (empty when it sees all).
func (s *Server) meCtx(ctx context.Context, c *Ctx) map[string]any {
	out := map[string]any{}
	b, _ := json.Marshal(c)
	_ = json.Unmarshal(b, &out)
	out["mgrCases"] = ui2SepMgrCases(ctx, s.st.DB, c.EmployeeID)
	locs := []string{}
	if c.Scope.Access == "locs" && len(c.Scope.Locations) > 0 {
		rows, err := s.st.DB.C("location").Find(ctx, bson.M{"_id": bson.M{"$in": c.Scope.Locations}}, options.Find().SetSort(bson.D{{Key: "name", Value: 1}}))
		if err == nil {
			for _, x := range rows {
				locs = append(locs, str(x["name"]))
			}
		}
	}
	depts := append([]string{}, c.Scope.Departments...)
	out["scopeLocations"], out["scopeDepartments"] = locs, depts
	return out
}

// ui2SepMgrCases counts the open cases of the manager's reports (not their own) whose approval is pending or
// approved, or that have none yet.
func ui2SepMgrCases(ctx context.Context, q store.Q, mgr string) int {
	reports, err := q.C("employee").Distinct(ctx, "_id", bson.M{"manager_id": mgr, "_id": bson.M{"$ne": mgr}})
	if err != nil || len(reports) == 0 {
		return 0
	}
	cases, err := q.C("separation").Find(ctx, bson.M{"employee_id": bson.M{"$in": reports}, "status": "Open"})
	if err != nil {
		return 0
	}
	reqIDs := []string{}
	for _, x := range cases {
		reqIDs = append(reqIDs, str(x["request_id"]))
	}
	reqs, err := approvalsByID(ctx, q, "approval_request", reqIDs)
	if err != nil {
		return 0
	}
	n := 0
	for _, x := range cases {
		if r := reqs[str(x["request_id"])]; r == nil || r["status"] == "Pending" || r["status"] == "Approved" {
			n++
		}
	}
	return n
}
