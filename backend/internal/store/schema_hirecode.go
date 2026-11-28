package store

// Hiring IDs of imported employees from their Employee code (owner's rule of 24 Nov 2026):
//
//   - An employee who did not come through a candidate (Import employees, or joined before stages) has Shortlisting,
//     Offer and Joining IDs made from the Employee code, only the prefix changed and the digits kept exactly:
//     EMP-000004 → SI-000004, OI-000004, JI-000004; EMP-1050 → SI-1050; a code that is only digits, 1050 → SI-1050.
//     The digits are the code's trailing run of digits.
//   - employee.hire_digits keeps those digits as text (leading zeros); employee.hire_no stays their number. Without
//     hire_digits the IDs are hire_no with four digits (SI-0042): a hire through Onboarding (the candidate's number)
//     and an imported employee whose number could not be used.
//   - The number cannot be used when a candidate has it (candidate.no) or another employee's hiring IDs do (two code
//     series, EMP-0005 and CON-0005), or the code has no digits: then the employee takes the next number of the
//     candidate series as before, and the timeline says why.
//   - candidate_seq is raised to at least the highest number used, so a later candidate never repeats one.
//
// Migration 025 applies the rule to every existing employee without a candidate record (migration 018 gave them
// numbers of the candidate series): the fields and the "Hiring IDs: …" timeline entry are rewritten. Hires through
// candidates are not touched. It runs again safely: a second run changes nothing.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var _ = registerHireCode()

func registerHireCode() bool {
	for _, t := range schemaTables {
		if t.cols != nil {
			continue
		}
		if t.Name == "employee" {
			t.Columns = append(t.Columns, Column{Name: "hire_digits", Type: TText})
		}
	}
	migrations = append(migrations, migration{"025_hire_ids_from_code", false, func(ctx context.Context, s *Store) error {
		_, err := HireIDsFromCode(ctx, s.DB)
		return err
	}})
	return true
}

// HireCodeDigits reads the number of an Employee code: its trailing digits (at most 9) and their value. ok is false
// when the code ends without a digit or the number is 0.
func HireCodeDigits(code string) (no int, digits string, ok bool) {
	code = strings.TrimSpace(code)
	i := len(code)
	for i > 0 && code[i-1] >= '0' && code[i-1] <= '9' {
		i--
	}
	digits = code[i:]
	if digits == "" || len(digits) > 9 {
		return 0, "", false
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n <= 0 {
		return 0, "", false
	}
	return n, digits, true
}

// HireIDsOf are the Shortlisting, Offer and Joining IDs: the digits as kept (hire_digits) when given, else the number
// with four digits.
func HireIDsOf(no int, digits string) (si, oi, ji string) {
	if digits == "" {
		return HireIDs(no)
	}
	return "SI-" + digits, "OI-" + digits, "JI-" + digits
}

// HireNoUser names who already has a hire number other than the employee emp: "candidate SI-0004" or "employee
// CON-0005", or "" when it is free.
func HireNoUser(ctx context.Context, q Q, no int, emp string) (string, error) {
	c, err := q.C("candidate").FindOne(ctx, bson.D{{Key: "no", Value: no}}, options.FindOne().SetProjection(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return "", err
	}
	if c != nil {
		return fmt.Sprintf("candidate %v", c["id"]), nil
	}
	e, err := q.C("employee").FindOne(ctx, bson.D{{Key: "hire_no", Value: no}, {Key: "_id", Value: bson.D{{Key: "$ne", Value: emp}}}}, options.FindOne().SetProjection(bson.D{{Key: "_id", Value: 1}}))
	if err != nil || e == nil {
		return "", err
	}
	return fmt.Sprintf("employee %v", e["id"]), nil
}

// RaiseSeq makes sure a sequence's next value is above last (setval to last only when it is behind). Like NextSeq
// it runs outside any transaction.
func RaiseSeq(ctx context.Context, q Q, name string, last int64) error {
	if last <= 0 {
		return nil
	}
	if err := ensureCounter(ctx, q.db, name); err != nil {
		return err
	}
	_, err := q.db.Collection(countersColl).UpdateOne(noSession{ctx}, bson.D{{Key: "_id", Value: name}}, bson.D{{Key: "$max", Value: bson.D{{Key: "value", Value: last}}}})
	return err
}

// HireNoFromCode works out the hiring number of an employee who did not come through a candidate: the Employee code's
// number when it is free, else the next free number of the candidate series. digits is "" for a series number; why
// says why the code's number was not used ("" when it was). candidate_seq is raised past the number. Inside a
// transaction the caller takes the "hire_no" advisory lock first.
func HireNoFromCode(ctx context.Context, q Q, emp string) (no int, digits, why string, err error) {
	n, d, ok := HireCodeDigits(emp)
	if ok {
		user, err := HireNoUser(ctx, q, n, emp)
		if err != nil {
			return 0, "", "", err
		}
		if user == "" {
			return n, d, "", RaiseSeq(ctx, q, "candidate_seq", int64(n))
		}
		why = fmt.Sprintf("SI-%s is already used by %s", d, user)
	} else {
		why = "the Employee code has no number"
	}
	for i := 0; i < 1000; i++ {
		x, err := NextSeq(ctx, q, "candidate_seq")
		if err != nil {
			return 0, "", "", err
		}
		user, err := HireNoUser(ctx, q, int(x), emp)
		if err != nil {
			return 0, "", "", err
		}
		if user == "" {
			return int(x), "", why, nil
		}
	}
	return 0, "", "", fmt.Errorf("no free number in the candidate series")
}

// SetHireNo stores an employee's hiring number and digits (nil digits for a series number).
func SetHireNo(ctx context.Context, q Q, emp string, no int, digits string) error {
	var d any
	if digits != "" {
		d = digits
	}
	_, err := q.C("employee").Update(ctx, bson.D{{Key: "_id", Value: emp}}, bson.D{{Key: "$set", Value: bson.D{{Key: "hire_no", Value: int32(no)}, {Key: "hire_digits", Value: d}}}})
	return err
}

// HireReason is the timeline reason of IDs given at import or by migration 025: base, then where the number came from.
func HireReason(base, why string) string {
	if why == "" {
		return base + ": the Employee code's number"
	}
	return base + ": " + why + ", so the next number of the candidate series"
}

// HireIDsFromCode is migration 025 (see the file comment). It returns how many employees' IDs changed.
func HireIDsFromCode(ctx context.Context, q Q) (int, error) {
	cands, err := q.C("candidate").Find(ctx, bson.D{}, options.Find().SetProjection(bson.D{{Key: "no", Value: 1}, {Key: "employee_id", Value: 1}}))
	if err != nil {
		return 0, err
	}
	taken := map[int]string{} // number → who has it
	viaCand := map[string]bool{}
	top := 0
	for _, c := range cands {
		no := rowInt(c["no"])
		if no > 0 {
			taken[no] = fmt.Sprintf("candidate %v", c["id"])
			top = max(top, no)
		}
		if e, _ := c["employee_id"].(string); e != "" {
			viaCand[e] = true
		}
	}
	emps, err := q.C("employee").Find(ctx, bson.D{}, options.Find().SetProjection(bson.D{{Key: "doj", Value: 1}, {Key: "hire_no", Value: 1}, {Key: "hire_digits", Value: 1}}).
		SetSort(bson.D{{Key: "doj", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return 0, err
	}
	type person struct {
		id, doj, curDigits string
		cur                int
		no                 int
		digits, why        string
		ok, done           bool
	}
	var list []*person
	for _, e := range emps {
		id, _ := e["id"].(string)
		if viaCand[id] {
			continue // a hire through Onboarding keeps the candidate's number
		}
		p := &person{id: id, cur: rowInt(e["hire_no"])}
		p.doj, _ = e["doj"].(string)
		p.curDigits, _ = e["hire_digits"].(string)
		p.no, p.digits, p.ok = HireCodeDigits(id)
		list = append(list, p)
	}
	// 1. Those already on their code's number keep it; 2. the others take their code's number when it is free.
	for _, p := range list {
		if p.ok && p.cur == p.no && p.curDigits == p.digits && taken[p.no] == "" {
			taken[p.no], p.done = "employee "+p.id, true
			top = max(top, p.no)
		}
	}
	for _, p := range list {
		if p.done || !p.ok {
			continue
		}
		if u := taken[p.no]; u != "" {
			p.why = fmt.Sprintf("SI-%s is already used by %s", p.digits, u)
			continue
		}
		taken[p.no], p.done = "employee "+p.id, true
		top = max(top, p.no)
	}
	// 3. The rest keep the series number they have when it is still free, else take a new one.
	for _, p := range list {
		if p.done {
			continue
		}
		p.digits = ""
		if !p.ok {
			p.why = "the Employee code has no number"
		}
		if p.cur > 0 && p.curDigits == "" && taken[p.cur] == "" {
			p.no, p.done = p.cur, true
			taken[p.no] = "employee " + p.id
			top = max(top, p.no)
		}
	}
	if err := RaiseSeq(ctx, q, "candidate_seq", int64(top)); err != nil {
		return 0, err
	}
	for _, p := range list {
		if p.done {
			continue
		}
		for {
			x, err := NextSeq(ctx, q, "candidate_seq")
			if err != nil {
				return 0, err
			}
			if taken[int(x)] == "" {
				p.no = int(x)
				break
			}
		}
		taken[p.no] = "employee " + p.id
	}
	changed := 0
	for _, p := range list {
		if p.no == p.cur && p.digits == p.curDigits {
			continue
		}
		if err := SetHireNo(ctx, q, p.id, p.no, p.digits); err != nil {
			return changed, err
		}
		reason := HireReason("Imported or joined before stages (migration 025)", p.why)
		if p.cur > 0 {
			osi, ooi, oji := HireIDsOf(p.cur, p.curDigits)
			reason += fmt.Sprintf(" (until 24 Nov 2026: %s, %s, %s)", osi, ooi, oji)
		}
		si, oi, ji := HireIDsOf(p.no, p.digits)
		n, err := q.C(LifecycleEventColl).UpdateMany(ctx, bson.D{{Key: "employee_id", Value: p.id}, {Key: "subject", Value: "employee"}, {Key: "action", Value: bson.D{{Key: "$regex", Value: "^Hiring IDs: "}}}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "action", Value: fmt.Sprintf("Hiring IDs: %s, %s, %s", si, oi, ji)}, {Key: "to_state", Value: ji}, {Key: "reason", Value: reason}}}})
		if err != nil {
			return changed, err
		}
		if n == 0 {
			if err := HireIDsEvent(ctx, q, p.id, p.no, p.digits, p.doj, reason, "System", ""); err != nil {
				return changed, err
			}
		}
		changed++
	}
	return changed, nil
}

// hireAt is the time of a "Hiring IDs" timeline entry: the joining date when it is in the past, else now.
func hireAt(doj string) time.Time {
	at := time.Now()
	if t, err := time.Parse("2006-01-02", doj); err == nil && t.Before(at) {
		at = t
	}
	return at
}
