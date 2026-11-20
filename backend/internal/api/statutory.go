package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

// Statutory and insurance files (spec 17). Range files (insurance, PF and ESI enrolment and
// exit) list joiners and leavers in a date range and can skip people already sent. Month files
// (ECR, ESI contribution) come from an approved payroll month. Downloads need pay.export and
// the role's download right, and respect data scope. Professional tax is not built (deferred).

var statFiles = map[string]struct {
	Name  string
	Range bool
	Cols  []string
}{
	// Columns as the prototype's file builder (sfBuild) names them. The ECR upload text (ecrText) is
	// the first 11 ECR cells in EPFO's order, unchanged; only the headings here are for people.
	"insurance": {"Insurance enrolment and removal", true, []string{"Action", "Employee code", "Name", "Date of birth", "Gender", "Cover type", "Cover start (joining date)",
		"Cover end (last working day)", "Mobile", "Email", "City", "Department"}},
	"pf": {"PF enrolment and exit", true, []string{"Action", "Employee code", "Name", "Father or spouse name", "Date of birth", "Gender", "Date of joining", "UAN", "UAN status",
		"Aadhaar", "PAN", "Bank account", "IFSC", "EPF wages", "Date of exit", "Reason for exit", "Mobile", "Email", "City"}},
	"esi": {"ESI enrolment and exit", true, []string{"Action", "Employee code", "Name", "Father or husband name", "Date of birth", "Gender", "Marital status", "Date of joining",
		"ESIC number", "IP status", "Aadhaar", "Monthly wages", "Dispensary city", "Date of exit", "Reason for exit", "Mobile"}},
	"ecr": {"ECR (PF contribution)", false, []string{"UAN", "Member name", "Gross wages", "EPF wages", "EPS wages", "EDLI wages", "EPF contribution (employee)",
		"EPS contribution (employer)", "EPF difference (employer)", "NCP days", "Refund of advances", "Remark"}},
	"esic": {"ESI contribution", false, []string{"IP number", "IP name", "Days for which wages paid", "Total monthly wages", "Reason code for zero days", "Last working day (if left)", "Remark"}},
}

// toCollect is a detail the file needs that the employee record does not hold yet.
func toCollect(v any) any {
	if s, _ := v.(string); strings.TrimSpace(s) == "" {
		return "To collect"
	}
	return v
}

type statRow struct {
	Key     string `json:"key"`
	Cells   []any  `json:"cells"`
	Missing string `json:"missing,omitempty"`
	Sent    bool   `json:"sent,omitempty"`
}

// statPeople are the employees matching filter (in employee code order) with what the files need: department,
// work city, personal details, government ids and the latest bank account. Missing personal details, ids and
// bank details are blank (dates null); city limits them to one work city.
func statPeople(ctx context.Context, q store.Q, filter any, city string) ([]store.Row, error) {
	emps, err := q.C("employee").Find(ctx, filter, options.Find().SetSort(compOrder("_id")))
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(emps))
	for i, e := range emps {
		ids[i] = str(e["id"])
	}
	depts, err := q.C("department").Find(ctx, bson.M{}, options.Find().SetProjection(compFields("id", "name")))
	if err != nil {
		return nil, err
	}
	deptName := map[string]any{}
	for _, d := range depts {
		deptName[str(d["id"])] = d["name"]
	}
	locCity, cityName, err := compPlaces(ctx, q)
	if err != nil {
		return nil, err
	}
	of := func(coll string, sort ...string) (map[string]store.Row, error) { // the first row per employee
		rows, err := q.C(coll).Find(ctx, bson.M{"employee_id": bson.M{"$in": ids}}, options.Find().SetSort(compOrder(sort...)))
		if err != nil {
			return nil, err
		}
		m := map[string]store.Row{}
		for _, r := range rows {
			if _, seen := m[str(r["employee_id"])]; !seen {
				m[str(r["employee_id"])] = r
			}
		}
		return m, nil
	}
	personal, err := of("employee_personal", "employee_id")
	if err != nil {
		return nil, err
	}
	gov, err := of("employee_gov_id", "employee_id")
	if err != nil {
		return nil, err
	}
	bank, err := of("employee_bank", "employee_id", "-from_month", "-_id")
	if err != nil {
		return nil, err
	}
	out := []store.Row{}
	for _, e := range emps {
		id := str(e["id"])
		dept, hasDept := deptName[str(e["department_id"])]
		cid, hasCity := locCity[str(e["location_id"])]
		cname, named := cityName[cid]
		if !hasDept || !hasCity || !named || (city != "" && cid != city) { // no department or work city: left out
			continue
		}
		p, g, b := personal[id], gov[id], bank[id]
		out = append(out, store.Row{"id": e["id"], "name": e["name"], "type": e["type"], "status": e["status"], "doj": e["doj"], "lwd": e["lwd"],
			"exit_reason": str(e["exit_reason"]), "dept": dept, "city": cname, "dob": p["dob"], "gender": str(p["gender"]), "mobile": str(p["mobile"]),
			"email": str(p["email"]), "father": str(p["father_spouse"]), "marital": str(p["marital"]), "uan": statIDClean(str(g["uan"])), "esic": statIDClean(str(g["esic"])),
			"aadhaar": str(g["aadhaar"]), "pan": str(g["pan"]), "bank": str(b["bank_name"]), "account": str(b["account_no"]), "ifsc": str(b["ifsc"])})
	}
	return out, nil
}

// statFamilyCover is who among ids has a verified medical insurance nomination (cover for the family too).
func statFamilyCover(ctx context.Context, q store.Q, ids []string) map[string]bool {
	out := map[string]bool{}
	schemes, err := q.C("nomination_scheme").Distinct(ctx, "_id", bson.M{"name": bson.M{"$regex": "medical", "$options": "i"}})
	if err != nil {
		return out
	}
	emps, err := q.C("nomination").Distinct(ctx, "employee_id", bson.M{"employee_id": bson.M{"$in": compNonNil(ids)}, "scheme_id": bson.M{"$in": schemes}, "status": "Verified"})
	if err != nil {
		return out
	}
	for _, e := range emps {
		out[str(e)] = true
	}
	return out
}

func (s *Server) statRows(ctx context.Context, c *Ctx, file, from, to, ym, city string) ([]statRow, error) {
	q := s.st.DB
	scope, err := c.ScopeFilter(ctx, q, "")
	if err != nil {
		return nil, err
	}
	sent := map[string]bool{}
	if keys, err := q.C("statutory_sent").Find(ctx, bson.M{"file": file}, options.Find().SetProjection(compFields("key"))); err == nil {
		for _, k := range keys {
			sent[k["key"].(string)] = true
		}
	}
	out := []statRow{}
	if statFiles[file].Range {
		// Joiners in the range, and leavers in the range (never pre-boarding).
		inRange := bson.M{"$gte": from, "$lte": to}
		list, err := statPeople(ctx, q, bson.M{"$and": bson.A{scope, bson.M{"status": bson.M{"$ne": "Pre-boarding"},
			"$or": bson.A{bson.M{"doj": inRange}, bson.M{"status": "Inactive", "lwd": inRange}}}}}, city)
		if err != nil {
			return nil, err
		}
		ym := to[:7]
		pc, _, err := loadPayCtxOpt(ctx, q, ym, false, bson.M{}, bson.M{"_id": bson.M{"$in": idsOf(list)}})
		if err != nil {
			return nil, err
		}
		family := map[string]bool{}
		if file == "insurance" {
			family = statFamilyCover(ctx, q, idsOf(list))
		}
		for _, p := range list {
			id := p["id"].(string)
			typ := calc.EmploymentType(p["type"].(string))
			if !calc.RulesFor(typ).Statutory {
				continue
			}
			st := pc.structure(pc.structureOn(id, ym+"-01"))
			gross := pc.grossAt(id, ym)
			if gross == 0 {
				gross = pc.grossAt(id, str(p["doj"])[:7])
			}
			res := calc.Calc(calc.Input{Type: typ, Gross: gross, Structure: st})
			joined := str(p["doj"]) >= from && str(p["doj"]) <= to
			left := p["status"] == "Inactive" && str(p["lwd"]) >= from && str(p["lwd"]) <= to
			death := strings.Contains(strings.ToLower(str(p["exit_reason"])), "death") || strings.Contains(strings.ToLower(str(p["exit_reason"])), "disab")
			add := func(action string, cells []any, missing string) {
				k := id + "|" + action
				r := statRow{Key: k, Cells: append([]any{action}, cells...), Missing: missing, Sent: sent[k]}
				out = append(out, r)
			}
			switch file {
			case "insurance":
				if !(res.MedOn) {
					continue
				}
				cover := "Employee only"
				if family[id] {
					cover = "Employee and family"
				}
				row := func(action string) {
					end := any("") // cover ends only for a removal
					if action == "Remove" {
						end = p["lwd"]
					}
					add(action, []any{id, p["name"], p["dob"], p["gender"], cover, p["doj"], end, p["mobile"], p["email"], p["city"], p["dept"]}, "")
				}
				if joined {
					row("Add")
				}
				if left {
					row("Remove")
				}
			case "pf":
				if pc.pay[id].PfExempt { // a PF exempt joiner is not a PF member (pfuan.go)
					continue
				}
				miss := ""
				if p["aadhaar"] == "" {
					miss = "Aadhaar missing"
				}
				uanStatus := "Existing UAN, link to establishment"
				if p["uan"] == "" {
					uanStatus = "New UAN to be generated"
				}
				reason := "1 - Cessation of service"
				if death {
					reason = "3 - Death"
				}
				base := []any{id, p["name"], toCollect(p["father"]), p["dob"], p["gender"], p["doj"], p["uan"], uanStatus, p["aadhaar"], p["pan"], p["account"], p["ifsc"], math.Round(res.PfBase)}
				tail := func(exit, why any) []any {
					return append(append([]any{}, base...), exit, why, p["mobile"], p["email"], p["city"])
				}
				if joined {
					add("Enrol", tail("", ""), miss)
				}
				if left {
					m := ""
					if p["uan"] == "" {
						m = "UAN missing"
					}
					add("Exit", tail(p["lwd"], reason), m)
				}
			case "esi":
				covered := gross <= st.EsiCeil
				if !covered {
					continue
				}
				ip := "Existing IP, tag to employer"
				if p["esic"] == "" {
					ip = "New IP number to be generated"
				}
				reason := "Left service"
				if death {
					reason = "Death"
				}
				base := []any{id, p["name"], toCollect(p["father"]), p["dob"], p["gender"], toCollect(p["marital"]), p["doj"], p["esic"], ip, p["aadhaar"], math.Round(gross), p["city"]}
				tail := func(exit, why any) []any {
					return append(append([]any{}, base...), exit, why, p["mobile"])
				}
				if joined {
					add("Enrol", tail("", ""), "")
				}
				if left {
					m := ""
					if p["esic"] == "" {
						m = "IP number missing"
					}
					add("Exit", tail(p["lwd"], reason), m)
				}
			}
		}
		return out, nil
	}
	// Month files from the approved payroll rows.
	m, err := loadPayMonth(ctx, q, ym, false)
	if err != nil {
		return nil, err
	}
	if m.Status != "Approved" && m.Status != "Published" {
		return nil, bad("Payroll for %s is not approved yet", ymLabel(ym))
	}
	rows, err := loadRows(ctx, q, ym)
	if err != nil {
		return nil, err
	}
	list, err := statPeople(ctx, q, scope, city)
	if err != nil {
		return nil, err
	}
	byID := map[string]store.Row{}
	for _, p := range list {
		byID[p["id"].(string)] = p
	}
	for _, o := range rows {
		p := byID[o.EmployeeID]
		if p == nil {
			continue // outside scope
		}
		switch file {
		case "ecr":
			if o.R.EePF <= 0 {
				continue
			}
			miss := ""
			switch uan := str(p["uan"]); {
			case uan == "":
				miss = "UAN missing"
			case !reUAN.MatchString(uan):
				miss = "UAN is not 12 digits"
			}
			c := ecrCells(o)
			out = append(out, statRow{Key: o.EmployeeID, Missing: miss, Cells: append([]any{p["uan"], o.Name}, append(c, miss)...)})
		case "esic":
			if !o.R.EsiOn {
				continue
			}
			reason, lwd := "", ""
			if o.LWD != "" && o.LWD[:7] == ym {
				reason, lwd = "2 - Left service", o.LWD
			} else if o.Paid == 0 {
				reason = "1 - Without work"
			}
			miss := ""
			switch ip := str(p["esic"]); {
			case ip == "":
				miss = "IP number missing"
			case !reESIC.MatchString(ip):
				miss = "IP number is not 10 or 17 digits"
			}
			// Whole days, and the wages the contribution was worked out on (earned gross): reimbursements,
			// bonus, arrears and released held salary are not ESI wages (RUN-58, RUN-59, REAL-41).
			out = append(out, statRow{Key: o.EmployeeID, Missing: miss, Cells: []any{p["esic"], o.Name, math.Floor(o.Paid + 1e-9), math.Round(o.R.Gross), reason, lwd, miss}})
		}
	}
	return out, nil
}

// EpsCeil is the statutory wage ceiling for the pension scheme (EPS) and EDLI: 8.33% of at most ₹15,000, so EPS is
// at most ₹1,250 and the rest of the employer's share goes to EPF. The company's own PF ceiling (₹25,000) applies
// only to EPF wages (REAL-40).
const EpsCeil = calc.EpsCeil

var reESIC = regexp.MustCompile(`^(\d{10}|\d{17})$`)

// statIDClean reads a UAN or ESIC number as stored: spaces dropped, and placeholders such as "No" or "NA"
// (left by imports) count as blank.
func statIDClean(v string) string {
	v = strings.ReplaceAll(strings.TrimSpace(v), " ", "")
	switch strings.ToUpper(strings.Trim(v, ".-/")) {
	case "", "NO", "NA", "N/A", "NIL", "NONE", "NULL", "0", "TOCOLLECT":
		return ""
	}
	return v
}

// ecrCells are the ECR figures for a row, from gross wages to refund (EPFO's order, without UAN and name):
// gross wages without reimbursements, bonus or pay lines; EPF wages; EPS and EDLI wages capped at EpsCeil; the
// employee's EPF; EPS on the capped wages (never more than the employer's share); the employer's EPF difference;
// NCP days, which leave out the days before joining and after the last working day (REAL-42); refund.
func ecrCells(o *PayRowOut) []any {
	epfWage := math.Round(o.R.PfBase)
	epsWage := math.Min(epfWage, EpsCeil)
	er := math.Round(o.R.ErPF)
	eps := calc.EpsShare(epfWage, er) // as the salary calculation splits it (R.Eps)
	ncp := math.Max(0, math.Round(float64(o.Days)-o.Paid-o.OutsideDays()))
	return []any{math.Round(o.R.Gross + o.R.VarPay), epfWage, epsWage, epsWage, math.Round(o.R.EePF), eps, er - eps, ncp, 0}
}

func idsOf(rows []store.Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r["id"].(string)
	}
	return out
}

// statParams reads the file and period; a download needs pay.export, a preview pay.run (view) or pay.export.
func (s *Server) statParams(r *http.Request, c *Ctx, download bool) (file, from, to, ym string, err error) {
	if download || c.Level("pay.run") < 1 {
		if err = c.Need("pay.export", 2); err != nil {
			return
		}
	}
	file = r.PathValue("file")
	f, ok := statFiles[file]
	if !ok {
		err = notFound("")
		return
	}
	qv := r.URL.Query()
	if f.Range {
		from, to = qv.Get("from"), qv.Get("to")
		if !isDate(from) || !isDate(to) || to < from {
			err = bad("Choose the date range")
		}
		return
	}
	ym = qv.Get("ym")
	if !isYm(ym) {
		err = bad("Choose the payroll month")
	}
	return
}

func (s *Server) statPreview(r *http.Request, c *Ctx) (any, error) {
	file, from, to, ym, err := s.statParams(r, c, false)
	if err != nil {
		return nil, err
	}
	rows, err := s.statRows(r.Context(), c, file, from, to, ym, r.URL.Query().Get("city"))
	if err != nil {
		return nil, err
	}
	rows = statMask(c, file, rows)
	missing, sent := 0, 0
	for _, x := range rows {
		if x.Missing != "" {
			missing++
		}
		if x.Sent {
			sent++
		}
	}
	return map[string]any{"name": statFiles[file].Name, "cols": statFiles[file].Cols, "rows": rows, "missing": missing, "sent": sent}, nil
}

// statSheet downloads a file; range files skip people already sent when asked and remember who was sent.
func (s *Server) statSheet(r *http.Request, c *Ctx) (string, []string, [][]any, error) {
	file, from, to, ym, err := s.statParams(r, c, true)
	if err != nil {
		return "", nil, nil, err
	}
	ctx := r.Context()
	city := r.URL.Query().Get("city")
	rows, err := s.statRows(ctx, c, file, from, to, ym, city)
	if err != nil {
		return "", nil, nil, err
	}
	skip := r.URL.Query().Get("skipSent") == "1"
	out := [][]any{}
	keys := []string{}
	for _, x := range rows {
		if skip && x.Sent {
			continue
		}
		out = append(out, x.Cells)
		keys = append(keys, x.Key)
	}
	if statFiles[file].Range && len(keys) > 0 {
		_ = s.st.Tx(ctx, func(tx store.Q) error {
			for _, k := range keys {
				if _, err := tx.C("statutory_sent").Upsert(ctx, bson.M{"file": file, "key": k}, nil, bson.M{"by_name": c.Name}); err != nil { // sent once
					return err
				}
			}
			return audit(ctx, tx, c, "Downloaded "+statFiles[file].Name, "statutory_sent", file, nil, map[string]any{"from": from, "to": to, "rows": len(keys)})
		})
	}
	_ = statRecord(ctx, s.st.DB, c, file, from, to, ym, city, skip, keys) // the "Files created so far" list
	period := ym
	if period == "" {
		period = from + "-to-" + to
	}
	return file + "-" + period + ".xlsx", statFiles[file].Cols, out, nil
}

// ecrText is the ECR upload file: one line per member with a UAN, fields joined by #~#.
func (s *Server) ecrText(r *http.Request, c *Ctx) (string, string, error) {
	if err := c.Need("pay.export", 2); err != nil {
		return "", "", err
	}
	ym := r.URL.Query().Get("ym")
	if !isYm(ym) {
		return "", "", bad("Choose the payroll month")
	}
	rows, err := s.statRows(r.Context(), c, "ecr", "", "", ym, r.URL.Query().Get("city"))
	if err != nil {
		return "", "", err
	}
	lines := []string{}
	for _, x := range rows {
		if x.Missing != "" {
			continue
		}
		parts := []string{}
		for _, v := range x.Cells[:11] {
			parts = append(parts, strings.TrimSuffix(fmt.Sprint(v), ".00"))
		}
		lines = append(lines, strings.Join(parts, "#~#"))
	}
	return "ECR-" + ym + ".txt", strings.Join(lines, "\n") + "\n", nil
}
