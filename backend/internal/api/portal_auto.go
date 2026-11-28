package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/jobs"
	"github.com/saltstayz/hrms/internal/realtime"
	"github.com/saltstayz/hrms/internal/store"
)

// Automatic sending to the Realtime portal (user, 19 Nov 2026; replaces the 17 Nov "Send to portal" button). The
// HRMS writes on its own:
//
//   - Mark joined → create the employee on the portal (the next free code, which becomes the Attendance code);
//   - a change of the details the portal gets (name, department, designation, property/branch, shift, phone…) of
//     someone the HRMS put on the portal → update;
//   - an exit (inactive, or the last working day reached) of someone on the portal → mark left.
//
// Each write is a job on the Mongo queue (PortalWriteArgs), inserted with the joining's transaction or by the
// every-minute sweep (PortalSweepArgs) that notices changes and exits; never inside a request. A job posts once
// and never again by itself (MaxAttempts 1, and a failed write stays failed until HR presses Retry). A refusal, an
// error or a missing mapping goes to the waiting list with the portal's message and a bell to HR; an unclear reply
// is "Check on the portal" as before. Sending is on since 25 Nov 2026 (owner's approval, realtime.Spec.Send); if it
// is switched off (ptReady), the writes due wait (phase "waiting") and go out when HR confirms the backlog ("Send
// waiting") once it is on again. A create also chooses the punch machine (ptDevice, portal_write.go).
//
// The state is kept in employee.portal_sync next to the last send: want ("create", set at Mark joined), phase
// (waiting, sending, failed, blocked; empty when nothing is pending), phase_action, phase_msg, phase_at.
// company_setting.portal_write.auto_since (migration 022) is when automatic sending began: exits before it of
// people the HRMS never sent are not marked left, and only joiners from it are created without a Mark joined.

const (
	ptPhaseWaiting = "waiting"
	ptPhaseSending = "sending"
	ptPhaseFailed  = "failed"
	ptPhaseBlocked = "blocked"
	ptAutoBy       = "HRMS (automatic)"
	portalSweepGap = time.Minute
	ptSendingStale = 20 * time.Minute
)

// ptCreateMu keeps two creates from taking the same next free code.
var ptCreateMu sync.Mutex

// PortalWriteArgs is one automatic write for one employee. Retry: HR pressed Retry (a failed write is tried once
// more). Backlog: HR confirmed the writes that waited while sending was off.
type PortalWriteArgs struct {
	EmployeeID string `json:"employeeId"`
	By         string `json:"by,omitempty"`
	Retry      bool   `json:"retry,omitempty"`
	Backlog    bool   `json:"backlog,omitempty"`
	NotSame    bool   `json:"notSame,omitempty"`
}

func (PortalWriteArgs) Kind() string { return "portal_write" }

func (PortalWriteArgs) InsertOpts() jobs.InsertOpts {
	// One try only: a write is never repeated by the queue. One waiting job per employee and request.
	return jobs.InsertOpts{MaxAttempts: 1, UniqueOpts: jobs.UniqueOpts{ByArgs: true,
		ByState: []jobs.JobState{jobs.JobStateAvailable, jobs.JobStatePending, jobs.JobStateRunning, jobs.JobStateScheduled}}}
}

type portalWriteWorker struct {
	jobs.WorkerDefaults[PortalWriteArgs]
	s *Server
}

func (w *portalWriteWorker) Timeout(*jobs.Job[PortalWriteArgs]) time.Duration { return 3 * time.Minute }

func (w *portalWriteWorker) Work(ctx context.Context, j *jobs.Job[PortalWriteArgs]) error {
	if _, err := w.s.PortalAutoRun(ctx, j.Args); err != nil {
		log.Printf("portal write %s: %v", j.Args.EmployeeID, err) // recorded on the employee when it could be; never retried
	}
	return nil
}

// PortalSweepArgs is the every-minute look for writes due (changes and exits).
type PortalSweepArgs struct{}

func (PortalSweepArgs) Kind() string { return "portal_sweep" }

func (PortalSweepArgs) InsertOpts() jobs.InsertOpts {
	return jobs.InsertOpts{UniqueOpts: jobs.UniqueOpts{ByPeriod: portalSweepGap}}
}

type portalSweepWorker struct {
	jobs.WorkerDefaults[PortalSweepArgs]
	s *Server
}

func (w *portalSweepWorker) Work(ctx context.Context, _ *jobs.Job[PortalSweepArgs]) error {
	if _, err := w.s.PortalSweep(ctx); err != nil {
		log.Printf("portal sweep: %v", err)
	}
	return nil
}

// ptEnqueueTx queues the automatic write for id in the caller's transaction (it runs once that commits). Without a
// job queue (some tests, a CLI) the every-minute sweep picks it up instead.
func (s *Server) ptEnqueueTx(ctx context.Context, tx store.Q, id string) error {
	if s.jobs == nil {
		return nil
	}
	_, err := s.jobs.InsertTx(ctx, tx, PortalWriteArgs{EmployeeID: id}, nil)
	return err
}

// ptWantCreate marks a new joiner to be created on the portal (Mark joined, hiring.go).
func ptWantCreate(ctx context.Context, tx store.Q, id string) error {
	e, err := tx.C("employee").FindOne(ctx, bson.M{"_id": id})
	if err != nil || e == nil {
		return err
	}
	sync := map[string]any{}
	for k, v := range asMap(e["portal_sync"]) {
		sync[k] = v
	}
	if st := str(sync["state"]); (st != "" && st != ptRemoved) || strings.TrimSpace(str(e["attendance_code"])) != "" {
		return nil // already on the portal (a rejoiner keeping their code): nothing to create
	}
	sync["want"] = "create"
	delete(sync, "state")
	_, err = tx.C("employee").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"portal_sync": sync}})
	return err
}

// ptDue is the write due for x now ("" for none): create for a joiner not on the portal, mark left after the exit
// of someone on it, update when the details sent have changed.
func ptDue(x *ptEmp, st ptState, autoSince string) string {
	state := str(x.Sync["state"])
	if state == "check" || state == "left" {
		return ""
	}
	has := func(a string) bool {
		for _, v := range st.Actions {
			if v == a {
				return true
			}
		}
		return false
	}
	switch st.Status {
	case ptNotOn:
		if has("create") && (str(x.Sync["want"]) == "create" || (state == "" && autoSince != "" && x.DOJ >= autoSince)) {
			return "create"
		}
	case ptOn, ptChanged:
		if st.ExitDue {
			if state == "on" || (autoSince != "" && x.LWD != "" && x.LWD >= autoSince) {
				return "left"
			}
			return ""
		}
		if st.Status == ptChanged && has("update") {
			return "update"
		}
	}
	return ""
}

// ptSaveSync replaces the employee's portal_sync with x.Sync changed by set (a nil value removes the key).
func ptSaveSync(ctx context.Context, q store.Q, x *ptEmp, set map[string]any) error {
	sync := map[string]any{}
	for k, v := range x.Sync {
		sync[k] = v
	}
	for k, v := range set {
		if v == nil {
			delete(sync, k)
		} else {
			sync[k] = v
		}
	}
	x.Sync = sync
	_, err := q.C("employee").Update(ctx, bson.M{"_id": x.ID}, bson.M{"$set": bson.M{"portal_sync": sync}})
	return err
}

func ptPhase(action, phase, msg string) map[string]any {
	if phase == "" {
		return map[string]any{"phase": nil, "phase_action": nil, "phase_msg": nil, "phase_at": nil}
	}
	return map[string]any{"phase": phase, "phase_action": action, "phase_msg": msg, "phase_at": time.Now()}
}

var ptVerb = map[string]string{"create": "Create on the portal", "update": "Update on the portal", "left": "Mark left on the portal"}

// ptBell tells HR (HR Admin and the holders of att.portal at Edit) once per employee, write and message.
func ptBell(ctx context.Context, q store.Q, x *ptEmp, action, text string) {
	to, err := hstRecipients(ctx, q, "att.portal")
	if err != nil {
		log.Printf("portal bell: %v", err)
		return
	}
	key := "portal:" + x.ID + ":" + action + ":" + ptHash(map[string]string{"m": text})
	for who, view := range to {
		if _, err := noticeOnce(ctx, q, key+":"+who, who, text, "/file?id="+x.ID, view); err != nil {
			log.Printf("portal bell: %v", err)
		}
	}
}

// PortalAutoRun makes the write due for one employee, once. It returns what happened, for the log and the tests.
func (s *Server) PortalAutoRun(ctx context.Context, a PortalWriteArgs) (string, error) {
	q := s.st.DB
	list, err := ptLoad(ctx, q, bson.M{"$or": bson.A{bson.M{"_id": a.EmployeeID}, bson.M{"alias_ids": a.EmployeeID}}})
	if err != nil || len(list) == 0 {
		return "no such employee", err
	}
	x := list[0]
	if _, busy := ptBusy.LoadOrStore(x.ID, true); busy {
		return "busy", nil
	}
	defer ptBusy.Delete(x.ID)
	m, err := ptLoadMaps(ctx, q)
	if err != nil {
		return "", err
	}
	set, err := ptSettings(ctx, q)
	if err != nil {
		return "", err
	}
	matches, err := ptMatches(ctx, q)
	if err != nil {
		return "", err
	}
	pad := timectxInt(set["card_pad"])
	st := ptStateOf(x, m, pad, matches)
	action := ptDue(x, st, str(set["auto_since"]))
	phase, phaseAct := str(x.Sync["phase"]), str(x.Sync["phase_action"])
	if action == "" {
		if phase != "" && phase != ptPhaseSending {
			return "nothing due", ptSaveSync(ctx, q, x, ptPhase("", "", ""))
		}
		return "nothing due", nil
	}
	switch {
	case phase == ptPhaseSending:
		return "already sending", nil
	case phase == ptPhaseFailed && phaseAct == action && !a.Retry:
		return "failed: waits for Retry", nil // never retried by itself
	case phase == ptPhaseWaiting && !a.Backlog && !a.Retry:
		if _, why := ptReady(); why != "" {
			return "waiting", nil
		}
		return "waiting for HR to send the backlog", nil
	}
	cfg, why := ptReady()
	if why != "" {
		return "waiting", ptSaveSync(ctx, q, x, ptPhase(action, ptPhaseWaiting, why))
	}
	by := ptAutoBy
	if a.By != "" {
		by = a.By + " (" + map[bool]string{true: "Retry", false: "Send waiting"}[a.Retry] + ")"
	}
	w := realtime.Write{Action: action, Code: st.Code}
	block := []string{}
	notSame := a.NotSame || x.Sync["not_same"] == true
	if action == "create" {
		ptCreateMu.Lock()
		defer ptCreateMu.Unlock()
		// The portal's employee code is the number of the person's ID, 6 digits (owner, 25 Nov 2026): JI-000006 →
		// 000006, EMP-000123 → 000123. An ID with no digits takes the next free number as before.
		code := ptIDCode(x.ID)
		if code == "" {
			if code, err = ptNextCode(ctx, q, set); err != nil {
				return "", err
			}
		}
		if msg, err := attCodeProblem(ctx, q, code, x.ID); err != nil {
			return "", err
		} else if msg != "" {
			block = append(block, msg+": set "+x.Name+"'s Attendance code by hand (Employees) if they are already on the portal")
		}
		if other, err := ptCodeHeld(ctx, q, code, x.ID); err != nil {
			return "", err
		} else if other != "" {
			block = append(block, "Portal code "+code+" is being sent for "+other)
		}
		if len(st.Matches) > 0 && !notSame {
			block = append(block, "The portal may already have "+x.Name+": Match portal codes proposed "+strings.Join(st.Matches, "; ")+
				". Apply that code (Attendance › Punch machine setup), or press Retry if they are someone else")
		}
		w.Code = code
	}
	switch action {
	case "create", "update":
		vals, b := ptValues(x, m, w.Code, pad)
		block = append(block, b...)
		if action == "update" {
			delete(vals, realtime.FieldCode) // the code and card stay as the portal has them
			delete(vals, realtime.FieldCard)
		}
		if action == "create" { // the punch machine the machine pool plans (owner, 25 Nov 2026); never on update or left
			serial, note, err := ptDevice(ctx, q, x)
			if err != nil {
				return "", err
			}
			if serial != "" {
				vals[realtime.FieldDevice] = serial
			} else {
				w.Notes = append(w.Notes, realtime.DeviceNote(note))
			}
		}
		w.Values = vals
	case "left":
		w.Values = map[string]string{realtime.FieldLeft: st.LeftOn}
	}
	if len(block) > 0 {
		msg := strings.Join(block, "; ")
		if phase != ptPhaseBlocked || str(x.Sync["phase_msg"]) != msg {
			ptBell(ctx, q, x, action, fmt.Sprintf("Realtime portal: %s for %s (%s) is held. %s", strings.ToLower(ptVerb[action]), x.Name, x.ID, msg))
		}
		return "blocked", ptSaveSync(ctx, q, x, ptPhase(action, ptPhaseBlocked, msg))
	}
	// Sending: the code is reserved (ptNextCode reads it) before the post.
	sending := ptPhase(action, ptPhaseSending, "")
	if a.NotSame {
		sending["not_same"] = true
	}
	if action == "create" {
		sending["code"] = w.Code
	}
	if err := ptSaveSync(ctx, q, x, sending); err != nil {
		return "", err
	}
	fail := func(msg string) (string, error) {
		ptBell(ctx, q, x, action, fmt.Sprintf("Realtime portal: %s for %s (%s) failed: %s. Press Retry on the employee file once it is fixed.", strings.ToLower(ptVerb[action]), x.Name, x.ID, msg))
		set := ptPhase(action, ptPhaseFailed, msg)
		if action == "create" && str(x.Sync["state"]) == "" {
			set["code"] = nil // the reserved code was not used
		}
		return "failed", ptSaveSync(ctx, q, x, set)
	}
	cl, err := ptClient(cfg)
	if err != nil {
		return fail(err.Error())
	}
	res, err := cl.Post(ctx, w)
	if err != nil { // nothing was posted
		return fail("not sent: " + err.Error())
	}
	sentVals, _ := ptValues(x, m, w.Code, pad)
	ticked, err := s.ptRecord(context.WithoutCancel(ctx), nil, x, w, res, sentVals, by)
	if err != nil {
		log.Printf("portal write %s %s: recorded failed: %v (portal said %q)", w.Action, x.ID, err, res.Message)
		_ = ptSaveSync(context.WithoutCancel(ctx), q, x, ptPhase(action, ptPhaseFailed, fmt.Sprintf("The portal answered (%q) but the HRMS could not record it. Check the portal before Retry", res.Message)))
		return "", err
	}
	switch {
	case res.Unclear:
		ptBell(ctx, q, x, action, fmt.Sprintf("Realtime portal: the reply to %s for %s (%s) was not clear (%s). Look them up on the portal and settle it on the employee file.",
			strings.ToLower(ptVerb[action]), x.Name, x.ID, res.Message))
		return "unclear", nil
	case !res.OK:
		return fail("the portal refused it: " + res.Message)
	}
	if ticked != "" {
		return "saved; to-do ticked: " + ticked, nil
	}
	return "saved", nil
}

// ptDevice is the serial number of the punch machine a new employee's record goes on (the machine pool's plan,
// mpPlan: their own property's machine first, then the next machine of the cluster with space), or why there is
// none: their property has no cluster, the cluster has no machine mapped, the cluster is full, or the machine has no
// serial number. None is not a problem: the create goes without a device and HR adds them to a machine on the portal.
func ptDevice(ctx context.Context, q store.Q, x *ptEmp) (serial, why string, err error) {
	p, err := mpLoad(ctx, q)
	if err != nil {
		return "", "", err
	}
	clID := p.locCluster[x.LocID]
	cl := p.clusters[clID]
	switch {
	case x.LocID == "":
		return "", "they have no property", nil
	case cl == nil:
		return "", "their property " + nonEmptyStr(x.Loc, x.LocID) + " is in no cluster", nil
	case len(cl.Machines) == 0:
		return "", "no punch machine is mapped to cluster " + cl.Name, nil
	}
	m := p.assign[x.ID]
	if _, counted := p.people[x.ID]; !counted && m == nil { // not active yet: placed after everyone already there
		ms := append([]*mpMachine(nil), cl.Machines...)
		ps := append(append([]mpPerson(nil), cl.People...), mpPerson{ID: x.ID, Name: x.Name, LocID: x.LocID, DOJ: "9999-12-31"})
		a, _, _ := mpPlan(ms, ps)
		m = a[x.ID]
	}
	switch {
	case m == nil:
		return "", fmt.Sprintf("cluster %s is full (%d records on its machines)", cl.Name, cl.Cap), nil
	case strings.TrimSpace(m.Serial) == "":
		return "", "machine " + m.Name + " has no serial number", nil
	}
	return strings.TrimSpace(m.Serial), "", nil
}

// ptSentDevice is the punch machine a post chose (the device row as sent), "" when none.
func ptSentDevice(rows []realtime.PlanRow) string {
	for _, r := range rows {
		if r.Role == realtime.FieldDevice {
			return nonEmptyStr(r.Shown, r.Value)
		}
	}
	return ""
}

// ptRecord keeps what a post did, whatever it was (the post is done and is not repeated): the send log, the
// employee's portal state (and Attendance code after a create), the audit with the fields sent and the reply, and
// the punch-machine to-do. c is nil for an automatic write.
func (s *Server) ptRecord(ctx context.Context, c *Ctx, x *ptEmp, w realtime.Write, res *realtime.Result, sentVals map[string]string, by string) (string, error) {
	now := time.Now()
	ticked := ""
	err := s.st.Tx(ctx, func(tx store.Q) error {
		ticked = ""
		device := ptSentDevice(res.Sent)
		rec := bson.M{"employee_id": x.ID, "action": w.Action, "code": w.Code, "ok": res.OK, "unclear": res.Unclear,
			"sent": res.Sent, "message": timectxNullIf(res.Message), "by": by, "at": now, "device": timectxNullIf(device)}
		if len(res.Notes) > 0 {
			rec["notes"] = res.Notes
		}
		if _, err := tx.C(store.PortalSendColl).Insert(ctx, rec); err != nil {
			return err
		}
		before := map[string]any{"attendance_code": timectxNullIf(x.Code), "portal_sync": x.Sync}
		sync := map[string]any{}
		for k, v := range x.Sync {
			sync[k] = v
		}
		for k := range ptPhase("", "", "") {
			delete(sync, k)
		}
		sync["action"], sync["at"], sync["by"], sync["message"] = w.Action, now, by, res.Message
		set := bson.M{}
		switch {
		case res.Unclear:
			sync["state"], sync["code"] = "check", w.Code
		case !res.OK:
			// Refused: the state stays as it was; the reply is kept on the card.
			sync["refused"] = true
			if sync["state"] == nil && w.Action == "create" {
				delete(sync, "code") // the reserved code was not taken
			}
		case w.Action == "left":
			sync["state"], sync["left_on"] = "left", w.Values[realtime.FieldLeft]
		default:
			sync["state"], sync["code"], sync["hash"], sync["values"] = "on", w.Code, ptHash(sentVals), stringMap(sentVals)
			delete(sync, "want")
			if w.Action == "create" {
				set["attendance_code"] = w.Code
			}
		}
		if res.OK || res.Unclear {
			delete(sync, "refused")
		}
		set["portal_sync"] = sync
		if _, err := tx.C("employee").Update(ctx, bson.M{"_id": x.ID}, bson.M{"$set": set}); err != nil {
			return err
		}
		x.Sync = sync
		verb := map[string]string{"create": "created on", "update": "details sent to", "left": "marked left on"}[w.Action]
		outcome := "the portal saved it"
		if res.Unclear {
			outcome = "the portal's reply was not clear"
		} else if !res.OK {
			outcome = "the portal refused it"
		}
		onMachine := ""
		if device != "" {
			onMachine = ", punch machine " + device
		}
		after := map[string]any{"action": w.Action, "code": w.Code, "sent": res.Sent, "reply": res.Message, "ok": res.OK, "unclear": res.Unclear, "device": timectxNullIf(device)}
		if len(res.Notes) > 0 {
			after["notes"] = res.Notes
		}
		if err := audit(ctx, tx, c, fmt.Sprintf("%s %s the Realtime portal (code %s%s) by %s: %s", x.Name, verb, w.Code, onMachine, by, outcome), "employee", x.ID, before,
			after); err != nil {
			return err
		}
		if res.OK {
			t, err := ptTickTodo(ctx, tx, x.ID, w, by)
			if err != nil {
				return err
			}
			ticked = t
		}
		return nil
	})
	return ticked, err
}

// PortalSweep looks for writes due (joiners, changes, exits) and queues them; with sending off it marks them
// waiting. A failed write is left for Retry; a blocked one is checked again and queued once the block is gone.
// It returns how many writes it queued.
func (s *Server) PortalSweep(ctx context.Context) (int, error) {
	q := s.st.DB
	set, err := ptSettings(ctx, q)
	if err != nil {
		return 0, err
	}
	since := str(set["auto_since"])
	or := bson.A{
		bson.M{"portal_sync.want": "create"},
		bson.M{"portal_sync.phase": bson.M{"$nin": bson.A{nil, ""}}},
		bson.M{"portal_sync.state": "on"},
	}
	if since != "" {
		or = append(or, bson.M{"status": "Active", "attendance_code": bson.M{"$in": bson.A{nil, ""}}, "doj": bson.M{"$gte": since}},
			bson.M{"attendance_code": bson.M{"$nin": bson.A{nil, ""}}, "portal_sync.state": bson.M{"$nin": bson.A{"left", "check"}}, "lwd": bson.M{"$gte": since}})
	}
	list, err := ptLoad(ctx, q, bson.M{"$or": or})
	if err != nil || len(list) == 0 {
		return 0, err
	}
	m, err := ptLoadMaps(ctx, q)
	if err != nil {
		return 0, err
	}
	matches, err := ptMatches(ctx, q)
	if err != nil {
		return 0, err
	}
	_, why := ptReady()
	pad := timectxInt(set["card_pad"])
	queued := 0
	for _, x := range list {
		if _, busy := ptBusy.Load(x.ID); busy {
			continue
		}
		st := ptStateOf(x, m, pad, matches)
		action := ptDue(x, st, since)
		phase, phaseAct := str(x.Sync["phase"]), str(x.Sync["phase_action"])
		at, _ := x.Sync["phase_at"].(time.Time)
		if dt, ok := x.Sync["phase_at"].(bson.DateTime); ok {
			at = dt.Time()
		}
		switch {
		case action == "" && phase == ptPhaseSending && time.Since(at) < ptSendingStale:
		case action == "":
			if phase != "" {
				if err := ptSaveSync(ctx, q, x, ptPhase("", "", "")); err != nil {
					return queued, err
				}
			}
		case phase == ptPhaseSending:
			if time.Since(at) > ptSendingStale { // the server stopped mid-send: whether it reached the portal is unknown
				msg := "The send stopped before the portal answered. Look the person up on the portal before you press Retry"
				ptBell(ctx, q, x, action, fmt.Sprintf("Realtime portal: %s for %s (%s): %s.", strings.ToLower(ptVerb[action]), x.Name, x.ID, msg))
				if err := ptSaveSync(ctx, q, x, ptPhase(action, ptPhaseFailed, msg)); err != nil {
					return queued, err
				}
			}
		case phase == ptPhaseFailed && phaseAct == action, phase == ptPhaseWaiting:
			// Failed: only HR's Retry. Waiting: only HR's "Send waiting" once sending is on.
		case why != "":
			if err := ptSaveSync(ctx, q, x, ptPhase(action, ptPhaseWaiting, why)); err != nil {
				return queued, err
			}
		default:
			if phase == ptPhaseBlocked && action != "left" && phaseAct == action {
				_, block := ptValues(x, m, st.Code, pad)
				if len(block) > 0 || (action == "create" && len(st.Matches) > 0 && x.Sync["not_same"] != true) {
					continue // still blocked: checked again next minute
				}
			}
			if s.jobs == nil {
				continue
			}
			if _, err := s.jobs.Insert(ctx, PortalWriteArgs{EmployeeID: x.ID}, nil); err != nil {
				return queued, err
			}
			queued++
		}
	}
	return queued, nil
}

// portalRetry (HR) tries a failed or held write once more; it runs as a job.
func (s *Server) portalRetry(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.portal", 2); err != nil {
		return nil, err
	}
	var b struct {
		NotSame bool `json:"notSame"`
	}
	_ = decode(r, &b)
	ctx := r.Context()
	x, err := s.ptOne(ctx, c, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	if _, why := ptReady(); why != "" {
		return nil, bad("%s", why)
	}
	phase := str(x.Sync["phase"])
	if phase != ptPhaseFailed && phase != ptPhaseBlocked && phase != ptPhaseWaiting {
		return nil, bad("Nothing to retry for %s", x.Name)
	}
	if s.jobs == nil {
		return nil, fmt.Errorf("the job queue is not running")
	}
	err = s.st.Tx(ctx, func(tx store.Q) error {
		if _, err := s.jobs.InsertTx(ctx, tx, PortalWriteArgs{EmployeeID: x.ID, By: c.Name, Retry: true, NotSame: b.NotSame}, nil); err != nil {
			return err
		}
		return audit(ctx, tx, c, fmt.Sprintf("Retry of the Realtime portal write for %s (%s, %s)", x.Name, str(x.Sync["phase_action"]), phase), "employee", x.ID,
			map[string]any{"phase": phase, "message": x.Sync["phase_msg"]}, map[string]any{"notSame": b.NotSame})
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"queued": true}, nil
}

// portalCreate (HR) creates on the portal an active employee who is not on it and was never sent: someone marked
// joined before automatic sending began (19 Nov 2026), so no create was ever due. One job, as at Mark joined.
func (s *Server) portalCreate(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.portal", 2); err != nil {
		return nil, err
	}
	ctx := r.Context()
	x, err := s.ptOne(ctx, c, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	if _, why := ptReady(); why != "" {
		return nil, bad("%s", why)
	}
	if x.Status != "Active" {
		return nil, bad("%s is not active", x.Name)
	}
	if st := str(x.Sync["state"]); (st != "" && st != ptRemoved) || str(x.Sync["phase"]) != "" || strings.TrimSpace(x.Code) != "" {
		return nil, bad("%s already has a portal code or a write pending", x.Name)
	}
	if s.jobs == nil {
		return nil, fmt.Errorf("the job queue is not running")
	}
	err = s.st.Tx(ctx, func(tx store.Q) error {
		if err := ptWantCreate(ctx, tx, x.ID); err != nil {
			return err
		}
		if _, err := s.jobs.InsertTx(ctx, tx, PortalWriteArgs{EmployeeID: x.ID, By: c.Name}, nil); err != nil {
			return err
		}
		return audit(ctx, tx, c, fmt.Sprintf("Create on the Realtime portal asked for %s", x.Name), "employee", x.ID, nil, map[string]any{"want": "create"})
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"queued": true}, nil
}

// ptRemoved is portal_sync.state after HR said the employee was removed (deleted) on the portal by hand.
const ptRemoved = "removed"

// ptFetchActive reads the portal's active employee list (GET). Tests replace it.
var ptFetchActive = func(ctx context.Context, cfg realtime.Config) ([]realtime.ListRow, error) {
	c, err := realtime.New(cfg)
	if err != nil {
		return nil, err
	}
	return c.ActiveList(ctx)
}

// portalRemoved (HR): the employee was deleted on the portal by hand. The HRMS checks the portal's list (GET) that
// the code is really gone, then forgets the portal record (Attendance code and portal_sync), so "Create on the
// portal" is offered again. Nothing is created by itself afterwards (state "removed": no automatic create).
func (s *Server) portalRemoved(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.portal", 2); err != nil {
		return nil, err
	}
	ctx := r.Context()
	x, err := s.ptOne(ctx, c, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	code := strings.TrimSpace(x.Code)
	if code == "" {
		code = str(x.Sync["code"])
	}
	if code == "" {
		return nil, bad("%s has no portal code", x.Name)
	}
	cfg, ok := rtEnv()
	if !ok {
		return nil, bad("Add the portal account (REALTIME_URL, REALTIME_USER, REALTIME_PASS) to the server first")
	}
	rows, err := ptFetchActive(ctx, cfg)
	if err != nil {
		return nil, bad("The portal's employee list could not be read: %v", err)
	}
	if row := realtime.FindCode(rows, code); row != nil {
		return nil, bad("Still on the portal: %s, code %s. Delete or remove them there first", row.Name, row.Code)
	}
	err = s.st.Tx(ctx, func(tx store.Q) error {
		sync := map[string]any{"state": ptRemoved, "removed_code": code, "removed_by": c.Name, "removed_at": time.Now()}
		if _, err := tx.C("employee").Update(ctx, bson.M{"_id": x.ID}, bson.M{"$set": bson.M{"portal_sync": sync, "attendance_code": nil}}); err != nil {
			return err
		}
		return audit(ctx, tx, c, fmt.Sprintf("%s was removed from the Realtime portal (code %s, checked on the portal's list)", x.Name, code), "employee", x.ID,
			map[string]any{"attendance_code": x.Code, "portal_sync": x.Sync}, map[string]any{"attendance_code": nil, "portal_sync": sync})
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// portalBacklog (HR) sends every write that waited while sending was off, once it is on: one job each.
func (s *Server) portalBacklog(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.portal", 2); err != nil {
		return nil, err
	}
	if _, why := ptReady(); why != "" {
		return nil, bad("%s", why)
	}
	if s.jobs == nil {
		return nil, fmt.Errorf("the job queue is not running")
	}
	ctx := r.Context()
	list, err := ptLoad(ctx, s.st.DB, bson.M{"portal_sync.phase": ptPhaseWaiting})
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, x := range list {
		if ok, err := c.InScope(ctx, s.st.DB, x.ID); err != nil {
			return nil, err
		} else if ok {
			ids = append(ids, x.ID)
		}
	}
	if len(ids) == 0 {
		return nil, bad("Nothing is waiting")
	}
	err = s.st.Tx(ctx, func(tx store.Q) error {
		for _, id := range ids {
			if _, err := s.jobs.InsertTx(ctx, tx, PortalWriteArgs{EmployeeID: id, By: c.Name, Backlog: true}, nil); err != nil {
				return err
			}
		}
		return audit(ctx, tx, c, fmt.Sprintf("Realtime portal: %d waiting write%s sent", len(ids), map[bool]string{true: "", false: "s"}[len(ids) == 1]),
			"company_setting", "portal_write", nil, map[string]any{"employees": ids})
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"queued": len(ids)}, nil
}
