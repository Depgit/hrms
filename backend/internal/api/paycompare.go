package api

// The before/after comparison for pay components (`hrms pay-compare -month YYYY-MM`): every person paid in a month is
// calculated twice from the same data, once with the calculation as it was before components (calc.Input.Classic: the
// percentage rates only) and once as payroll now runs it (the structure's components), and every difference is
// listed. For a structure converted from percentages (migration 014) there must be none, to the paisa. People on a
// structure made from components are counted apart: their pay is meant to differ. Read only: nothing is written.

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

// PayCompareResult is one month's comparison.
type PayCompareResult struct {
	Ym       string    `json:"ym"`
	People   int       `json:"people"`   // paid in the month
	Compared int       `json:"compared"` // on a converted (percentage) structure
	Designed int       `json:"designed"` // on a structure made from components: not compared
	Net      float64   `json:"net"`      // total net pay of the compared rows, as calculated now
	Diffs    []PayDiff `json:"diffs"`
	Sample   []PaySpot `json:"sample,omitempty"` // a few rows, both ways, for the report
}

// PayDiff is a person whose two calculations differ, with each differing field ("r.basic: 10000 → 10000.01").
type PayDiff struct {
	EmployeeID string   `json:"employeeId"`
	Name       string   `json:"name"`
	Fields     []string `json:"fields"`
}

// PaySpot is a row's headline figures, before and after.
type PaySpot struct {
	EmployeeID string  `json:"employeeId"`
	Gross      float64 `json:"gross"`
	NetBefore  float64 `json:"netBefore"`
	NetAfter   float64 `json:"netAfter"`
	CTCBefore  float64 `json:"ctcBefore"`
	CTCAfter   float64 `json:"ctcAfter"`
}

// PayCompare compares month ym.
func (s *Server) PayCompare(ctx context.Context, ym string) (*PayCompareResult, error) {
	if !isYm(ym) {
		return nil, fmt.Errorf("month must look like 2026-11")
	}
	pc, list, err := loadPayCtx(ctx, s.st.DB, ym, nil)
	if err != nil {
		return nil, err
	}
	out := &PayCompareResult{Ym: ym, People: len(list), Diffs: []PayDiff{}}
	for _, e := range list {
		if pc.structure(pc.structureOn(e.ID, ym+"-01")).Designed || pc.blendsDesigned(e.ID, ym) {
			out.Designed++
			continue
		}
		pc.classic = false
		now := pc.row(e)
		pc.classic = true
		before := pc.row(e)
		pc.classic = false
		out.Compared++
		out.Net += now.Net
		if len(out.Sample) < 5 {
			out.Sample = append(out.Sample, PaySpot{e.ID, now.Gross, before.Net, now.Net, before.R.CTC, now.R.CTC})
		}
		if reflect.DeepEqual(now, before) {
			continue
		}
		out.Diffs = append(out.Diffs, PayDiff{EmployeeID: e.ID, Name: e.Name, Fields: jsonDiff(before, now)})
	}
	return out, nil
}

// blendsDesigned: a structure made from components applies from a date inside the month (the month is blended).
func (pc *payCtx) blendsDesigned(id, ym string) bool {
	for _, m := range pc.smap[id] {
		if m.From[:7] == ym && pc.structure(m.ID).Designed {
			return true
		}
	}
	return false
}

// jsonDiff lists the leaves that differ between two values, as they would be stored.
func jsonDiff(a, b any) []string {
	flat := func(v any) map[string]string {
		raw, _ := json.Marshal(v)
		var x any
		_ = json.Unmarshal(raw, &x)
		out := map[string]string{}
		var walk func(p string, v any)
		walk = func(p string, v any) {
			switch t := v.(type) {
			case map[string]any:
				for k, y := range t {
					walk(p+"."+k, y)
				}
			case []any:
				for i, y := range t {
					walk(fmt.Sprintf("%s[%d]", p, i), y)
				}
			default:
				b, _ := json.Marshal(t)
				out[p] = string(b)
			}
		}
		walk("", x)
		return out
	}
	fa, fb := flat(a), flat(b)
	keys := map[string]bool{}
	for k := range fa {
		keys[k] = true
	}
	for k := range fb {
		keys[k] = true
	}
	out := []string{}
	for k := range keys {
		if fa[k] != fb[k] {
			out = append(out, fmt.Sprintf("%s: %s → %s", k[1:], orNone(fa[k]), orNone(fb[k])))
		}
	}
	sort.Strings(out)
	return out
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// ---------- Before and after a data change (migration 017): -dump, then -against ----------

// PayRows calculates every person paid in month ym as payroll now runs it, by employee id (read only).
func (s *Server) PayRows(ctx context.Context, ym string) (map[string]*PayRowOut, error) {
	if !isYm(ym) {
		return nil, fmt.Errorf("month must look like 2026-11")
	}
	pc, list, err := loadPayCtx(ctx, s.st.DB, ym, nil)
	if err != nil {
		return nil, err
	}
	out := map[string]*PayRowOut{}
	for _, e := range list {
		out[e.ID] = pc.row(e)
	}
	return out, nil
}

// PayAgainst compares the rows of month ym as calculated now with rows saved earlier (PayRows, as JSON), field by
// field. The component lines (r.comps, shown on the payslip of a structure with a variable target) and the "variable
// not entered" note are display only and left out; every other difference is listed.
func (s *Server) PayAgainst(ctx context.Context, ym string, before map[string]json.RawMessage) (*PayCompareResult, error) {
	now, err := s.PayRows(ctx, ym)
	if err != nil {
		return nil, err
	}
	strip := func(v any) map[string]any {
		raw, _ := json.Marshal(v)
		m := map[string]any{}
		_ = json.Unmarshal(raw, &m)
		delete(m, "varMissing")
		if r, ok := m["r"].(map[string]any); ok {
			delete(r, "comps")
		}
		return m
	}
	out := &PayCompareResult{Ym: ym, People: len(now), Diffs: []PayDiff{}}
	ids := []string{}
	for id := range now {
		ids = append(ids, id)
	}
	for id := range before {
		if now[id] == nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		var b any
		if raw, ok := before[id]; ok {
			_ = json.Unmarshal(raw, &b)
		}
		a := now[id]
		out.Compared++
		if a != nil {
			out.Net += a.Net
		}
		bm, am := strip(b), strip(a)
		if reflect.DeepEqual(bm, am) {
			continue
		}
		name := ""
		if a != nil {
			name = a.Name
		}
		out.Diffs = append(out.Diffs, PayDiff{EmployeeID: id, Name: name, Fields: jsonDiff(bm, am)})
	}
	return out, nil
}
