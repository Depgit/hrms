package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/realtime"
	"github.com/saltstayz/hrms/internal/store"
)

// Sending employees to the Realtime portal. Since 19 Nov 2026 (user) the HRMS writes on its own after Mark joined, a
// change of details and an exit (portal_auto.go: jobs, the sweep, Retry and "Send waiting"); HR no longer presses
// "Send to portal" and there is no preview. This file holds what both use: the details sent, the mapping, the
// employee's portal state, the waiting list and settling an unclear reply.
//
//   - Details sent: Employee code and card number (the Attendance code; for a new person the next free number),
//     name, father's name, dates of birth and joining, gender, phone, email, and the portal's department,
//     designation, branch and shift as HR mapped them once (portal_map; a property's branch defaults to the portal
//     branch of the punch machine mapped to it). A value with no mapping holds the write with a clear message.
//   - Every write opens the portal's form afresh, stops on any problem before the post, and posts once, never
//     retried by itself. The portal's own message is shown and kept.
//   - After a create the code becomes the employee's Attendance code (punches match from then on) and the
//     machine-pool enrol to-do is ticked when the branch sent is in the cluster the person is to be enrolled on. A
//     mark-left ticks the remove to-do. Saving on the portal is not the same as the person's face or finger on the
//     machine: the portal's own device step ("Upload Name in Device") is never pressed by the HRMS.
//   - Rights: att.portal (View sees statuses and the list, Edit maps, retries and sends the waiting backlog). Every
//     write is audited with the exact fields sent and the portal's reply, and logged in portal_send.
//
// Settings: company_setting.portal_write (code_start, card_pad, since, lists).

const ptCardPad = 8

// ptKinds are the mapped lists.
var ptKinds = []string{"dept", "desig", "branch", "shift"}

var ptKindRole = map[string]string{"dept": realtime.FieldDept, "desig": realtime.FieldDesig, "branch": realtime.FieldBranch, "shift": realtime.FieldShift}

var ptKindLabel = map[string]string{"dept": "department", "desig": "designation", "branch": "property (branch)", "shift": "shift"}

// ptClient opens a portal session. Tests use the fake portal through rtEnv.
var ptClient = func(cfg realtime.Config) (*realtime.Client, error) { return realtime.New(cfg) }

// ptBusy keeps two writes for one employee from running at once.
var ptBusy sync.Map

// ---------- Settings and mapping ----------

func ptSettings(ctx context.Context, q store.Q) (bson.M, error) {
	r, err := q.C("company_setting").FindOne(ctx, bson.M{"_id": 1}, options.FindOne().SetProjection(bson.M{"portal_write": 1}))
	if err != nil {
		return nil, err
	}
	out := bson.M{}
	if r != nil {
		if m := asMap(r["portal_write"]); m != nil {
			for k, v := range m {
				out[k] = v
			}
		}
	}
	if _, ok := out["card_pad"]; !ok {
		out["card_pad"] = ptCardPad
	}
	return out, nil
}

func ptSet(ctx context.Context, q store.Q, set bson.M) error {
	cur, err := ptSettings(ctx, q)
	if err != nil {
		return err
	}
	for k, v := range set {
		cur[k] = v
	}
	_, err = q.C("company_setting").Update(ctx, bson.M{"_id": 1}, bson.M{"$set": bson.M{"portal_write": map[string]any(cur)}})
	return err
}

func asMap(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case bson.M:
		return m
	case bson.D:
		out := map[string]any{}
		for _, e := range m {
			out[e.Key] = e.Value
		}
		return out
	}
	return nil
}

// ptMaps is HR's mapping: kind → HRMS ref → the portal's choice.
type ptMaps map[string]map[string]string

func ptLoadMaps(ctx context.Context, q store.Q) (ptMaps, error) {
	rows, err := q.C(store.PortalMapColl).Find(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	m := ptMaps{}
	for _, k := range ptKinds {
		m[k] = map[string]string{}
	}
	for _, r := range rows {
		if m[str(r["kind"])] != nil {
			m[str(r["kind"])][str(r["ref"])] = str(r["value"])
		}
	}
	// A property with no mapping takes the portal branch of a machine mapped to it.
	devs, err := q.C("attendance_device").Find(ctx, bson.M{"location_id": bson.M{"$ne": nil}, "portal_branch": bson.M{"$nin": bson.A{nil, ""}}},
		options.Find().SetProjection(bson.M{"location_id": 1, "portal_branch": 1, "status": 1}).SetSort(bson.D{{Key: "id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	m["machine"] = map[string]string{}
	for _, d := range devs {
		if l := str(d["location_id"]); m["machine"][l] == "" || str(d["status"]) == "Active" {
			m["machine"][l] = str(d["portal_branch"])
		}
	}
	return m, nil
}

func (m ptMaps) branch(loc string) string {
	if v := m["branch"][loc]; v != "" {
		return v
	}
	return m["machine"][loc]
}

// ---------- One employee's details ----------

type ptEmp struct {
	ID, Name, Status, LWD, DOJ, Code, LocID, Loc, DeptID, Dept, DesigID, Desig, Shift string
	Father, DOB, Gender, Phone, Email                                                 string
	Sync                                                                              map[string]any
}

func date10(v any) string {
	switch t := v.(type) {
	case time.Time:
		return t.Format("2006-01-02")
	case bson.DateTime:
		return t.Time().In(time.Local).Format("2006-01-02")
	}
	s := str(v)
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

// ptLoad reads the employees (all of them when ids is nil) with what is sent to the portal.
func ptLoad(ctx context.Context, q store.Q, filter bson.M) ([]*ptEmp, error) {
	es, err := q.C("employee").Find(ctx, filter, options.Find().SetProjection(bson.M{"name": 1, "status": 1, "lwd": 1, "doj": 1, "attendance_code": 1,
		"location_id": 1, "department_id": 1, "designation_id": 1, "shift_code": 1, "portal_sync": 1}).SetSort(bson.D{{Key: "name", Value: 1}}))
	if err != nil || len(es) == 0 {
		return nil, err
	}
	names := map[string]map[string]string{}
	for _, coll := range []string{"department", "designation", "location"} {
		rows, err := q.C(coll).Find(ctx, bson.M{}, options.Find().SetProjection(bson.M{"name": 1}))
		if err != nil {
			return nil, err
		}
		names[coll] = map[string]string{}
		for _, r := range rows {
			names[coll][str(r["id"])] = str(r["name"])
		}
	}
	ids := make([]string, 0, len(es))
	for _, e := range es {
		ids = append(ids, str(e["id"]))
	}
	ps, err := q.C("employee_personal").Find(ctx, bson.M{"employee_id": bson.M{"$in": ids}},
		options.Find().SetProjection(bson.M{"employee_id": 1, "dob": 1, "gender": 1, "mobile": 1, "email": 1, "father_spouse": 1}))
	if err != nil {
		return nil, err
	}
	per := map[string]store.Row{}
	for _, p := range ps {
		per[str(p["employee_id"])] = p
	}
	out := make([]*ptEmp, 0, len(es))
	for _, e := range es {
		id := str(e["id"])
		x := &ptEmp{ID: id, Name: str(e["name"]), Status: str(e["status"]), LWD: date10(e["lwd"]), DOJ: date10(e["doj"]), Code: strings.TrimSpace(str(e["attendance_code"])),
			LocID: str(e["location_id"]), DeptID: str(e["department_id"]), DesigID: str(e["designation_id"]), Shift: str(e["shift_code"]), Sync: asMap(e["portal_sync"])}
		x.Loc, x.Dept, x.Desig = names["location"][x.LocID], names["department"][x.DeptID], names["designation"][x.DesigID]
		if p := per[id]; p != nil {
			x.Father, x.DOB, x.Gender, x.Email = strings.TrimSpace(str(p["father_spouse"])), date10(p["dob"]), str(p["gender"]), strings.TrimSpace(str(p["email"]))
			x.Phone = rmPhone(str(p["mobile"]))
		}
		out = append(out, x)
	}
	return out, nil
}

func rmPhone(s string) string {
	d := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
	if len(d) > 10 {
		d = d[len(d)-10:]
	}
	return d
}

// ptCard is the card number for a code: padded with zeros like the portal's (00001050) when the code is a number.
func ptCard(code string, pad int) string {
	if _, err := strconv.Atoi(code); err != nil || pad <= len(code) {
		return code
	}
	return strings.Repeat("0", pad-len(code)) + code
}

// ptValues are the details sent for the employee, and what blocks the send (a value with no mapping).
func ptValues(x *ptEmp, m ptMaps, code string, pad int) (map[string]string, []string) {
	v := map[string]string{realtime.FieldName: x.Name, realtime.FieldFather: x.Father, realtime.FieldDOB: x.DOB, realtime.FieldDOJ: x.DOJ,
		realtime.FieldGender: x.Gender, realtime.FieldPhone: x.Phone, realtime.FieldEmail: x.Email}
	if code != "" {
		v[realtime.FieldCode], v[realtime.FieldCard] = code, ptCard(code, pad)
	}
	var block []string
	for _, k := range ptKinds {
		ref, name := "", ""
		val := ""
		switch k {
		case "dept":
			ref, name, val = x.DeptID, x.Dept, m["dept"][x.DeptID]
		case "desig":
			ref, name, val = x.DesigID, x.Desig, m["desig"][x.DesigID]
		case "branch":
			ref, name, val = x.LocID, x.Loc, m.branch(x.LocID)
		case "shift":
			ref, name, val = x.Shift, x.Shift, m["shift"][x.Shift]
		}
		if ref == "" {
			continue
		}
		if val == "" {
			block = append(block, fmt.Sprintf("Map the %s %q to the portal's list first (Attendance › Punch machine setup › Realtime portal: mapping)", ptKindLabel[k], name))
			continue
		}
		v[ptKindRole[k]] = val
	}
	if x.Name == "" {
		block = append(block, "The employee has no name")
	}
	return v, block
}

// ptHash fingerprints the details (not the code), to show "changed since last send".
func ptHash(v map[string]string) string {
	keys := make([]string, 0, len(v))
	for k := range v {
		if k != realtime.FieldCode && k != realtime.FieldCard {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s\n", k, v[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Statuses on the employee file.
const (
	ptNotOn   = "Not on portal"
	ptOn      = "On portal"
	ptChanged = "Changed since last send"
	ptLeft    = "Left"
	ptCheck   = "Check on the portal"
)

type ptState struct {
	Status  string   `json:"status"`
	Code    string   `json:"code,omitempty"`
	Actions []string `json:"actions"` // what may be sent now: create, update, left
	ExitDue bool     `json:"exitDue"` // inactive or past the last working day, still on the portal
	LeftOn  string   `json:"leftOn"`  // the leaving date a mark-left sends
	Blocked []string `json:"blocked"` // what blocks a create or an update
	Changed []string `json:"changed"` // details changed since the last send
	Matches []string `json:"matches"` // the portal may already have them (Match portal codes)
	Unsent  bool     `json:"unsent"`  // on the portal (Attendance code) but never sent from the HRMS
	Last    any      `json:"last"`    // the last send
}

func ptStateOf(x *ptEmp, m ptMaps, pad int, matches map[string][]string) ptState {
	st := ptState{Code: x.Code, Actions: []string{}, Blocked: []string{}, Changed: []string{}, Matches: matches[x.ID], Last: x.Sync}
	state := str(x.Sync["state"])
	if x.Code == "" && state == "on" {
		st.Code = str(x.Sync["code"])
	}
	on := st.Code != "" && state != "check"
	vals, block := ptValues(x, m, st.Code, pad)
	st.Blocked = block
	if st.Blocked == nil {
		st.Blocked = []string{}
	}
	st.LeftOn = x.LWD
	if st.LeftOn == "" || st.LeftOn > today() {
		st.LeftOn = today()
	}
	gone := x.Status == "Inactive" || (x.LWD != "" && x.LWD <= today())
	switch {
	case state == "left":
		st.Status = ptLeft
	case state == "check":
		st.Status = ptCheck
	case !on:
		st.Status = ptNotOn
		if x.Status == "Active" && !gone {
			st.Actions = append(st.Actions, "create")
		}
	default:
		st.Status = ptOn
		if h := str(x.Sync["hash"]); h != "" && h != ptHash(vals) {
			st.Status = ptChanged
			if old := asMap(x.Sync["values"]); old != nil {
				for _, k := range sortedRoles(vals, old) {
					if k != realtime.FieldCode && k != realtime.FieldCard && str(old[k]) != vals[k] {
						st.Changed = append(st.Changed, realtime.FieldLabels[k])
					}
				}
			}
		}
		st.Unsent = str(x.Sync["hash"]) == ""
		if !gone {
			st.Actions = append(st.Actions, "update")
		}
		if gone || x.LWD != "" {
			st.Actions = append(st.Actions, "left")
		}
		st.ExitDue = gone
	}
	return st
}

func sortedRoles(a map[string]string, b map[string]any) []string {
	seen := map[string]bool{}
	var out []string
	for k := range a {
		seen[k] = true
		out = append(out, k)
	}
	for k := range b {
		if !seen[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// ptMatches are the employees for whom the last "Match portal codes" proposed a portal employee: creating them
// could make a second record on the portal.
func ptMatches(ctx context.Context, q store.Q) (map[string][]string, error) {
	r, err := q.C("company_setting").FindOne(ctx, bson.M{"_id": 1}, options.FindOne().SetProjection(bson.M{"realtime_match": 1}))
	if err != nil || r == nil {
		return map[string][]string{}, err
	}
	out := map[string][]string{}
	m := asMap(r["realtime_match"])
	if m == nil {
		return out, nil
	}
	for _, p := range rmDecodeProps(m["props"]) {
		if p.Level == rmLevelNone {
			continue
		}
		cands := p.Cands
		if p.Pick != nil {
			cands = append([]rmCand{*p.Pick}, cands...)
		}
		for _, cd := range cands {
			out[p.EmployeeID] = append(out[p.EmployeeID], fmt.Sprintf("%s (code %s, %s)", cd.Name, nonEmptyStr(cd.Code, cd.Card), nonEmptyStr(cd.Branch, cd.Dept)))
		}
	}
	return out, nil
}

func nonEmptyStr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// ptNextCode is the next free number: one more than the highest numeric code among the HRMS's Attendance codes, the
// codes of the portal's last fetched employee list and the codes sent, and at least code_start.
func ptNextCode(ctx context.Context, q store.Q, set bson.M) (string, error) {
	hi := int64(timectxInt(set["code_start"])) - 1
	see := func(s string) {
		if n, err := strconv.ParseInt(rtNorm(s), 10, 64); err == nil && n > hi && n < 1e9 {
			hi = n
		}
	}
	codes, err := q.C("employee").Distinct(ctx, "attendance_code", bson.M{"attendance_code": bson.M{"$nin": bson.A{nil, ""}}})
	if err != nil {
		return "", err
	}
	for _, c := range codes {
		see(str(c))
	}
	sent, err := q.C(store.PortalSendColl).Distinct(ctx, "code", bson.M{"action": "create"})
	if err != nil {
		return "", err
	}
	for _, c := range sent {
		see(str(c))
	}
	// Codes reserved by a create being sent (portal_auto.go) or kept after an unclear reply.
	held, err := q.C("employee").Distinct(ctx, "portal_sync.code", bson.M{"portal_sync.code": bson.M{"$nin": bson.A{nil, ""}}})
	if err != nil {
		return "", err
	}
	for _, c := range held {
		see(str(c))
	}
	if r, _ := q.C("company_setting").FindOne(ctx, bson.M{"_id": 1}, options.FindOne().SetProjection(bson.M{"realtime_match": 1})); r != nil {
		if m := asMap(r["realtime_match"]); m != nil {
			for _, p := range rmDecodeProps(m["props"]) {
				for _, cd := range p.Cands {
					see(cd.Code)
				}
				if p.Pick != nil {
					see(p.Pick.Code)
				}
			}
			for _, cd := range rmDecodeCands(m["leftover"]) {
				see(cd.Code)
			}
		}
	}
	if hi < 0 {
		hi = 0
	}
	return strconv.FormatInt(hi+1, 10), nil
}

// ---------- Routes ----------

func (s *Server) portalWriteRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/attendance/portal", s.a(s.portalWaiting))
	m.HandleFunc("GET /api/attendance/portal/map", s.a(s.portalMapGet))
	m.HandleFunc("PUT /api/attendance/portal/map", s.a(s.portalMapPut))
	m.HandleFunc("POST /api/attendance/portal/lists", s.a(s.portalReadLists))
	m.HandleFunc("PUT /api/attendance/portal/settings", s.a(s.portalSettingsPut))
	m.HandleFunc("GET /api/employees/{id}/portal", s.a(s.portalEmployee))
	m.HandleFunc("POST /api/employees/{id}/portal/retry", s.a(s.portalRetry))
	m.HandleFunc("POST /api/attendance/portal/backlog", s.a(s.portalBacklog))
	m.HandleFunc("POST /api/employees/{id}/portal/resolve", s.a(s.portalResolve))
}

func (s *Server) ptOne(ctx context.Context, c *Ctx, id string) (*ptEmp, error) {
	if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
		return nil, err
	}
	l, err := ptLoad(ctx, s.st.DB, bson.M{"_id": id})
	if err != nil {
		return nil, err
	}
	if len(l) == 0 {
		return nil, notFound("No such employee")
	}
	return l[0], nil
}

func ptReady() (realtime.Config, string) {
	cfg, ok := rtEnv()
	switch {
	case !ok:
		return cfg, "Add the portal account (REALTIME_URL, REALTIME_USER, REALTIME_PASS) to the server first"
	case !realtime.Confirmed():
		return cfg, "Sending is not switched on yet: the portal's employee form has to be confirmed first (README, Sending employees to the portal)"
	}
	return cfg, ""
}

// portalEmployee is the "Realtime portal" card of the employee file.
func (s *Server) portalEmployee(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.portal", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	x, err := s.ptOne(ctx, c, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	m, err := ptLoadMaps(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	set, err := ptSettings(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	matches, err := ptMatches(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	st := ptStateOf(x, m, timectxInt(set["card_pad"]), matches)
	out := map[string]any{"state": st, "name": x.Name, "employeeStatus": x.Status, "lwd": x.LWD, "canSend": c.Level("att.portal") >= 2}
	if _, why := ptReady(); why != "" {
		out["notReady"] = why
	}
	out["auto"] = ptAutoView(x, st, str(set["auto_since"]))
	sends, err := s.st.DB.C(store.PortalSendColl).Find(ctx, bson.M{"employee_id": x.ID}, options.Find().SetSort(bson.D{{Key: "at", Value: -1}}).SetLimit(5).
		SetProjection(bson.M{"action": 1, "code": 1, "ok": 1, "unclear": 1, "message": 1, "by": 1, "at": 1}))
	if err != nil {
		return nil, err
	}
	out["sends"] = sends
	return out, nil
}

func stringMap(m map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ptTickTodo ticks the employee's punch-machine to-do the send has done: enrol (or move) when the branch sent is in
// the cluster they are to be enrolled on, remove after a mark-left.
func ptTickTodo(ctx context.Context, q store.Q, id string, w realtime.Write, by string) (string, error) {
	p, err := mpLoad(ctx, q)
	if err != nil {
		return "", err
	}
	todos, err := mpTodos(ctx, q, p)
	if err != nil {
		return "", err
	}
	for _, t := range todos {
		if t.EmployeeID != id {
			continue
		}
		ok := false
		switch {
		case w.Action == "left":
			ok = t.Kind == "Remove"
		case t.Kind == "Enrol" || t.Kind == "Move":
			br := mpNormBranch(w.Values[realtime.FieldBranch])
			for _, m := range p.machines {
				if br != "" && mpNormBranch(m.Branch) == br && p.locCluster[m.LocID] == t.ToID {
					ok = true
				}
			}
		}
		if !ok {
			return "", nil
		}
		return t.Task, mpDone(ctx, q, t, "portal send", by)
	}
	return "", nil
}

// portalResolve settles a send whose reply was not clear, after HR looked at the portal: on (the code becomes the
// Attendance code) or not on the portal.
func (s *Server) portalResolve(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.portal", 2); err != nil {
		return nil, err
	}
	var b struct {
		On bool `json:"on"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	x, err := s.ptOne(ctx, c, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	if str(x.Sync["state"]) != "check" {
		return nil, bad("Nothing to settle: the last send was clear")
	}
	code, action := str(x.Sync["code"]), str(x.Sync["action"])
	err = s.st.Tx(ctx, func(tx store.Q) error {
		sync := map[string]any{}
		for k, v := range x.Sync {
			sync[k] = v
		}
		set := bson.M{}
		switch {
		case !b.On && action == "create":
			// Not on the portal: the create is not sent again by itself (never retried); HR presses Retry.
			sync["state"], sync["code"] = nil, nil
			for k, v := range ptPhase(action, ptPhaseFailed, "Not on the portal after an unclear reply (checked by "+c.Name+"). Press Retry to create them") {
				sync[k] = v
			}
		case !b.On:
			sync["state"] = "on"
		case action == "left":
			sync["state"] = "left"
		default:
			sync["state"] = "on"
			if action == "create" && x.Code == "" {
				msg, err := attCodeProblem(ctx, tx, code, x.ID)
				if err != nil {
					return err
				}
				if msg != "" {
					return bad("%s", msg)
				}
				set["attendance_code"] = code
			}
		}
		sync["resolved_by"], sync["resolved_at"] = c.Name, time.Now()
		set["portal_sync"] = sync
		if _, err := tx.C("employee").Update(ctx, bson.M{"_id": x.ID}, bson.M{"$set": set}); err != nil {
			return err
		}
		return audit(ctx, tx, c, fmt.Sprintf("Unclear portal send for %s settled: %s", x.Name, map[bool]string{true: "it is on the portal", false: "it is not on the portal"}[b.On]),
			"employee", x.ID, map[string]any{"portal_sync": x.Sync}, map[string]any{"portal_sync": sync, "attendance_code": set["attendance_code"]})
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// ptAutoView is the automatic-sending part of the employee's portal card and of the waiting list: the write due,
// and where it stands (phase: waiting, sending, failed, blocked; "" = nothing pending).
func ptAutoView(x *ptEmp, st ptState, autoSince string) map[string]any {
	return map[string]any{"due": ptDue(x, st, autoSince), "phase": str(x.Sync["phase"]), "action": str(x.Sync["phase_action"]),
		"message": str(x.Sync["phase_msg"]), "at": x.Sync["phase_at"]}
}

// ptKindOf is the waiting-list group of a pending write.
var ptKindOf = map[string]string{ptPhaseWaiting: "Waiting", ptPhaseBlocked: "Held", ptPhaseFailed: "Failed", ptPhaseSending: "Sending"}

// portalWaiting is the waiting list of automatic writes: waiting for sending to be switched on (or for HR's "Send
// waiting"), held by a missing mapping, failed (Retry), being sent, and unclear replies to settle.
func (s *Server) portalWaiting(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.portal", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	q := s.st.DB
	set, err := ptSettings(ctx, q)
	if err != nil {
		return nil, err
	}
	list, err := ptLoad(ctx, q, bson.M{"$or": bson.A{bson.M{"portal_sync.phase": bson.M{"$nin": bson.A{nil, ""}}}, bson.M{"portal_sync.state": "check"}}})
	if err != nil {
		return nil, err
	}
	m, err := ptLoadMaps(ctx, q)
	if err != nil {
		return nil, err
	}
	matches, err := ptMatches(ctx, q)
	if err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	counts := map[string]int{"Waiting": 0, "Held": 0, "Failed": 0, "Sending": 0, "Check": 0}
	for _, x := range list {
		if ok, err := c.InScope(ctx, q, x.ID); err != nil || !ok {
			if err != nil {
				return nil, err
			}
			continue
		}
		st := ptStateOf(x, m, timectxInt(set["card_pad"]), matches)
		kind := ptKindOf[str(x.Sync["phase"])]
		if st.Status == ptCheck {
			kind = "Check"
		}
		if kind == "" {
			continue
		}
		counts[kind]++
		action := str(x.Sync["phase_action"])
		if kind == "Check" {
			action = str(x.Sync["action"])
		}
		rows = append(rows, map[string]any{"employeeId": x.ID, "name": x.Name, "kind": kind, "action": action, "message": nonEmptyStr(str(x.Sync["phase_msg"]), str(x.Sync["message"])),
			"at": nonNil(x.Sync["phase_at"], x.Sync["at"]), "location": x.Loc, "dept": x.Dept, "desig": x.Desig, "doj": x.DOJ, "lwd": x.LWD, "code": st.Code})
	}
	out := map[string]any{"rows": rows, "counts": counts, "since": set["auto_since"], "canSend": c.Level("att.portal") >= 2}
	if _, why := ptReady(); why != "" {
		out["notReady"] = why
	}
	return out, nil
}

func nonNil(a, b any) any {
	if a != nil {
		return a
	}
	return b
}

// portalMapGet: the HRMS departments, designations, properties and shifts with their portal choice, and the portal's
// lists as last read.
func (s *Server) portalMapGet(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.portal", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	q := s.st.DB
	m, err := ptLoadMaps(ctx, q)
	if err != nil {
		return nil, err
	}
	set, err := ptSettings(ctx, q)
	if err != nil {
		return nil, err
	}
	lists := asMap(set["lists"])
	count := func(field string) (map[string]int, error) {
		rows, err := q.C("employee").Aggregate(ctx, bson.A{bson.M{"$match": bson.M{"status": bson.M{"$ne": "Inactive"}}},
			bson.M{"$group": bson.M{"_id": "$" + field, "n": bson.M{"$sum": 1}}}})
		out := map[string]int{}
		for _, r := range rows {
			out[str(r["_id"])] = timectxInt(r["n"])
		}
		return out, err
	}
	kinds := map[string]any{}
	for _, k := range ptKinds {
		var rows []store.Row
		var used map[string]int
		switch k {
		case "dept":
			rows, err = q.C("department").Find(ctx, bson.M{}, options.Find().SetProjection(bson.M{"name": 1}).SetSort(bson.D{{Key: "name", Value: 1}}))
			if err == nil {
				used, err = count("department_id")
			}
		case "desig":
			rows, err = q.C("designation").Find(ctx, bson.M{}, options.Find().SetProjection(bson.M{"name": 1}).SetSort(bson.D{{Key: "name", Value: 1}}))
			if err == nil {
				used, err = count("designation_id")
			}
		case "branch":
			rows, err = q.C("location").Find(ctx, bson.M{}, options.Find().SetProjection(bson.M{"name": 1}).SetSort(bson.D{{Key: "name", Value: 1}}))
			if err == nil {
				used, err = count("location_id")
			}
		case "shift":
			rows, err = q.C("shift").Find(ctx, bson.M{}, options.Find().SetProjection(bson.M{"code": 1, "name": 1}).SetSort(bson.D{{Key: "code", Value: 1}}))
			for _, r := range rows {
				r["id"] = r["code"]
			}
			if err == nil {
				used, err = count("shift_code")
			}
		}
		if err != nil {
			return nil, err
		}
		choices := ptListTexts(lists, k)
		out := []map[string]any{}
		for _, r := range rows {
			ref := str(r["id"])
			if used[ref] == 0 && m[k][ref] == "" {
				continue // nobody active has it: nothing to map
			}
			e := map[string]any{"ref": ref, "name": str(r["name"]), "people": used[ref], "value": m[k][ref]}
			if k == "branch" && m[k][ref] == "" {
				e["machine"] = m["machine"][ref]
			}
			if m[k][ref] == "" && e["machine"] == nil {
				for _, ch := range choices { // the same name on the portal is suggested
					if mpNormBranch(ch) == mpNormBranch(str(r["name"])) {
						e["suggest"] = ch
					}
				}
			}
			out = append(out, e)
		}
		kinds[k] = out
	}
	portal := map[string]any{}
	for _, k := range ptKinds {
		portal[k] = ptListTexts(lists, k)
	}
	return map[string]any{"kinds": kinds, "portal": portal, "listsAt": lists["at"], "codeStart": set["code_start"], "cardPad": set["card_pad"],
		"canEdit": c.Level("att.portal") >= 2}, nil
}

func ptListTexts(lists map[string]any, kind string) []string {
	out := []string{}
	if lists == nil {
		return out
	}
	switch l := lists[kind].(type) {
	case bson.A:
		for _, v := range l {
			out = append(out, str(v))
		}
	case []any:
		for _, v := range l {
			out = append(out, str(v))
		}
	case []string:
		out = append(out, l...)
	}
	return out
}

func (s *Server) portalMapPut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.portal", 2); err != nil {
		return nil, err
	}
	var b struct{ Kind, Ref, Value string }
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Value = strings.TrimSpace(b.Value)
	if ptKindRole[b.Kind] == "" || strings.TrimSpace(b.Ref) == "" {
		return nil, bad("Unknown mapping")
	}
	ctx := r.Context()
	err := s.st.Tx(ctx, func(tx store.Q) error {
		cur, err := tx.C(store.PortalMapColl).FindOne(ctx, bson.M{"kind": b.Kind, "ref": b.Ref})
		if err != nil {
			return err
		}
		var before any
		if cur != nil {
			before = cur["value"]
		}
		if b.Value == "" {
			if _, err := tx.C(store.PortalMapColl).Delete(ctx, bson.M{"kind": b.Kind, "ref": b.Ref}); err != nil {
				return err
			}
		} else if _, err := tx.C(store.PortalMapColl).Upsert(ctx, bson.M{"kind": b.Kind, "ref": b.Ref}, bson.M{"value": b.Value, "updated_by": c.Name, "updated_at": time.Now()}, nil); err != nil {
			return err
		}
		return audit(ctx, tx, c, fmt.Sprintf("Realtime portal mapping: %s %s → %q", ptKindLabel[b.Kind], b.Ref, b.Value), store.PortalMapColl, b.Kind+":"+b.Ref,
			map[string]any{"value": before}, map[string]any{"value": b.Value})
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// portalReadLists reads the portal form's lists (GET only) for the mapping page.
func (s *Server) portalReadLists(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.portal", 2); err != nil {
		return nil, err
	}
	cfg, why := ptReady()
	if why != "" {
		return nil, bad("%s", why)
	}
	ctx := r.Context()
	cl, err := ptClient(cfg)
	if err != nil {
		return nil, bad("%v", err)
	}
	f, err := cl.ReadForm(ctx)
	if err != nil {
		return nil, bad("The portal's employee form could not be read: %v", err)
	}
	lists := map[string]any{"at": time.Now(), "by": c.Name}
	counts := map[string]int{}
	for _, k := range ptKinds {
		texts := []any{}
		if fl := f.FindRole(ptKindRole[k]); fl != nil {
			for _, o := range fl.Options {
				if t := strings.TrimSpace(o.Text); t != "" && !strings.Contains(strings.ToLower(t), "select") && !strings.HasPrefix(t, "--") {
					texts = append(texts, t)
				}
			}
		}
		lists[k] = texts
		counts[k] = len(texts)
	}
	err = s.st.Tx(ctx, func(tx store.Q) error {
		if err := ptSet(ctx, tx, bson.M{"lists": lists}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Realtime portal lists read for the mapping", "company_setting", "portal_write", nil, counts)
	})
	if err != nil {
		return nil, err
	}
	return counts, nil
}

func (s *Server) portalSettingsPut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.portal", 2); err != nil {
		return nil, err
	}
	var b struct {
		CodeStart *int `json:"codeStart"`
		CardPad   *int `json:"cardPad"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	set := bson.M{}
	if b.CodeStart != nil {
		if *b.CodeStart < 0 || *b.CodeStart > 99999999 {
			return nil, bad("The first code is a number up to 99999999")
		}
		set["code_start"] = *b.CodeStart
	}
	if b.CardPad != nil {
		if *b.CardPad < 0 || *b.CardPad > 12 {
			return nil, bad("The card number has 0 to 12 digits")
		}
		set["card_pad"] = *b.CardPad
	}
	ctx := r.Context()
	err := s.st.Tx(ctx, func(tx store.Q) error {
		before, err := ptSettings(ctx, tx)
		if err != nil {
			return err
		}
		if err := ptSet(ctx, tx, set); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Realtime portal send settings changed", "company_setting", "portal_write",
			map[string]any{"code_start": before["code_start"], "card_pad": before["card_pad"]}, set)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}
