package api

// Approver names tab: pick the name in a cell (owner, 26 Nov 2026). Each cell of the Properties, Departments and
// Company-wide tables is a picker of active employees; saving maps the person on Access › Role mapping
// (user_role_map), so they get that role's access, with the same code as the approver-names bulk upload
// (apprSlotSet in bulk_apprnames.go).
//
// Changing a cell that has a name moves the slot: the place leaves the old holder's mapping (the mapping is switched
// off when it was their only place, as Edit on Role mapping does), then the new person gets it, in one transaction.
// "Not mapped" clears the slot the same way. A holder who has the place by all locations, by cluster, city or state,
// or by every department is refused: "Change it on Access › Role mapping".
//
// Right: Role mapping at Edit (acc.users 2), as for the bulk upload. Every change is audited "(Approver names)".

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

const apprCellHow = "Approver names"

// approverSlotPut is PUT /api/approvals/approvers/slot {role, level, place (its code), employeeId|null}.
func (s *Server) approverSlotPut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("acc.users", 2); err != nil {
		return nil, err
	}
	var b struct {
		Role       string  `json:"role"`
		Level      string  `json:"level"`
		Place      string  `json:"place"`
		EmployeeID *string `json:"employeeId"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	changes := []bulkChange{}
	err := s.st.Tx(ctx, func(tx store.Q) error {
		sl, msg, err := apprSlotFind(ctx, tx, strings.TrimSpace(b.Role), strings.TrimSpace(b.Level), strings.TrimSpace(b.Place), "")
		if err != nil {
			return err
		}
		if msg != "" {
			return bad("%s", msg)
		}
		// The person picked: checked before anything moves.
		var e store.Row
		target := ""
		if b.EmployeeID != nil && strings.TrimSpace(*b.EmployeeID) != "" {
			typed := strings.TrimSpace(*b.EmployeeID)
			if e, msg, err = apprSlotPerson(ctx, tx, strings.ToUpper(typed), typed); err != nil {
				return err
			}
			if msg != "" {
				return bad("%s", msg)
			}
			if str(e["status"]) != "Active" {
				return bad("%s (%s) is not active. Only active employees can hold a role", str(e["name"]), str(e["id"]))
			}
			if err := roleAssignable(ctx, tx, sl.Role); err != nil {
				return err
			}
			target = str(e["id"])
		}
		out, msg, err := apprSlotClear(ctx, tx, c, sl, target)
		if err != nil {
			return err
		}
		if msg != "" {
			return bad("%s", msg)
		}
		changes = append(changes, out...)
		if e == nil {
			return nil
		}
		ch, msg, err := apprSlotSet(ctx, tx, c, sl, e, apprCellHow)
		if err != nil {
			return err
		}
		if msg != "" {
			return bad("%s", msg)
		}
		if ch != nil {
			changes = append(changes, *ch)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"changes": changes}, nil
}

// apprSlotClear takes the slot away from whoever holds it now, until nobody (or keep, the person about to be named)
// holds it: the place leaves each holder's mapping of the role, and a mapping left with no place is switched off.
func apprSlotClear(ctx context.Context, tx store.Q, c *Ctx, sl apprSlot, keep string) ([]bulkChange, string, error) {
	out := []bulkChange{}
	unit := map[string]string{slotProperty: "loc|" + sl.PlaceID, slotDept: "dept|" + sl.Place, slotCorpDept: "hdept|" + sl.Place, slotCompany: "co|" + sl.Role}[sl.Level]
	for range 20 {
		holder, err := apprSlotHolder(ctx, tx, sl)
		if err != nil {
			return nil, "", err
		}
		if holder.ID == "" || holder.ID == keep {
			return out, "", nil
		}
		who := holder.Name + " (" + holder.ID + ")"
		rows, err := tx.C("user_role_map").Find(ctx, bson.M{"employee_id": holder.ID, "role_name": sl.Role, "active": true})
		if err != nil {
			return nil, "", err
		}
		moved := false
		for _, x := range rows {
			m := accessRoleMapOf(x)
			u, err := units(ctx, tx, m)
			if err != nil {
				return nil, "", err
			}
			if !containsStr(u, unit) {
				continue
			}
			byHand := func(how string) ([]bulkChange, string, error) {
				return nil, fmt.Sprintf("%s holds %s %s. Change it on Access › Role mapping", who, sl.Role, how), nil
			}
			set := bson.M{}
			switch sl.Level {
			case slotProperty:
				if m.Access != "locs" {
					if m.Access == "all" {
						return byHand("for all locations")
					}
					return byHand("by " + strings.TrimSuffix(m.Access, "s"))
				}
				rest := apprWithout(m.LocationIDs, sl.PlaceID)
				if len(rest) == 0 {
					set["active"] = false
				} else {
					set["location_ids"] = rest
				}
			case slotDept, slotCorpDept:
				if !containsStr(m.Departments, sl.Place) {
					return byHand("for every department")
				}
				rest := apprWithout(m.Departments, sl.Place)
				if len(rest) == 0 {
					set["active"] = false
				} else {
					set["departments"] = rest
				}
			default:
				set["active"] = false
			}
			row, err := tx.C("user_role_map").FindOneAndUpdate(ctx, bson.M{"_id": m.ID}, bson.M{"$set": set})
			if err != nil {
				return nil, "", err
			}
			note := sl.Place + " taken out of " + holder.Name + "'s " + sl.Role + " mapping"
			if set["active"] == false {
				note = holder.Name + "'s " + sl.Role + " mapping switched off: it had no other place"
			}
			accessStripArea(x)
			accessStripArea(row)
			if err := audit(ctx, tx, c, "Changed role mapping ("+apprCellHow+")", "user_role_map", m.ID, x, row); err != nil {
				return nil, "", err
			}
			out = append(out, bulkChange{Field: sl.Label, From: who, To: "Not mapped", Note: note})
			moved = true
		}
		if !moved { // a holder the directory sees but no mapping of theirs covers the place
			return nil, fmt.Sprintf("%s is %s now. Change it on Access › Role mapping", sl.Label, who), nil
		}
	}
	return nil, sl.Label + " is held by too many mappings. Change it on Access › Role mapping", nil
}

func apprWithout(l []string, v string) []string {
	out := []string{}
	for _, x := range l {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}
