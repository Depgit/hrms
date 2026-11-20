package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

// The employee payroll file (prototype p14 readiness): one place per person with readiness for
// the payroll month, the pay row, upcoming lines and what is waiting on whom.

type readyItem struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	State string `json:"state"` // ready | pending | issue | hold | na
	Text  string `json:"text"`
	Owner string `json:"owner"`
}

func (s *Server) employeeFile(r *http.Request, c *Ctx) (any, error) {
	// Prototype: anyone who can see the employee directory opens the file (outside their own My view); pay
	// figures are shown only to roles with salary or payroll rights, as the prototype's sv('salary', …) does.
	if c.Level("emp.directory") < 1 && c.Level("pay.run") < 1 && c.Level("emp.salary") < 1 {
		return nil, forbidden("")
	}
	ctx, id := r.Context(), r.PathValue("id")
	if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
		return nil, err
	}
	ym := r.URL.Query().Get("ym")
	if !isYm(ym) {
		ym = openYm(ctx, s.st.DB)
	}
	e, err := loadEmp(ctx, s.st.DB, id)
	if err != nil {
		return nil, err
	}
	m, err := loadPayMonth(ctx, s.st.DB, ym, false)
	if err != nil {
		return nil, err
	}
	locked := m.LockedAt != nil || m.Status == "Approved" || m.Status == "Published"
	row, err := s.previewRow(ctx, s.st.DB, ym, id)
	if err != nil {
		return nil, err
	}
	items := []readyItem{}
	// Personal details and pending changes.
	have := empfileDetails(ctx, s.st.DB, id)
	need := []string{}
	for _, k := range []string{"mobile", "email", "pan", "aadhaar", "dob"} {
		if have[k] == "" {
			need = append(need, map[string]string{"mobile": "mobile", "email": "email", "pan": "PAN", "aadhaar": "Aadhaar", "dob": "date of birth"}[k])
		}
	}
	// Change requests waiting for Payroll to verify, by kind.
	pending := func(kind string) int64 {
		n, _ := s.st.DB.C("approval_request").Count(ctx, bson.M{"wf": "p_change", "status": "Pending", "employee_id": id, "payload.kind": kind})
		return n
	}
	pendPersonal, pendBank := pending("Personal"), pending("Bank")
	it := readyItem{Key: "personal", Title: "Personal details", State: "ready", Text: "Complete", Owner: "HR"}
	if len(need) > 0 {
		it.State, it.Text = "issue", "Missing: "+strings.Join(need, ", ")
	} else if pendPersonal > 0 {
		it.State, it.Text, it.Owner = "pending", "A change is waiting for Payroll to verify", "Payroll"
	}
	items = append(items, it)
	bank, _ := s.st.DB.C("employee_bank").FindOne(ctx, bson.M{"employee_id": id, "from_month": bson.M{"$lte": ym}},
		options.FindOne().SetSort(bson.D{{Key: "from_month", Value: -1}, {Key: "_id", Value: -1}}))
	it = readyItem{Key: "bank", Title: "Bank details", State: "ready", Text: "Verified account in use", Owner: "Payroll"}
	if pendBank > 0 {
		it.State, it.Text = "hold", "New account waiting for verification. Salary is held until Payroll verifies it"
	} else if bank == nil {
		it.State, it.Text = "issue", "No bank account on record"
	}
	items = append(items, it)
	// A hold covers its starting month and later ones, as in the payroll run (one placed from the payroll checklist
	// only the months up to its to_ym).
	if h, _ := s.st.DB.C("salary_hold").FindOne(ctx, bson.M{"employee_id": id, "status": "Active"}, options.FindOne().SetSort(bson.D{{Key: "from_ym", Value: 1}, {Key: "_id", Value: 1}})); h != nil {
		from, through := h["from_ym"].(string), str(h["to_ym"])
		if holdCovers(from, through, ym) {
			since := " since " + ymLabel(from)
			if through != "" {
				since = " for " + holdMonths(from, through)
			}
			items = append(items, readyItem{"hold", "Salary hold", "hold", h["reason"].(string) + since + ". Net pay is left out of the bank file until released", "Payroll"})
		} else if from <= ym {
			items = append(items, readyItem{"hold", "Salary hold", "ready", h["id"].(string) + " holds the salary for " + holdMonths(from, through) + ". " + ymLabel(ym) + " is paid as usual", "Payroll"})
		} else {
			items = append(items, readyItem{"hold", "Salary hold", "ready", h["id"].(string) + " starts in " + ymLabel(from) + ". " + ymLabel(ym) + " is paid as usual", "Payroll"})
		}
	}
	items = append(items, s.taxReadiness(ctx, e))
	pend, _ := s.pendingFor(ctx, s.st.DB, ym, []string{id})
	it = readyItem{Key: "approvals", Title: "Attendance and leave requests", State: "ready", Text: "Nothing pending", Owner: "—"}
	if len(pend) > 0 {
		who := []string{}
		for _, x := range pend {
			if x.Approver != "" && !containsStr(who, x.Approver) {
				who = append(who, x.Approver)
			}
		}
		it.State, it.Text, it.Owner = "pending", itoa(len(pend))+" pending, with "+strings.Join(who, ", "), "Approvers"
	}
	items = append(items, it)
	if ack := s.ackReadiness(ctx, id, ym); ack != nil { // company policies (policy_ack.go)
		items = append(items, *ack)
	}
	if row != nil {
		it = readyItem{Key: "final", Title: "Final attendance", State: "ready", Owner: "Payroll"}
		txt := []string{}
		head := "Locked. "
		if !locked {
			head = "Not locked yet. "
			it.State = "pending"
		}
		txt = append(txt, head+trimNum(row.Paid)+" paid days of "+itoa(row.Days))
		if row.LOP > 0 {
			txt = append(txt, trimNum(row.LOP)+" loss of pay")
		}
		if row.Edited {
			txt = append(txt, "edited by Payroll")
		}
		if n := row.Counts["Missing punch"]; n > 0 {
			txt = append(txt, itoa(n)+" missing punch")
			if !locked {
				it.State = "issue"
			}
		}
		it.Text = strings.Join(txt, ", ")
		items = append(items, it)
	}
	overall := []string{"Ready to pay", "ok"}
	for _, st := range []struct{ s, label, tone string }{{"hold", "On hold", "bad"}, {"issue", "Needs attention", "bad"}, {"pending", "Waiting", "warn"}} {
		n := 0
		for _, x := range items {
			if x.State == st.s {
				n++
			}
		}
		if n > 0 {
			overall = []string{st.label, st.tone}
			if st.s == "pending" {
				overall[0] = "Waiting on " + itoa(n)
			}
			break
		}
	}
	lines, reqs, slips := empfileLists(ctx, s.st.DB, id, ym)
	for _, r := range reqs { // a bank change's account follows the viewer's bank right (PPL-043)
		tmp := store.Row{"employee_id": id, "payload": r["payload"]}
		maskRequestPayload(c, tmp)
		r["payload"] = tmp["payload"]
	}
	// Pay figures go out only to roles that see salary in full (Payroll, HR Admin, CHRO …); a Masked or Hidden
	// salary group (HRBP, line managers) gets the readiness items without amounts, as the prototype's sv('salary').
	if id != c.EmployeeID && c.Sensitive["salary"] != access.Full {
		row, lines, slips = nil, []map[string]any{}, []map[string]any{}
	}
	return map[string]any{"ym": ym, "employee": e, "items": items, "overall": overall, "row": row, "lines": lines, "requests": reqs, "payslips": slips, "locked": locked}, nil
}

func trimNum(v float64) string { return strconv.FormatFloat(calc.Round2(v), 'f', -1, 64) }

// empfileDetails are the personal and ID details the payroll file needs ("" when missing): mobile, email, dob, pan,
// aadhaar, uan and esic.
func empfileDetails(ctx context.Context, q store.Q, id string) map[string]string {
	out := map[string]string{}
	if p, _ := q.C("employee_personal").FindOne(ctx, bson.M{"employee_id": id}); p != nil {
		for _, k := range []string{"mobile", "email", "dob"} {
			out[k] = str(p[k])
		}
	}
	if g, _ := q.C("employee_gov_id").FindOne(ctx, bson.M{"employee_id": id}); g != nil {
		for _, k := range []string{"pan", "aadhaar", "uan", "esic"} {
			out[k] = str(g[k])
		}
	}
	return out
}

// empfileLists are the file's tables: pay lines from the month on (not void), the last 30 requests about the person
// (newest first, with the name of who raised each) and the last 12 payslips.
func empfileLists(ctx context.Context, q store.Q, id, ym string) (lines, reqs, slips []store.Row) {
	lines, reqs, slips = []store.Row{}, []store.Row{}, []store.Row{}
	if rows, err := q.C("ledger").Find(ctx, bson.M{"employee_id": id, "void": false, "ym": bson.M{"$gte": ym}},
		options.Find().SetSort(bson.D{{Key: "ym", Value: 1}, {Key: "_id", Value: 1}})); err == nil {
		for _, x := range rows {
			lines = append(lines, store.Row{"ym": x["ym"], "kind": x["kind"], "label": x["label"], "amount": x["amount"], "reason": str(x["reason"])})
		}
	}
	if rows, err := q.C("approval_request").Find(ctx, bson.M{"employee_id": id},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}).SetLimit(30)); err == nil {
		by, _ := ctxIndex(ctx, q, "employee", "_id", ctxValues(rows, "raised_by"))
		for _, x := range rows {
			at, _ := x["created_at"].(time.Time)
			reqs = append(reqs, store.Row{"id": x["id"], "wf": x["wf"], "status": x["status"], "payload": x["payload"],
				"created": at.In(store.Loc()).Format("02 Jan 2006"), "raised_by_name": by[str(x["raised_by"])]["name"]})
		}
	}
	if rows, err := q.C("payslip").Find(ctx, bson.M{"employee_id": id}, options.Find().SetSort(bson.D{{Key: "ym", Value: -1}}).SetLimit(12)); err == nil {
		for _, x := range rows {
			slips = append(slips, store.Row{"ym": x["ym"], "net": x["net"]})
		}
	}
	return lines, reqs, slips
}
