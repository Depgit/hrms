package approval

// The approval grid used before 11 Nov 2026 (collection approval_matrix): condition rows × role columns, each cell
// A (approve) or N (notify). Every matching row was combined and the column order set the approval order. Routes
// are now rule lists (RuleSet); the grid is kept only to read stored grids and convert them (Convert), and the old
// way of combining them (buildMatrix) only as the reference the conversion and its proof test compare against.

// Row is one condition row of a grid.
type Row struct {
	ID    string            `json:"id"`
	Label string            `json:"label"`
	SK    string            `json:"sk,omitempty"` // separation rows: applies to this exit type only
	C     Condition         `json:"c"`
	On    bool              `json:"on"`
	Cells map[string]string `json:"cells"` // role -> "A" (approve) | "N" (notify)
}

// Matrix is a stored grid.
type Matrix struct {
	Cols []string `json:"cols"`
	Rows []Row    `json:"rows"`
}

// evalRow is the old row test: the row's exit type (separation only), then its one condition.
func evalRow(r Row, f Facts) bool {
	if r.SK != "" && f.WF == "resign" && orDefault(f.SKind, "Resignation") != r.SK {
		return false
	}
	return EvalCond(r.C, f)
}

// buildMatrix is the old Build: it combined every matching row (spec 21 #1); Approve beat Notify; approvers ran
// in column order; a person on two stages was asked once.
func buildMatrix(m Matrix, f Facts, resolve func(role string) string) Chain {
	A, N := map[string]bool{}, map[string]bool{}
	out := Chain{Chain: []string{}, Notify: []Notify{}, Why: []string{}}
	for _, r := range m.Rows {
		if !r.On || !evalRow(r, f) {
			continue
		}
		if r.C.K != "always" {
			out.Why = append(out.Why, r.Label)
		}
		for role, v := range r.Cells {
			if v == "A" {
				A[role] = true
			} else if v == "N" {
				N[role] = true
			}
		}
	}
	seen := map[string]bool{}
	for _, col := range m.Cols {
		if !A[col] || (f.WF == "joining" && f.InitiatorRole == col) {
			continue
		}
		p := resolve(col)
		if p != Unassigned && p != "" && seen[p] {
			continue
		}
		seen[p] = true
		out.Chain = append(out.Chain, col)
	}
	for _, col := range m.Cols {
		if N[col] && !A[col] {
			out.Notify = append(out.Notify, Notify{col, resolve(col)})
		}
	}
	return out
}
