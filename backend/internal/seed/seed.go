// Package seed loads reference data (roles, rights, structure v1, LWF, approval matrices,
// change reasons) and, optionally, a sample organisation matching the prototype.
package seed

import (
	"context"
	"fmt"
	"math/rand"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/approval"
	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

// Reference loads data every installation needs. Safe to run repeatedly.
func Reference(ctx context.Context, st *store.Store) error {
	return st.Tx(ctx, func(tx store.Q) error {
		for _, r := range access.Roles {
			// INSERT … ON CONFLICT (name) DO UPDATE SET description=COALESCE(role.description, $3)
			if _, err := tx.C("role").Upsert(ctx, bson.M{"name": r}, nil,
				bson.M{"description": access.RoleDescriptions[r], "system": true, "can_download": !access.NoDownloadByDefault[r]}); err != nil {
				return err
			}
			if _, err := tx.C("role").Update(ctx, bson.M{"name": r, "description": nil}, bson.M{"$set": bson.M{"description": access.RoleDescriptions[r]}}); err != nil {
				return err
			}
			for _, right := range access.Rights {
				if _, err := tx.C("role_right").Upsert(ctx, bson.M{"role_name": r, "right_id": right.ID}, nil, bson.M{"level": access.DefaultRights[r][right.ID]}); err != nil {
					return err
				}
			}
			for g, m := range access.DefaultSensitive[r] {
				if _, err := tx.C("role_sensitive").Upsert(ctx, bson.M{"role_name": r, "grp": g}, nil, bson.M{"mode": m}); err != nil {
					return err
				}
			}
		}
		// ON CONFLICT DO NOTHING without a target: the name, or another default structure (one_default_structure).
		if taken, err := tx.C("salary_structure").Exists(ctx, bson.M{"$or": bson.A{bson.M{"name": "Version 1"}, bson.M{"is_default": true}}}); err != nil {
			return err
		} else if !taken {
			if _, err := tx.C("salary_structure").Insert(ctx, bson.M{"name": "Version 1", "rates": calc.DefaultStructure, "is_default": true}); err != nil {
				return err
			}
		}
		// The approval rule lists. Grids stored before the rule lists (an upgrade, when seed runs before the app has
		// migrated) are converted first, so HR's edited grids are kept; the seeded lists then fill only what is
		// still missing. A list that exists is left as it is.
		if _, err := store.ConvertApprovalMatrices(ctx, tx); err != nil {
			return err
		}
		for wf, flows := range approval.SeedFlows() {
			for flow, rs := range flows {
				noResp := "escalate"
				if wf == "regularize" {
					noResp = "auto"
				}
				if _, err := tx.C(store.ApprovalFlowColl).Upsert(ctx, bson.M{"wf": wf, "flow": string(flow)}, nil,
					bson.M{"rules": rs.Rules, "no_response": noResp}); err != nil {
					return err
				}
			}
		}
		reasons := map[string][]string{
			"Employee update": {"Promotion", "Transfer", "Role change", "Reporting line change", "Restructuring", "Policy change", "Data correction"},
			"Salary":          {"Annual increment", "Promotion", "Transfer", "Market correction", "Policy change", "Data correction"},
			"Separation":      {"Resigned", "Terminated", "Contract ended", "Absconded", "Retired", "Death or permanent disability"},
			"Master data":     {"Discontinued", "Replaced by a new entry", "Restructuring", "Created by mistake"},
			"Hold":            {"Absent without notice", "Pending clearance or handover", "Investigation or disciplinary case", "Bank or KYC issue", "Asset not returned", "Other"},
			"Deduction":       {"Cash shortage", "Damage or loss of property", "Penalty as per policy", "Notice period shortfall", "Excess payment recovery", "Other recovery"},
		}
		for usedFor, names := range reasons {
			for _, n := range names {
				if _, err := tx.C("simple_master").Upsert(ctx, bson.M{"kind": "change_reason", "name": n, "used_for": usedFor}, nil, nil); err != nil {
					return err
				}
			}
		}
		simple := map[string][]string{
			"qualification":   {"10th", "12th", "Diploma", "Graduate", "Post graduate"},
			"employment_type": {"Permanent", "Retainer", "Contractual"},
			"attendance_mode": {"Biometric", "Geo-fenced app", "Manager marked"},
			// Job roles and skills (owner, 14 Nov 2026): a starting list HR changes in Masters.
			"job_role": {"Front desk", "Housekeeping", "Kitchen", "Service", "Maintenance", "Security", "Sales", "Office"},
			"skill":    {"Guest service", "Cooking", "Cleaning", "Electrical", "Plumbing", "Driving", "Accounts", "Computer"},
		}
		for kind, names := range simple {
			for _, n := range names {
				if _, err := tx.C("simple_master").Upsert(ctx, bson.M{"kind": kind, "name": n, "used_for": nil}, nil, nil); err != nil {
					return err
				}
			}
		}
		if err := store.AssignSysKeys(ctx, tx); err != nil { // the reasons and mode the app writes itself
			return err
		}
		if _, err := tx.C("employee_code_settings").Upsert(ctx, bson.M{"id": 1}, nil, nil); err != nil {
			return err
		}
		for _, s := range [][3]any{{"All", "EMP-", 4}, {"Permanent", "PRM-", 5}, {"Contractual", "CON-", 5}, {"Retainer", "RET-", 5}} {
			if _, err := tx.C("employee_code_series").Upsert(ctx, bson.M{"key": s[0]}, nil, bson.M{"prefix": s[1], "pad": s[2], "next_no": 1}); err != nil {
				return err
			}
		}
		for _, g := range []string{"G1", "G2", "G3", "G4", "G5", "G6"} {
			if _, err := tx.C("grade").Upsert(ctx, bson.M{"name": g}, nil, nil); err != nil {
				return err
			}
		}
		return timeConfig(ctx, tx)
	})
}

// seeder inserts and keeps the first error, so the sample data reads as a list of inserts.
type seeder struct {
	ctx context.Context
	tx  store.Q
	err error
}

// ins inserts a document and returns its id as a string ("" after an error).
func (s *seeder) ins(coll string, doc bson.M) string {
	if s.err != nil {
		return ""
	}
	id, err := s.tx.C(coll).Insert(s.ctx, doc)
	if err != nil {
		s.err = fmt.Errorf("insert into %s: %w", coll, err)
		return ""
	}
	str, _ := id.(string)
	return str
}

// SampleOrg loads the prototype's sample organisation. It does nothing if regions already exist.
// Returns the employee ids of the named users, keyed by role.
func SampleOrg(ctx context.Context, st *store.Store) (map[string]string, error) {
	if exists, _ := st.DB.C("region").Exists(ctx, nil); exists {
		return nil, nil
	}
	users := map[string]string{}
	err := st.Tx(ctx, func(tx store.Q) error {
		sd := &seeder{ctx: ctx, tx: tx}
		id := sd.ins
		regions := map[string]string{}
		for _, r := range []string{"North", "South", "West"} {
			regions[r] = id("region", bson.M{"name": r})
		}
		states := map[string]string{}
		for _, s := range [][2]string{{"Delhi", "North"}, {"Haryana", "North"}, {"Chandigarh", "North"}, {"Uttar Pradesh", "North"}, {"Karnataka", "South"}, {"Telangana", "South"}, {"Maharashtra", "West"}} {
			states[s[0]] = id("state", bson.M{"name": s[0], "region_id": regions[s[1]]})
		}
		for name, rule := range calc.LwfSeed {
			id("lwf_rule", bson.M{"state_id": states[name], "effective_from": "2026-04-01", "rule": rule})
		}
		cityRef := map[string]float64{"Delhi": 18500, "Mumbai": 16000}
		bandX := map[string][2]float64{"G1": {1.05, 1.4}, "G2": {1.4, 1.9}, "G3": {1.9, 2.8}, "G4": {2.8, 4.5}, "G5": {4.5, 7.5}, "G6": {7.5, 13}}
		r500 := func(v float64) float64 { return float64(int(v/500+0.5)) * 500 }
		cities, clusters := map[string]string{}, map[string]string{}
		for _, c := range [][2]string{{"Gurugram", "Haryana"}, {"Delhi", "Delhi"}, {"Bengaluru", "Karnataka"}, {"Hyderabad", "Telangana"}, {"Mumbai", "Maharashtra"}} {
			cities[c[0]] = id("city", bson.M{"name": c[0], "state_id": states[c[1]], "headcount_budget": 40})
			clusters[c[0]] = id("cluster", bson.M{"name": c[0] + " Cluster 1", "city_id": cities[c[0]]})
			ref := cityRef[c[0]]
			if ref == 0 {
				ref = 15500
			}
			for g, x := range bandX {
				id("pay_band", bson.M{"city_id": cities[c[0]], "grade": g, "min_gross": r500(ref * x[0]), "max_gross": r500(ref * x[1])})
			}
		}
		type loc struct{ name, city string }
		locList := []loc{{"Gurugram – Sector 29 Hub", "Gurugram"}, {"Gurugram – Golf Course Rd", "Gurugram"}, {"Delhi – Saket", "Delhi"},
			{"Bengaluru – Indiranagar", "Bengaluru"}, {"Hyderabad – Hitec City", "Hyderabad"}, {"Mumbai – Andheri East", "Mumbai"}}
		locs := map[string]string{}
		for _, l := range locList {
			locs[l.name] = id("location", bson.M{"name": l.name, "cluster_id": clusters[l.city], "headcount_budget": 12, "payroll_budget": 400000})
		}
		depts := map[string]string{}
		corp := map[string]bool{"Sales": true, "Finance": true, "People Ops": true, "Technology": true}
		desig := map[string][][2]any{
			"Front Office": {{"Front Desk Associate", []string{"G1", "G2"}}, {"Front Office Supervisor", []string{"G2", "G3"}}},
			"Housekeeping": {{"Housekeeping Attendant", []string{"G1"}}, {"Housekeeping Supervisor", []string{"G2", "G3"}}},
			"F&B":          {{"Kitchen Helper", []string{"G1"}}, {"F&B Supervisor", []string{"G2", "G3"}}},
			"Maintenance":  {{"Maintenance Engineer", []string{"G2", "G3", "G4"}}},
			"Operations":   {{"Property Manager", []string{"G4", "G5"}}, {"Area Manager", []string{"G5", "G6"}}},
			"Sales":        {{"Sales Executive", []string{"G3", "G4"}}, {"Sales Head", []string{"G6"}}},
			"Finance":      {{"Accountant", []string{"G3", "G4"}}, {"Finance Head", []string{"G6"}}},
			"People Ops":   {{"HR Executive", []string{"G3", "G4", "G5", "G6"}}, {"Payroll Executive", []string{"G4", "G5"}}},
			"Technology":   {{"Software Engineer", []string{"G3", "G4", "G5"}}},
		}
		desigIDs := map[string]string{}
		desigGrades := map[string][]string{}
		for _, d := range []string{"Front Office", "Housekeeping", "F&B", "Maintenance", "Operations", "Sales", "Finance", "People Ops", "Technology"} {
			depts[d] = id("department", bson.M{"name": d, "corporate": corp[d]})
			for _, x := range desig[d] {
				desigIDs[x[0].(string)] = id("designation", bson.M{"name": x[0], "department_id": depts[d], "grades": x[1]})
				desigGrades[x[0].(string)] = x[1].([]string)
			}
		}
		nightLocs := []string{locs["Gurugram – Sector 29 Hub"], locs["Delhi – Saket"], locs["Bengaluru – Indiranagar"], locs["Mumbai – Andheri East"]}
		for _, s := range [][5]any{{"G", "General", "09:00", "18:00", nil}, {"M", "Morning", "06:00", "14:00", nil}, {"E", "Evening", "14:00", "22:00", nil}, {"N", "Night", "22:00", "06:00", nightLocs}} {
			id("shift", bson.M{"code": s[0], "name": s[1], "start_time": s[2], "end_time": s[3], "break_minutes": 30, "location_ids": s[4]})
		}
		id("shift", bson.M{"code": "SPL", "name": "Split", "start_time": "07:00", "end_time": "20:00", "status": "Inactive", "inactive_since": "2026-04-01", "inactive_reason": "Discontinued"})
		if sd.err != nil {
			return sd.err
		}
		// Each shift's first version (Set shift), from 2000-01-01 so every date has one
		// (INSERT … SELECT … FROM shift ON CONFLICT DO NOTHING).
		shifts, err := tx.C("shift").Find(ctx, nil, options.Find().SetSort(bson.D{{Key: "seq", Value: 1}}))
		if err != nil {
			return err
		}
		for _, sh := range shifts {
			if _, err := tx.C("shift_version").Upsert(ctx, bson.M{"shift_id": sh["id"], "effective_from": "2000-01-01"}, nil,
				bson.M{"start_time": sh["start_time"], "end_time": sh["end_time"], "break_minutes": sh["break_minutes"], "rules": sh["rules"],
					"created_by": "System", "note": "Sample shift"}); err != nil {
				return err
			}
		}
		var defaultStructure string
		if r, err := tx.C("salary_structure").FindOne(ctx, bson.M{"is_default": true}); err != nil {
			return err
		} else if r != nil {
			defaultStructure, _ = r["id"].(string)
		}

		n := 1000
		emp := func(name, typ, dept, des, grade, location, shift, manager, doj string, gross float64) string {
			n++
			code := fmt.Sprintf("EMP-%d", n)
			mobile := fmt.Sprintf("98%08d", (n*7919311)%100000000)
			var mgr any
			if manager != "" { // NULLIF($10,'')
				mgr = manager
			}
			id("employee", bson.M{"id": code, "name": name, "type": typ, "doj": doj, "department_id": depts[dept], "designation_id": desigIDs[des],
				"grade": grade, "location_id": locs[location], "shift_code": shift, "manager_id": mgr, "status": "Active", "verified": true,
				"attendance_mode": map[bool]string{true: "Geo-fenced app", false: "Biometric"}[corp[dept]]})
			email := fmt.Sprintf("%s@mail.in", code)
			id("employee_personal", bson.M{"employee_id": code, "mobile": mobile, "email": email,
				"dob": fmt.Sprintf("%d-%02d-%02d", 1975+n%28, 1+n%12, 1+n%27), "gender": []string{"Male", "Female"}[n%2]})
			// A UAN for everyone (10010000 and the employee number, 12 digits): the sample staff are existing PF members, so the
			// payroll's UAN check (no UAN on record: salary held) holds nobody in the sample org.
			id("employee_gov_id", bson.M{"employee_id": code, "pan": fmt.Sprintf("ABCPX%04dK", n), "aadhaar": fmt.Sprintf("%012d", int64(n)*8191*3571),
				"uan": fmt.Sprintf("10010000%04d", n)})
			id("employee_bank", bson.M{"employee_id": code, "bank_name": "HDFC Bank", "account_no": fmt.Sprintf("5010%010d", n*977), "ifsc": "HDFC0001234", "from_month": doj[:7]})
			id("gross_history", bson.M{"employee_id": code, "from_month": doj[:7], "gross": gross, "reason": "Seed"})
			id("structure_map", bson.M{"employee_id": code, "from_date": "1900-01-01", "structure_id": defaultStructure})
			return code
		}
		mapRole := func(empID, role, acc string, locIDs []string, depts []string) {
			if locIDs == nil {
				locIDs = []string{}
			}
			if depts == nil {
				depts = []string{}
			}
			id("user_role_map", bson.M{"employee_id": empID, "role_name": role, "access": acc, "location_ids": locIDs, "departments": depts, "created_by": "Seed"})
		}
		hub, golf, saket := locs["Gurugram – Sector 29 Hub"], locs["Gurugram – Golf Course Rd"], locs["Delhi – Saket"]
		north := []string{hub, golf, saket}

		// Named users (people who sign in to approve), with the role each holds.
		users["CHRO"] = emp("Anjali Verma", "Permanent", "People Ops", "HR Executive", "G6", "Gurugram – Sector 29 Hub", "G", "", "2021-04-01", 300000)
		users["HR Admin"] = emp("Sandeep Rawat", "Permanent", "People Ops", "HR Executive", "G6", "Gurugram – Sector 29 Hub", "G", users["CHRO"], "2022-01-10", 150000)
		users["HRBP Corporate"] = emp("Pooja Malhotra", "Permanent", "People Ops", "HR Executive", "G6", "Gurugram – Sector 29 Hub", "G", users["CHRO"], "2022-03-01", 140000)
		users["HRBP"] = emp("Meera Kulkarni", "Permanent", "People Ops", "HR Executive", "G5", "Gurugram – Sector 29 Hub", "G", users["HRBP Corporate"], "2023-02-01", 90000)
		users["Operations Head"] = emp("Aditya Mehra", "Permanent", "Operations", "Area Manager", "G6", "Gurugram – Sector 29 Hub", "G", users["CHRO"], "2021-06-01", 250000)
		users["City Head"] = emp("Harish Gupta", "Permanent", "Operations", "Area Manager", "G6", "Gurugram – Sector 29 Hub", "G", users["Operations Head"], "2022-05-01", 180000)
		users["Cluster Manager"] = emp("Vivek Menon", "Permanent", "Operations", "Area Manager", "G5", "Gurugram – Sector 29 Hub", "G", users["City Head"], "2022-07-01", 120000)
		users["Property Manager"] = emp("Rakesh Bajaj", "Permanent", "Operations", "Property Manager", "G5", "Gurugram – Sector 29 Hub", "G", users["Cluster Manager"], "2023-01-15", 85000)
		users["Line Manager"] = emp("Anil Kapoor", "Permanent", "Front Office", "Front Office Supervisor", "G3", "Gurugram – Sector 29 Hub", "G", users["Property Manager"], "2023-04-01", 42000)
		users["Function Head"] = emp("Rohit Khanna", "Permanent", "Sales", "Sales Head", "G6", "Gurugram – Sector 29 Hub", "G", users["CHRO"], "2022-09-01", 200000)
		users["Payroll"] = emp("Sunita Rao", "Permanent", "People Ops", "Payroll Executive", "G4", "Gurugram – Sector 29 Hub", "G", users["HR Admin"], "2023-06-01", 60000)
		users["Payroll Manager"] = emp("Deepak Arora", "Permanent", "People Ops", "Payroll Executive", "G5", "Gurugram – Sector 29 Hub", "G", users["HR Admin"], "2022-11-01", 95000)
		users["Finance Head"] = emp("Mahesh Iyer", "Permanent", "Finance", "Finance Head", "G6", "Gurugram – Sector 29 Hub", "G", users["CHRO"], "2021-08-01", 250000)

		mapRole(users["HR Admin"], "HR Admin", "all", nil, nil)
		mapRole(users["CHRO"], "CHRO", "all", nil, nil)
		mapRole(users["HRBP Corporate"], "HRBP Corporate", "all", nil, nil)
		mapRole(users["HRBP"], "HRBP", "locs", north, nil)
		mapRole(users["Operations Head"], "Operations Head", "locs", north, nil)
		mapRole(users["City Head"], "City Head", "locs", []string{hub, golf}, nil)
		mapRole(users["Cluster Manager"], "Cluster Manager", "locs", north, nil)
		mapRole(users["Property Manager"], "Property Manager", "locs", []string{hub}, nil)
		mapRole(users["Function Head"], "Function Head", "all", nil, []string{"Sales"})
		mapRole(users["Payroll"], "Payroll", "all", nil, nil)
		mapRole(users["Payroll Manager"], "Payroll Manager", "all", nil, nil)
		mapRole(users["Finance Head"], "Finance Head", "all", nil, nil)

		// Sample staff across properties.
		rng := rand.New(rand.NewSource(11))
		first := []string{"Aarav", "Priya", "Rohit", "Neha", "Imran", "Kavya", "Suresh", "Anita", "Vikram", "Lata", "Arjun", "Isha", "Manoj", "Divya", "Karan", "Faisal", "Ritu", "Nitin", "Swati", "Rahul"}
		last := []string{"Sharma", "Verma", "Khan", "Iyer", "Reddy", "Singh", "Patil", "Nair", "Gupta", "Das", "Yadav", "Joshi", "Mehta", "Rao"}
		opsDesig := map[string]string{"Front Office": "Front Desk Associate", "Housekeeping": "Housekeeping Attendant", "F&B": "Kitchen Helper", "Maintenance": "Maintenance Engineer"}
		opsDepts := []string{"Front Office", "Housekeeping", "F&B", "Maintenance"}
		for i := 0; i < 32; i++ {
			l := locList[i%len(locList)]
			d := opsDepts[rng.Intn(len(opsDepts))]
			ds := opsDesig[d]
			g := desigGrades[ds][rng.Intn(len(desigGrades[ds]))]
			typ := []string{"Permanent", "Permanent", "Permanent", "Contractual", "Retainer"}[rng.Intn(5)]
			mgr := users["Line Manager"]
			if l.name != "Gurugram – Sector 29 Hub" {
				mgr = users["Property Manager"]
			}
			shift := []string{"G", "M", "E"}[rng.Intn(3)]
			gross := float64(16000+rng.Intn(9000)) - float64((16000+rng.Intn(9000))%100)
			emp(first[rng.Intn(len(first))]+" "+last[rng.Intn(len(last))], typ, d, ds, g, l.name, shift, mgr,
				fmt.Sprintf("202%d-%02d-%02d", 3+rng.Intn(3), 1+rng.Intn(12), 1+rng.Intn(27)), gross)
		}
		for i := 0; i < 8; i++ {
			d := []string{"Sales", "Finance", "Technology", "Sales"}[i%4]
			ds := map[string]string{"Sales": "Sales Executive", "Finance": "Accountant", "Technology": "Software Engineer"}[d]
			mgr := users["Function Head"]
			if d != "Sales" {
				mgr = users["Finance Head"]
			}
			emp(first[rng.Intn(len(first))]+" "+last[rng.Intn(len(last))], "Permanent", d, ds, desigGrades[ds][0], "Gurugram – Sector 29 Hub", "G", mgr,
				fmt.Sprintf("2024-%02d-10", 1+i), float64(30000+rng.Intn(30000)/100*100))
		}
		if sd.err != nil {
			return sd.err
		}
		_, err = tx.C("employee_code_series").Update(ctx, bson.M{"key": "All"}, bson.M{"$set": bson.M{"next_no": n + 1}})
		return err
	})
	if err == nil {
		err = LinkCalendars(ctx, st)
	}
	return users, err
}

// Starting logins for the sample organisation, so a fresh install can be opened straight away.
// They are set only when the account has no password yet, and skip the strength rules, so
// change them (Forgot password) before real use.
const (
	StartingAdmin    = "EMP-1002" // Sandeep Rawat, HR Admin
	StartingUser     = "EMP-1020" // Karan Khan, employee
	StartingPassword = "pass1234"
)

func StartingLogins(ctx context.Context, st *store.Store) ([]string, error) {
	var set []string
	err := st.Tx(ctx, func(tx store.Q) error {
		set = nil // the transaction may run again
		for _, id := range []string{StartingAdmin, StartingUser} {
			// INSERT … SELECT id, $2, $3 FROM employee WHERE id=$1 ON CONFLICT (employee_id) DO NOTHING
			if ok, err := tx.C("employee").Exists(ctx, bson.M{"id": id}); err != nil {
				return err
			} else if !ok {
				continue
			}
			salt := access.RandomHex(16)
			ins, err := tx.C("auth_credential").Upsert(ctx, bson.M{"employee_id": id}, nil, bson.M{"salt": salt, "hash": access.HashPassword(StartingPassword, salt)})
			if err != nil {
				return err
			}
			if ins {
				set = append(set, id)
			}
		}
		return nil
	})
	return set, err
}
