package seed

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/store"
)

// FirstAdminInput is what `hrms create-admin` takes. Everything else the first HR Admin needs is made here.
type FirstAdminInput struct {
	Name   string
	Mobile string // 10 digits: the OTP goes here
	Email  string // optional
	ID     string // optional Employee ID; blank takes the next one from the "All" series (EMP-0001)
	State  string // where the head office is; default Haryana
	City   string // default Gurugram
	// Password is optional. Without it the person activates the account with an OTP, which needs the SMS set up;
	// with it they can sign in straight away (and should change it).
	Password string
}

var reMobile = regexp.MustCompile(`^[6-9]\d{9}$`)

// CheckPassword is the rule for a password given on the command line: 10 or more characters with an upper-case
// letter, a lower-case letter, a digit and a symbol.
func CheckPassword(p string) error {
	var up, lo, dg, sy bool
	for _, r := range p {
		switch {
		case unicode.IsUpper(r):
			up = true
		case unicode.IsLower(r):
			lo = true
		case unicode.IsDigit(r):
			dg = true
		default:
			sy = true
		}
	}
	if len(p) < 10 || !up || !lo || !dg || !sy {
		return errors.New("the password needs 10 or more characters with an upper-case letter, a lower-case letter, a digit and a symbol")
	}
	return nil
}

// FirstAdmin makes the first HR Admin of a new installation: the head-office place (region, state, city, cluster,
// location, department, designation, General shift) HR renames later in Masters, and one employee with the HR Admin
// role. It needs `hrms seed` first (without -sample), and refuses once any HR Admin exists, so it cannot be used
// later to add an admin behind the app's back. It returns the Employee ID.
func FirstAdmin(ctx context.Context, st *store.Store, in FirstAdminInput) (string, error) {
	in.Name = strings.TrimSpace(in.Name)
	mobile := strings.Join(strings.FieldsFunc(in.Mobile, func(r rune) bool { return r < '0' || r > '9' }), "")
	mobile = strings.TrimPrefix(mobile, "91")
	if in.Name == "" {
		return "", errors.New("the name is required")
	}
	if !reMobile.MatchString(mobile) {
		return "", errors.New("the mobile number must be a 10-digit Indian mobile number: the OTP is sent to it")
	}
	in.Password = strings.TrimSpace(in.Password) // a space or new line pasted with the secret must not become part of it
	if in.Password != "" {
		if err := CheckPassword(in.Password); err != nil {
			return "", err
		}
	}
	if in.State == "" {
		in.State = "Haryana"
	}
	if in.City == "" {
		in.City = "Gurugram"
	}
	var code string
	err := st.Tx(ctx, func(tx store.Q) error {
		code = "" // the transaction may run again
		if ok, err := tx.C("user_role_map").Exists(ctx, bson.M{"role_name": "HR Admin", "active": true}); err != nil {
			return err
		} else if ok {
			return errors.New("an HR Admin already exists. Add more people from the app (Access), not from here")
		}
		series, err := tx.C("employee_code_series").Lock(ctx, bson.M{"key": "All"})
		if err != nil {
			return err
		}
		if series == nil {
			return errors.New("the reference data is not loaded: run `hrms seed` (without -sample) first")
		}
		code = strings.ToUpper(strings.TrimSpace(in.ID))
		if code == "" {
			n, _ := series["next_no"].(int32)
			if n == 0 {
				if f, ok := series["next_no"].(int64); ok {
					n = int32(f)
				} else {
					n = 1
				}
			}
			pad, _ := series["pad"].(int32)
			prefix, _ := series["prefix"].(string)
			code = fmt.Sprintf("%s%0*d", prefix, int(pad), n)
			if _, err := tx.C("employee_code_series").Update(ctx, bson.M{"key": "All"}, bson.M{"$set": bson.M{"next_no": n + 1}}); err != nil {
				return err
			}
		}
		if ok, err := tx.C("employee").Exists(ctx, bson.M{"_id": code}); err != nil {
			return err
		} else if ok {
			return fmt.Errorf("the Employee ID %s is already taken", code)
		}
		sd := &seeder{ctx: ctx, tx: tx}
		// find returns the id of the first document matching, or makes it.
		find := func(coll string, match, extra bson.M) string {
			if sd.err != nil {
				return ""
			}
			if r, err := tx.C(coll).FindOne(ctx, match); err != nil {
				sd.err = err
				return ""
			} else if r != nil {
				id, _ := r["id"].(string)
				return id
			}
			doc := bson.M{}
			for k, v := range match {
				doc[k] = v
			}
			for k, v := range extra {
				doc[k] = v
			}
			return sd.ins(coll, doc)
		}
		region := find("region", bson.M{"name": "Head Office"}, nil)
		state := find("state", bson.M{"name": in.State}, bson.M{"region_id": region})
		city := find("city", bson.M{"name": in.City}, bson.M{"state_id": state})
		cluster := find("cluster", bson.M{"name": in.City + " Cluster 1"}, bson.M{"city_id": city})
		location := find("location", bson.M{"name": "Head Office"}, bson.M{"cluster_id": cluster})
		dept := find("department", bson.M{"name": "People Ops"}, bson.M{"corporate": true})
		desig := find("designation", bson.M{"name": "HR Admin"}, bson.M{"department_id": dept, "grades": []string{"G1", "G2", "G3", "G4", "G5", "G6"}})
		shiftID := find("shift", bson.M{"code": "G"}, bson.M{"name": "General", "start_time": "09:00", "end_time": "18:00", "break_minutes": 30})
		if sd.err != nil {
			return sd.err
		}
		if _, err := tx.C("shift_version").Upsert(ctx, bson.M{"shift_id": shiftID, "effective_from": "2000-01-01"}, nil,
			bson.M{"start_time": "09:00", "end_time": "18:00", "break_minutes": 30, "created_by": "System", "note": "First admin"}); err != nil {
			return err
		}
		if _, err := store.ShiftMapsFromLocations(ctx, tx); err != nil { // G applies to all departments at all locations
			return err
		}
		sd.ins("employee", bson.M{"id": code, "name": in.Name, "type": "Permanent", "doj": time.Now().Format("2006-01-02"),
			"department_id": dept, "designation_id": desig, "grade": "G6", "location_id": location, "shift_code": "G",
			"status": "Active", "verified": true, "attendance_mode": "Manager marked"})
		sd.ins("employee_personal", bson.M{"employee_id": code, "mobile": mobile, "email": strings.TrimSpace(in.Email)})
		sd.ins("user_role_map", bson.M{"employee_id": code, "role_name": "HR Admin", "access": "all", "location_ids": []string{}, "departments": []string{}, "created_by": "First admin"})
		if sd.err != nil {
			return sd.err
		}
		if r, err := tx.C("salary_structure").FindOne(ctx, bson.M{"is_default": true}); err != nil {
			return err
		} else if r != nil {
			if _, err := tx.C("structure_map").Insert(ctx, bson.M{"employee_id": code, "from_date": "1900-01-01", "structure_id": r["id"]}); err != nil {
				return err
			}
		}
		if in.Password != "" {
			salt := access.RandomHex(16)
			if _, err := tx.C("auth_credential").Upsert(ctx, bson.M{"employee_id": code}, nil, bson.M{"salt": salt, "hash": access.HashPassword(in.Password, salt)}); err != nil {
				return err
			}
		}
		return nil
	})
	return code, err
}

// SetPassword sets the sign-in password of an existing employee (run as `hrms set-password -id EMP-0001`, the
// password from HRMS_ADMIN_PASSWORD). It is the way back in when the first admin's password was stored wrong and
// SMS is not set up yet. It clears wrong tries and any lock, and signs the person out everywhere.
func SetPassword(ctx context.Context, st *store.Store, id, password string) error {
	id = strings.ToUpper(strings.TrimSpace(id))
	password = strings.TrimSpace(password)
	if id == "" {
		return errors.New("the Employee ID is required")
	}
	if err := CheckPassword(password); err != nil {
		return err
	}
	return st.Tx(ctx, func(tx store.Q) error {
		if ok, err := tx.C("employee").Exists(ctx, bson.M{"_id": id}); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("there is no employee %s", id)
		}
		salt := access.RandomHex(16)
		if _, err := tx.C("auth_credential").Upsert(ctx, bson.M{"employee_id": id},
			bson.M{"salt": salt, "hash": access.HashPassword(password, salt), "fails": 0, "locked_until": nil, "updated_at": tx.Now()}, nil); err != nil {
			return err
		}
		_, err := tx.C("auth_session").DeleteMany(ctx, bson.M{"employee_id": id})
		return err
	})
}
