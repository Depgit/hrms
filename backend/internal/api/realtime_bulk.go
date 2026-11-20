package api

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// "Attendance code" column of Bulk update › Employee master update and Import employees (16 Nov 2026): the
// employee's code in the Realtime attendance portal (realtime_pull.go), its EmpCode or CardNo. Optional; blank keeps
// the current code. Leading zeros do not matter (00001050 = 1050), and no two employees may share a code. It is
// written with the rest of the row; a row with a bad code writes nothing.

const attCodeCol = "Attendance code"

var reAttCode = regexp.MustCompile(`^[A-Za-z0-9/_-]{1,20}$`)

const attCodeHint = " Attendance code is the employee's code in the Realtime attendance portal (EmpCode or CardNo; leading zeros do not matter). Punches pulled from the portal are matched by it (by the number of the Employee ID, EMP-1050 = 1050, only when that setting is on). Attendance › Punch machine setup › Match portal codes proposes codes from the portal's employee list. No two employees may share a code."

// attCodeWire adds the column at the end of a template and checks and writes it around the template's row
// function. isNew: Import employees (the employee is created by the row).
func attCodeWire(t *bulkTemplate, isNew bool) {
	t.Cols = append(t.Cols, attCodeCol)
	t.Optional = append(t.Optional, attCodeCol)
	t.Hint += attCodeHint
	prev := t.row
	t.row = func(s *Server, ctx context.Context, tx store.Q, c *Ctx, r bulkRow, apply bool) (bulkResult, error) {
		code := strings.TrimSpace(r.V[attCodeCol])
		if code == "" {
			return prev(s, ctx, tx, c, r, apply)
		}
		var pre bulkResult
		var cur string
		if isNew {
			pre.ID, pre.Name = strings.ToUpper(strings.TrimSpace(r.V["Employee Code"])), strings.TrimSpace(r.V["Full Name"])
		} else {
			if _, ok := bulkEmployee(ctx, tx, c, r, &pre); !ok {
				return prev(s, ctx, tx, c, r, apply) // says why, as before
			}
			if e, _ := tx.C("employee").FindOne(ctx, bson.M{"_id": pre.ID}, options.FindOne().SetProjection(bson.M{"attendance_code": 1})); e != nil {
				cur = str(e["attendance_code"])
			}
		}
		if msg, err := attCodeProblem(ctx, tx, code, pre.ID); err != nil {
			return pre, err
		} else if msg != "" {
			pre.Errors = append(pre.Errors, msg)
			return pre, nil
		}
		res, err := prev(s, ctx, tx, c, r, apply)
		if err != nil || len(res.Errors) > 0 || res.ID == "" || (!isNew && rtNorm(cur) == rtNorm(code)) {
			return res, err
		}
		if _, err := tx.C("employee").Update(ctx, bson.M{"_id": res.ID}, bson.M{"$set": bson.M{"attendance_code": code}}); err != nil {
			return res, err
		}
		res.Changes = append(res.Changes, bulkChange{Field: attCodeCol, From: cur, To: code})
		return res, nil
	}
}

// attCodeProblem says what is wrong with a code for employee self ("" when it is fine).
func attCodeProblem(ctx context.Context, q store.Q, code, self string) (string, error) {
	if !reAttCode.MatchString(code) {
		return "Attendance code is letters and digits (up to 20)", nil
	}
	n := rtNorm(code)
	pat := "^0*" + regexp.QuoteMeta(n) + "$"
	if n == "0" {
		pat = "^0+$"
	}
	f := bson.M{"attendance_code": bson.M{"$regex": pat, "$options": "i"}}
	if self != "" {
		f["_id"] = bson.M{"$ne": self}
	}
	other, err := q.C("employee").FindOne(ctx, f, options.FindOne().SetProjection(bson.M{"_id": 1, "name": 1}))
	if err != nil {
		return "", err
	}
	if other != nil {
		return fmt.Sprintf("Attendance code %s is already %s's (%s)", code, str(other["name"]), str(other["id"])), nil
	}
	return "", nil
}
