package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// Punch-machine connector (spec 11.2, built last; vendor details still to come). Each device
// gets a key shown once; it, or a vendor bridge, pushes punches to POST /api/punches with the key
// in X-Device-Key. Punches are kept raw and each day's in and out are worked out from them. A
// regularized day is never overwritten. The MIS upload stays as the fallback.

var ist = time.FixedZone("IST", 5*3600+1800)

func hashKey(k string) string { s := sha256.Sum256([]byte(k)); return hex.EncodeToString(s[:]) }

func (s *Server) deviceList(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	devs, err := s.st.DB.C("attendance_device").Find(ctx, nil, options.Find().SetSort(bson.D{{Key: "status", Value: 1}, {Key: "name", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	locIDs := []string{}
	for _, d := range devs {
		locIDs = append(locIDs, str(d["location_id"]))
	}
	locs, err := timectxByID(ctx, s.st.DB, "location", locIDs, "name")
	if err != nil {
		return nil, err
	}
	out := []store.Row{}
	for _, d := range devs {
		var lastSeen, location any
		if t := timectxStamp(d["last_seen_at"], "02 Jan 2006 15:04"); t != "" {
			lastSeen = t
		}
		if l := locs[str(d["location_id"])]; l != nil { // a device may have no location
			location = l["name"]
		}
		out = append(out, store.Row{"id": d["id"], "name": d["name"], "vendor": d["vendor"], "serial": d["serial"], "key_hint": d["key_hint"], "status": d["status"],
			"punches": d["punches"], "last_seen": lastSeen, "location": location, "created_by": d["created_by"], "created": timectxStamp(d["created_at"], "02 Jan 2006")})
	}
	return out, nil
}

func (s *Server) deviceAdd(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 2); err != nil {
		return nil, err
	}
	var b struct {
		Name       string `json:"name"`
		LocationID string `json:"locationId"`
		Vendor     string `json:"vendor"`
		Serial     string `json:"serial"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Name = strings.TrimSpace(b.Name); b.Name == "" {
		return nil, bad("Name the device, for example Sector 29 front desk")
	}
	key := randomHex(24)
	ctx := r.Context()
	var id int
	err := s.st.Tx(ctx, func(tx store.Q) error {
		v, err := tx.C("attendance_device").Insert(ctx, bson.M{"name": b.Name, "location_id": timectxNullIf(b.LocationID), "vendor": strings.TrimSpace(b.Vendor),
			"serial": strings.TrimSpace(b.Serial), "key_hash": hashKey(key), "key_hint": key[len(key)-4:], "created_by": c.Name})
		if err != nil {
			return err
		}
		id = timectxInt(v)
		return audit(ctx, tx, c, "Punch machine added", "attendance_device", fmt.Sprint(id), nil, b)
	})
	return map[string]any{"id": id, "key": key, "note": "Copy the key now; it is not shown again."}, err
}

// devicesID reads a device id from the path: only the number's own spelling matches, so "007" or "x" finds no
// device (-1).
func devicesID(v string) int64 {
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || strconv.FormatInt(n, 10) != v {
		return -1
	}
	return n
}

func (s *Server) deviceAction(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 2); err != nil {
		return nil, err
	}
	ctx, id, act := r.Context(), r.PathValue("id"), r.PathValue("action")
	switch act {
	case "key":
		key := randomHex(24)
		n, err := s.st.DB.C("attendance_device").Update(ctx, bson.D{{Key: "_id", Value: devicesID(id)}, {Key: "status", Value: "Active"}},
			bson.M{"$set": bson.M{"key_hash": hashKey(key), "key_hint": key[len(key)-4:]}})
		if err != nil || n == 0 {
			return nil, orNotFound(err)
		}
		_ = audit(ctx, s.st.DB, c, "Punch machine key replaced", "attendance_device", id, nil, nil)
		return map[string]string{"key": key, "note": "The old key stops working now. Copy the new key; it is not shown again."}, nil
	case "inactive":
		var b struct {
			Reason string `json:"reason"`
		}
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		if strings.TrimSpace(b.Reason) == "" {
			return nil, bad("Give the reason")
		}
		n, err := s.st.DB.C("attendance_device").Update(ctx, bson.D{{Key: "_id", Value: devicesID(id)}},
			bson.M{"$set": bson.M{"status": "Inactive", "inactive_since": store.Today(), "inactive_reason": b.Reason}})
		if err != nil || n == 0 { // an unknown machine (TIME-240): nothing changed, nothing logged
			return nil, orNotFound(err)
		}
		_ = audit(ctx, s.st.DB, c, "Punch machine made inactive", "attendance_device", id, nil, b)
		return nil, nil
	}
	return nil, notFound("")
}

type punchIn struct {
	EmployeeID string `json:"employeeId"`
	Time       string `json:"time"`
	Direction  string `json:"direction"`
}

func parsePunchTime(v string) (time.Time, bool) {
	for _, f := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if f == time.RFC3339 {
			if t, err := time.Parse(f, v); err == nil {
				return t, true
			}
			continue
		}
		if t, err := time.ParseInLocation(f, v, ist); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// punchPush receives punches from a device (no user session; the device key authenticates).
func (s *Server) punchPush(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	key := r.Header.Get("X-Device-Key")
	var d store.Row
	var err error
	if key != "" {
		d, err = s.st.DB.C("attendance_device").FindOne(ctx, bson.D{{Key: "key_hash", Value: hashKey(key)}, {Key: "status", Value: "Active"}},
			options.FindOne().SetProjection(bson.D{{Key: "name", Value: 1}}))
	}
	if key == "" || err != nil || d == nil {
		writeErr(w, unauthorized("Unknown or inactive device key"))
		return
	}
	dev, devName := timectxInt(d["id"]), str(d["name"])
	var b struct {
		Punches []punchIn `json:"punches"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 5<<20))
	if err := dec.Decode(&b); err != nil || len(b.Punches) == 0 {
		writeErr(w, bad("Send {\"punches\": [{\"employeeId\", \"time\", \"direction\"}]}"))
		return
	}
	if len(b.Punches) > 5000 {
		writeErr(w, bad("Send at most 5,000 punches at a time"))
		return
	}
	var accepted int
	var rejected []punchRej
	err = s.st.Tx(ctx, func(tx store.Q) error {
		var err error
		if accepted, rejected, err = ingestPunches(ctx, tx, b.Punches, devName); err != nil {
			return err
		}
		_, err = tx.C("attendance_device").Update(ctx, bson.D{{Key: "_id", Value: dev}},
			bson.M{"$set": bson.M{"last_seen_at": tx.Now()}, "$inc": bson.M{"punches": int64(accepted)}})
		return err
	})
	if err != nil {
		log.Printf("punch push from device %d: %v", dev, err)
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"accepted": accepted, "rejected": rejected})
}

type punchRej struct {
	Row int    `json:"row"`
	Why string `json:"why"`
}

// ingestPunches stores punches from a machine (pushed by a device, or pulled from the office server's API) and
// rebuilds each day they touch (see punchdays.go: calendar days, or the shift's window for shifts that need one).
// A punch already stored (same employee and time) counts once, also when it comes twice in one batch. A
// regularized day is never overwritten. The employees and the punches already stored are looked up once for the
// whole batch, and the new punches go in with one insert.
func ingestPunches(ctx context.Context, tx store.Q, punches []punchIn, source string) (int, []punchRej, error) {
	rejected := []punchRej{}
	type parsed struct {
		i       int
		id, dir string
		t       time.Time
	}
	var ok []parsed
	ids, times := []string{}, []time.Time{}
	for i, p := range punches {
		id := strings.ToUpper(strings.TrimSpace(p.EmployeeID))
		t, good := parsePunchTime(strings.TrimSpace(p.Time))
		dir := strings.ToUpper(strings.TrimSpace(p.Direction))
		switch {
		case !good:
			rejected = append(rejected, punchRej{i, "time must look like 2026-11-01 09:02 (IST) or RFC 3339"})
			continue
		case t.After(time.Now().Add(10 * time.Minute)):
			rejected = append(rejected, punchRej{i, "time is in the future"})
			continue
		case dir != "" && dir != "IN" && dir != "OUT":
			rejected = append(rejected, punchRej{i, "direction is IN, OUT or empty"})
			continue
		}
		ok = append(ok, parsed{i, id, dir, t})
		ids, times = append(ids, id), append(times, t)
	}
	// Employees who can punch: anyone but Pre-boarding.
	known := map[string]bool{}
	if len(ids) > 0 {
		emps, err := tx.C("employee").Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}, {Key: "status", Value: bson.D{{Key: "$ne", Value: "Pre-boarding"}}}},
			options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}}))
		if err != nil {
			return 0, nil, err
		}
		for _, e := range emps {
			known[str(e["id"])] = true
		}
	}
	// Punches already stored at these instants (times are kept to the millisecond).
	key := func(id string, t time.Time) string { return id + "|" + strconv.FormatInt(t.UnixMilli(), 10) }
	have := map[string]bool{}
	if len(ids) > 0 {
		prior, err := tx.C("punch").Find(ctx, bson.D{{Key: "employee_id", Value: bson.D{{Key: "$in", Value: ids}}}, {Key: "ts", Value: bson.D{{Key: "$in", Value: times}}}},
			options.Find().SetProjection(bson.D{{Key: "_id", Value: 0}, {Key: "employee_id", Value: 1}, {Key: "ts", Value: 1}}))
		if err != nil {
			return 0, nil, err
		}
		for _, p := range prior {
			if t, isTime := p["ts"].(time.Time); isTime {
				have[key(str(p["employee_id"]), t)] = true
			}
		}
	}
	accepted := 0
	days := map[string]map[string]bool{}
	var add []any
	for _, p := range ok {
		if !known[p.id] {
			rejected = append(rejected, punchRej{p.i, "unknown employee " + p.id})
			continue
		}
		if have[key(p.id, p.t)] {
			continue // a device resends: the same punch counts once
		}
		have[key(p.id, p.t)] = true
		add = append(add, bson.M{"employee_id": p.id, "ts": p.t, "direction": timectxNullIf(p.dir), "source": source})
		accepted++
		if days[p.id] == nil {
			days[p.id] = map[string]bool{}
		}
		days[p.id][p.t.In(ist).Format("2006-01-02")] = true
	}
	devicesSortRejected(rejected)
	if err := tx.C("punch").InsertMany(ctx, add); err != nil {
		return 0, nil, err
	}
	if err := rebuildPunchDays(ctx, tx, nil, days, false); err != nil {
		return 0, nil, err
	}
	return accepted, rejected, nil
}

// devicesSortRejected keeps the rejections in the order of the punches sent (format problems and unknown employees
// are found in two passes).
func devicesSortRejected(l []punchRej) {
	sort.SliceStable(l, func(i, j int) bool { return l[i].Row < l[j].Row })
}
