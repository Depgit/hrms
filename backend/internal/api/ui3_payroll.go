package api

import (
	"context"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// Round 3, payroll (prototype gaps): the place on Form 16, master order for leave plans,
// the prototype's arrear reasons, improvement plans waiting for approval on Upcoming pay lines,
// and assets added to the central store by Payroll.

// ---------- Form 16: "Place: Gurugram · Date: …" ----------

var rePIN = regexp.MustCompile(`\b\d{6}\b`)

// companyPlace is the city of the registered office, for the place line of Form 16: the city
// from the city master that the address names (the last one named), else the part of the address
// before the state and PIN ("Plot 12, Sector 29, Gurugram, Haryana 122001" gives Gurugram).
// Empty while the address is not filled in.
func companyPlace(ctx context.Context, q store.Q, address string) string {
	a := strings.TrimSpace(address)
	if a == "" || strings.HasPrefix(a, "Registered office address") {
		return ""
	}
	low := strings.ToLower(a)
	best, at := "", -1
	if rows, err := q.C("city").Find(ctx, nil, options.Find().SetProjection(bson.D{{Key: "name", Value: 1}})); err == nil {
		for _, r := range rows {
			n := strings.TrimSpace(str(r["name"]))
			if n == "" {
				continue
			}
			if i := wordIndex(low, strings.ToLower(n)); i > at {
				best, at = n, i
			}
		}
	}
	if best != "" {
		return best
	}
	parts := []string{}
	for _, p := range strings.Split(a, ",") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) < 2 {
		return ""
	}
	last := strings.TrimSpace(rePIN.ReplaceAllString(parts[len(parts)-1], ""))
	if last == "" || len(parts) >= 3 { // "…, Gurugram, Haryana 122001" or "…, Gurugram, 122001"
		return strings.TrimSpace(rePIN.ReplaceAllString(parts[len(parts)-2], ""))
	}
	return last // "Sector 29, Gurugram 122001"
}

// wordIndex is the last index of word in s as a whole word, or -1.
func wordIndex(s, word string) int {
	alnum := func(b byte) bool { return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' }
	last := -1
	for i := 0; i < len(s); {
		j := strings.Index(s[i:], word)
		if j < 0 {
			break
		}
		j += i
		end := j + len(word)
		if (j == 0 || !alnum(s[j-1])) && (end == len(s) || !alnum(s[end])) {
			last = j
		}
		i = j + 1
	}
	return last
}

// ---------- Leave plans in master order ----------

// leavePlanOrder is the position of each leave plan in master order (the order they were created).
func leavePlanOrder(ctx context.Context, q store.Q) map[string]int {
	out := map[string]int{}
	rows, err := q.C("leave_plan").Find(ctx, nil, options.Find().SetSort(bson.D{{Key: "seq", Value: 1}}).SetProjection(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return out
	}
	for i, r := range rows {
		out[str(r["id"])] = i
	}
	return out
}

// ---------- Arrear reasons ----------

// legacyArrearTypes are the reasons offered before the prototype's list. Arrears already raised
// with them keep their reason; a request that still sends one (an open form, the bulk gross
// change's "Salary revision") is accepted. They are not offered for new arrears.
var legacyArrearTypes = []string{"Salary revision", "Attendance correction", "Missed payment", "Shift or overtime"}

func arrearTypeOK(t string) bool {
	return containsStr(arrearTypes, t) || containsStr(legacyArrearTypes, t)
}

// ---------- Upcoming pay lines: improvement plans waiting for approval ----------

// pendingPips are the improvement plans waiting for approval, for the "Waiting for approval" list
// of Upcoming pay lines (prototype vFwd lists them as "PIP · N days"). It uses the approvals
// inbox's own visibility and scope, so nobody sees a plan here they could not see in Approvals.
func (s *Server) pendingPips(ctx context.Context, c *Ctx) []store.Row {
	l, err := s.apprUIPips(ctx, c, "")
	if err != nil {
		return nil
	}
	out := []store.Row{}
	for _, x := range l {
		if x["status"] != "Pending" {
			continue
		}
		created := ""
		if t, ok := x["created_at"].(time.Time); ok && !t.IsZero() {
			created = t.Format("02 Jan 2006")
		}
		out = append(out, store.Row{"id": x["id"], "wf": "p_pip", "employee_id": x["employee_id"], "name": x["employee_name"],
			"payload": x["payload"], "created": created, "sub": x["sub"], "waitingOn": x["waitingOn"]})
	}
	return out
}

// ---------- Assets into the central store ----------

// canAddAssets: who adds serial-numbered assets to the central store. HR Admin and HRBP Corporate
// add any stock; everyone else with dues edit rights except an HRBP (whose stock arrives by
// transfer) adds assets only, as the prototype's Assets tab allows ("Add an asset").
func canAddAssets(c *Ctx) bool {
	return c.Level("emp.assets") >= 2 && c.Role != "HRBP"
}
