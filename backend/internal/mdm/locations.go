package mdm

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// Locations and CMS properties. HRMS calls a property a "location"; location.property_id links one to its CMS
// property. The link is set once and never changes; a linked location's name follows the CMS name and its status
// follows the property's (retired when the CMS deactivates it).

func noCase(s string) bson.M {
	return bson.M{"$regex": "^" + regexp.QuoteMeta(s) + `\z`, "$options": "i"}
}

func (s *Service) renameLocation(ctx context.Context, tx store.Q, id, name string) error {
	if s.o.Hooks.Rename != nil {
		return s.o.Hooks.Rename(ctx, tx, id, name)
	}
	return builtinRename(ctx, tx, id, name)
}

func (s *Service) retireLocation(ctx context.Context, tx store.Q, id, since, reason string) error {
	if s.o.Hooks.Retire != nil {
		return s.o.Hooks.Retire(ctx, tx, id, since, reason)
	}
	return builtinRetire(ctx, tx, id, since, reason)
}

func (s *Service) reactivateLocation(ctx context.Context, tx store.Q, id string) error {
	if s.o.Hooks.Reactivate != nil {
		return s.o.Hooks.Reactivate(ctx, tx, id)
	}
	return builtinReactivate(ctx, tx, id)
}

// Actor is who made a change, as the audit log shows it: "Name (EMP-1)" and the role view; zero is the system.
type Actor struct {
	Name string // "Asha Rao (EMP-1001)"
	Role string
}

// audit writes to the append-only audit log as the app's audit() does (actor "System" for the system).
func audit(ctx context.Context, tx store.Q, by Actor, action, id string, before, after any) error {
	j := func(v any) any {
		if v == nil {
			return nil
		}
		b, _ := json.Marshal(v)
		return b
	}
	actor, role := "System", any(nil)
	if by.Name != "" {
		actor = by.Name
		if by.Role != "" {
			role = by.Role
		}
	}
	_, err := tx.C("audit_log").Insert(ctx, bson.M{"actor": actor, "actor_role": role, "action": action, "entity": "location",
		"entity_id": id, "before": j(before), "after": j(after)})
	return err
}

// builtinRename is the Masters screen's rename of a location (saveMaster + renameRefs for "location"): the name,
// the job changes waiting for their date that move someone to it, and the audit entry.
func builtinRename(ctx context.Context, tx store.Q, id, name string) error {
	before, err := tx.C("location").Lock(ctx, bson.M{"_id": id})
	if err != nil || before == nil {
		return err
	}
	after, err := tx.C("location").FindOneAndUpdate(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"name": name}})
	if err != nil {
		return err
	}
	n, err := tx.C("employee_change").UpdateMany(ctx, bson.M{"state": "Scheduled", "apply_column": "location_id", "apply_value": id},
		bson.M{"$set": bson.M{"new_value": name}})
	if err != nil {
		return err
	}
	logged := store.Row{}
	for k, v := range after {
		logged[k] = v
	}
	if n > 0 {
		logged["references_updated"] = map[string]int64{"scheduled job changes": n}
	}
	return audit(ctx, tx, Actor{}, "Changed location", id, before, logged)
}

// builtinRetire is retireMaster for a location: inactive from the date with the reason, at once when the date
// has come (the maintenance sweep retires a later date when it comes).
func builtinRetire(ctx context.Context, tx store.Q, id, since, reason string) error {
	before, err := tx.C("location").Lock(ctx, bson.M{"_id": id})
	if err != nil || before == nil {
		return err
	}
	set := bson.M{"inactive_since": since, "inactive_reason": reason}
	if since <= store.DateOf(tx.Now()) {
		set["status"] = "Inactive"
	}
	out, err := tx.C("location").FindOneAndUpdate(ctx, bson.M{"_id": id}, bson.M{"$set": set})
	if err != nil {
		return err
	}
	n, err := tx.C("employee").Count(ctx, bson.M{"location_id": id, "status": "Active"})
	if err != nil {
		return err
	}
	out["activeEmployeesUsingIt"] = n
	return audit(ctx, tx, Actor{}, "Marked location inactive", id, before, out)
}

// builtinReactivate is reactivateMaster for a location.
func builtinReactivate(ctx context.Context, tx store.Q, id string) error {
	before, err := tx.C("location").Lock(ctx, bson.M{"_id": id})
	if err != nil || before == nil {
		return err
	}
	if _, err := tx.C("location").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"status": "Active", "inactive_since": nil, "inactive_reason": nil}}); err != nil {
		return err
	}
	return audit(ctx, tx, Actor{}, "Reactivated location", id, nil, nil)
}

// PropertiesFromCMS says whether locations may only be created by linking a CMS property (the switch in the CMS
// connection settings).
func PropertiesFromCMS(ctx context.Context, q store.Q) (bool, error) {
	r, err := q.C(collSetting).FindOne(ctx, bson.M{"_id": 1})
	if err != nil || r == nil {
		return false, err
	}
	return r["properties_from_cms"] == true, nil
}

// PropertyByID reads one property of the local copy (nil when unknown).
func PropertyByID(ctx context.Context, q store.Q, pid string) (store.Row, error) {
	return q.C(collProperty).FindOne(ctx, bson.M{"property_id": pid})
}

// CheckLinkable says why a property cannot be given to a new location: unknown, inactive in the CMS, or already
// linked. It returns the property.
func CheckLinkable(ctx context.Context, q store.Q, pid string) (store.Row, error) {
	pid = strings.TrimSpace(pid)
	if pid == "" {
		return nil, userErr(400, "Pick a CMS property")
	}
	p, err := PropertyByID(ctx, q, pid)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, userErr(400, "CMS property %s is not known to HRMS yet. It arrives with the CMS's events or the nightly reconciliation", pid)
	}
	if str(p["status"]) != "Active" {
		return nil, userErr(400, "CMS property %s (%s) is inactive in the CMS", pid, str(p["name"]))
	}
	l, err := q.C("location").FindOne(ctx, bson.M{"property_id": pid}, options.FindOne().SetProjection(bson.M{"name": 1}))
	if err != nil {
		return nil, err
	}
	if l != nil {
		return nil, userErr(409, "CMS property %s is already linked to location “%s”", pid, str(l["name"]))
	}
	return p, nil
}

// LinkLocation links an existing location to a CMS property, once: the location then takes the CMS name, and
// retires if the property is inactive. Inside tx. It returns notes about what could not follow (a name clash).
func (s *Service) LinkLocation(ctx context.Context, tx store.Q, locID, pid string, by Actor) ([]string, error) {
	pid = strings.TrimSpace(pid)
	loc, err := tx.C("location").Lock(ctx, bson.M{"_id": locID})
	if err != nil {
		return nil, err
	}
	if loc == nil {
		return nil, userErr(404, "Location not found")
	}
	if cur := str(loc["property_id"]); cur != "" {
		if cur == pid {
			return nil, nil
		}
		return nil, userErr(409, "Location “%s” is linked to CMS property %s. A link is set once and cannot change", str(loc["name"]), cur)
	}
	if pid == "" {
		return nil, userErr(400, "Pick a CMS property")
	}
	p, err := PropertyByID(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, userErr(400, "CMS property %s is not known to HRMS yet. It arrives with the CMS's events or the nightly reconciliation", pid)
	}
	if other, err := tx.C("location").FindOne(ctx, bson.M{"property_id": pid}); err != nil {
		return nil, err
	} else if other != nil {
		return nil, userErr(409, "CMS property %s is already linked to location “%s”", pid, str(other["name"]))
	}
	after, err := tx.C("location").FindOneAndUpdate(ctx, bson.M{"_id": locID}, bson.M{"$set": bson.M{"property_id": pid}})
	if err != nil {
		return nil, err
	}
	if err := audit(ctx, tx, by, "Linked location to CMS property", locID, loc, after); err != nil {
		return nil, err
	}
	return s.follow(ctx, tx, after, propertyOf(p))
}

func propertyOf(p store.Row) Property {
	return Property{PropertyID: str(p["property_id"]), Name: str(p["name"]), City: str(p["city"]), State: str(p["state"]), Status: str(p["status"])}
}

// LockReason is the note a linked location carries in the Masters list ("cmsLocked"), or "" when not linked.
func LockReason(propertyID, cmsName string) string {
	if propertyID == "" {
		return ""
	}
	if cmsName == "" {
		return fmt.Sprintf("Linked to CMS property %s: its name and status come from the CMS", propertyID)
	}
	return fmt.Sprintf("Linked to CMS property %s (“%s” in the CMS): its name and status come from the CMS", propertyID, cmsName)
}

// LocationFlags adds "cmsLocked" to the rows of the Locations master that are linked to a CMS property.
func LocationFlags(ctx context.Context, q store.Q, rows []store.Row) error {
	var pids []any
	for _, r := range rows {
		if p := str(r["property_id"]); p != "" {
			pids = append(pids, p)
		}
	}
	if len(pids) == 0 {
		return nil
	}
	ps, err := q.C(collProperty).Find(ctx, bson.M{"property_id": bson.M{"$in": pids}}, options.Find().SetProjection(bson.M{"property_id": 1, "name": 1}))
	if err != nil {
		return err
	}
	names := map[string]string{}
	for _, p := range ps {
		names[str(p["property_id"])] = str(p["name"])
	}
	for _, r := range rows {
		if p := str(r["property_id"]); p != "" {
			r["cmsLocked"] = LockReason(p, names[p])
		}
	}
	return nil
}

// CheckLocation holds the masters lock for the Locations master, for the Masters screen and the Master update
// workbook (master_rules.go checkMaster calls it for t == "location"). before is nil for a new location; vals holds
// what changes. A new location must name an unlinked, active CMS property when "Properties come from the CMS" is
// on (its name is then the CMS name); a linked location's name is the CMS's; property_id is never changed here.
func CheckLocation(ctx context.Context, tx store.Q, before store.Row, vals map[string]any) error {
	pid, hasPID := vals["property_id"].(string)
	pid = strings.TrimSpace(pid)
	_, present := vals["property_id"]
	if before != nil {
		if present {
			if cur := str(before["property_id"]); cur != "" {
				return userErr(400, "Location “%s” is linked to CMS property %s. A link is set once and cannot change", str(before["name"]), cur)
			}
			return userErr(400, "Link a location to its CMS property with Link to CMS property")
		}
		cur := str(before["property_id"])
		if name, ok := vals["name"].(string); ok && cur != "" {
			p, err := PropertyByID(ctx, tx, cur)
			if err != nil {
				return err
			}
			if p == nil || str(p["name"]) != name {
				return userErr(400, "Location “%s” is linked to CMS property %s: its name comes from the CMS. Change the name in the CMS", str(before["name"]), cur)
			}
		}
		return nil
	}
	if !hasPID || pid == "" {
		delete(vals, "property_id")
		on, err := PropertiesFromCMS(ctx, tx)
		if err != nil {
			return err
		}
		if on {
			return userErr(400, "Properties come from the CMS: add a location in Masters › Locations by picking its CMS property")
		}
		return nil
	}
	p, err := CheckLinkable(ctx, tx, pid)
	if err != nil {
		return err
	}
	vals["property_id"] = pid
	vals["name"] = str(p["name"])
	return nil
}

// CMSProperties lists the local copy of the CMS's properties with the location each is linked to, for the
// Locations master (pick a property for a new location, or link an existing one).
func (s *Service) CMSProperties(ctx context.Context) (map[string]any, error) {
	on, err := PropertiesFromCMS(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	ps, err := s.st.DB.C(collProperty).Find(ctx, nil, options.Find().SetSort(bson.D{{Key: "name", Value: 1}, {Key: "property_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	locs, err := s.st.DB.C("location").Find(ctx, bson.M{"property_id": bson.M{"$type": "string"}}, options.Find().SetProjection(bson.M{"name": 1, "property_id": 1}))
	if err != nil {
		return nil, err
	}
	byPID := map[string]store.Row{}
	for _, l := range locs {
		byPID[str(l["property_id"])] = l
	}
	out := make([]map[string]any, 0, len(ps))
	for _, p := range ps {
		row := map[string]any{"propertyId": p["property_id"], "name": p["name"], "city": p["city"], "state": p["state"], "status": p["status"],
			"locationId": nil, "locationName": nil, "updatedAt": p["updated_at"]}
		if l, ok := byPID[str(p["property_id"])]; ok {
			row["locationId"], row["locationName"] = l["id"], l["name"]
		}
		out = append(out, row)
	}
	return map[string]any{"fromCms": on, "properties": out}, nil
}

// CheckReactivate holds the masters lock for reactivating a location (master_rules.go reactivateMaster calls it for
// t == "location"): a location whose CMS property is inactive is reactivated in the CMS, not in HRMS.
func CheckReactivate(ctx context.Context, q store.Q, loc store.Row) error {
	pid := str(loc["property_id"])
	if pid == "" {
		return nil
	}
	p, err := PropertyByID(ctx, q, pid)
	if err != nil || p == nil {
		return err
	}
	if str(p["status"]) != "Active" {
		return userErr(400, "Location “%s” is linked to CMS property %s, which is inactive in the CMS. Reactivate the property in the CMS", str(loc["name"]), pid)
	}
	return nil
}
