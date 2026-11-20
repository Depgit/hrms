package mdm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// The pull side for consumers (PMS, Purchase Desk, Moneyball), authenticated by a per-consumer API key sent as
// "Authorization: Bearer <key>" (HRMS keeps only the key's SHA-256):
//
//	GET /api/mdm/events?after=<pos>&limit=<n>        the outbox from a position, in pos order
//	GET /api/mdm/employees?after=<code>&limit=<n>    the current published snapshots, by employee_code
//	GET /api/mdm/employees/summary                   count + checksum, for the nightly reconciliation

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(Canonical(v), '\n'))
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// consumer authenticates a pull request; it writes the refusal and returns nil when the key is not good.
func (s *Service) consumer(w http.ResponseWriter, r *http.Request) store.Row {
	h := r.Header.Get("Authorization")
	tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	if tok == "" || !strings.HasPrefix(h, "Bearer ") {
		w.Header().Set("WWW-Authenticate", `Bearer realm="hrms-mdm"`)
		writeError(w, http.StatusUnauthorized, "Send the API key as Authorization: Bearer <key>")
		return nil
	}
	k, err := s.st.DB.C(collKey).FindOne(r.Context(), bson.M{"key_hash": keyHash(tok)})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Something went wrong")
		return nil
	}
	if k == nil || k["active"] != true {
		w.Header().Set("WWW-Authenticate", `Bearer realm="hrms-mdm", error="invalid_token"`)
		writeError(w, http.StatusUnauthorized, "The API key is not valid or is paused")
		return nil
	}
	now := s.now()
	if t, ok := k["last_used_at"].(time.Time); !ok || now.Sub(t) > time.Minute {
		_, _ = s.st.DB.C(collKey).Update(r.Context(), bson.M{"_id": k["id"]}, bson.M{"$set": bson.M{"last_used_at": now}})
	}
	return k
}

func intParam(r *http.Request, name string, def, lo, hi int64) (int64, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < lo || n > hi {
		return 0, false
	}
	return n, true
}

// EventsPage is a page of the pull feed.
type EventsPage struct {
	Events    []Event `json:"events"`
	NextAfter int64   `json:"next_after"` // pass as after= for the next page
	HasMore   bool    `json:"has_more"`
	Head      int64   `json:"head"` // the last pos written when the page was read
}

// Events reads the outbox after a position, in pos order.
func (s *Service) Events(ctx context.Context, after, limit int64) (EventsPage, error) {
	head, err := s.Head(ctx, s.st.DB)
	if err != nil {
		return EventsPage{}, err
	}
	rows, err := s.st.DB.C(collOutbox).Find(ctx, bson.M{"pos": bson.M{"$gt": after, "$lte": head}}, findSortLimit(bson.D{{Key: "pos", Value: 1}}, limit+1))
	if err != nil {
		return EventsPage{}, err
	}
	p := EventsPage{Events: []Event{}, NextAfter: after, Head: head}
	for i, r := range rows {
		if int64(i) == limit {
			p.HasMore = true
			break
		}
		e := eventOf(r)
		p.Events = append(p.Events, e)
		p.NextAfter = e.Pos
	}
	return p, nil
}

// EventsHandler serves GET /api/mdm/events?after=<pos>&limit=<n> (limit 1–1000, default 100).
func (s *Service) EventsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.consumer(w, r) == nil {
			return
		}
		after, ok1 := intParam(r, "after", 0, 0, 1<<62)
		limit, ok2 := intParam(r, "limit", 100, 1, 1000)
		if !ok1 || !ok2 {
			writeError(w, http.StatusBadRequest, "after must be a position (0 or more) and limit a number from 1 to 1000")
			return
		}
		p, err := s.Events(r.Context(), after, limit)
		if err != nil {
			s.logf("events feed: %v", err)
			writeError(w, http.StatusInternalServerError, "Something went wrong")
			return
		}
		writeJSON(w, http.StatusOK, p)
	})
}

// PublishedEmployee is one current published snapshot.
type PublishedEmployee struct {
	EmployeeCode string         `json:"employee_code"`
	Sequence     int64          `json:"sequence"`
	Hash         string         `json:"hash"`
	Pos          int64          `json:"pos"`
	Data         map[string]any `json:"data"`
}

// EmployeesPage is a page of the published snapshots.
type EmployeesPage struct {
	SchemaVersion string              `json:"schema_version"`
	Employees     []PublishedEmployee `json:"employees"`
	NextAfter     *string             `json:"next_after"` // pass as after= for the next page; null at the end
	HasMore       bool                `json:"has_more"`
}

// Employees reads the published snapshots after a code, in employee_code order (byte order).
func (s *Service) Employees(ctx context.Context, after string, limit int64) (EmployeesPage, error) {
	f := bson.M{}
	if after != "" {
		f["employee_code"] = bson.M{"$gt": after}
	}
	rows, err := s.st.DB.C(collPublished).Find(ctx, f, findSortLimit(bson.D{{Key: "employee_code", Value: 1}}, limit+1))
	if err != nil {
		return EmployeesPage{}, err
	}
	p := EmployeesPage{SchemaVersion: SchemaVersion, Employees: []PublishedEmployee{}}
	for i, r := range rows {
		if int64(i) == limit {
			p.HasMore = true
			break
		}
		d, _ := r["data"].(map[string]any)
		p.Employees = append(p.Employees, PublishedEmployee{EmployeeCode: str(r["employee_code"]), Sequence: i64(r["sequence"]),
			Hash: str(r["hash"]), Pos: i64(r["pos"]), Data: d})
	}
	if p.HasMore {
		last := p.Employees[len(p.Employees)-1].EmployeeCode
		p.NextAfter = &last
	}
	return p, nil
}

// EmployeesHandler serves GET /api/mdm/employees?after=<code>&limit=<n> (limit 1–1000, default 200).
func (s *Service) EmployeesHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.consumer(w, r) == nil {
			return
		}
		limit, ok := intParam(r, "limit", 200, 1, 1000)
		if !ok {
			writeError(w, http.StatusBadRequest, "limit must be a number from 1 to 1000")
			return
		}
		p, err := s.Employees(r.Context(), r.URL.Query().Get("after"), limit)
		if err != nil {
			s.logf("employees snapshot: %v", err)
			writeError(w, http.StatusInternalServerError, "Something went wrong")
			return
		}
		writeJSON(w, http.StatusOK, p)
	})
}

// Summary is the published snapshots in one line, for a consumer's nightly check.
type Summary struct {
	SchemaVersion string `json:"schema_version"`
	Count         int64  `json:"count"`
	// Checksum is the hex SHA-256 of one line "employee_code:sequence\n" per published employee, in employee_code
	// byte order. A consumer computes the same over the employees it holds; equal checksums mean nothing is missing.
	Checksum    string `json:"checksum"`
	Algorithm   string `json:"algorithm"`
	Head        int64  `json:"head"` // the last pos the snapshots include
	GeneratedAt string `json:"generated_at"`
}

// ChecksumAlgorithm describes Summary.Checksum.
const ChecksumAlgorithm = `sha256 of "employee_code:sequence\n" per employee, in employee_code byte order`

// Checksum computes Summary.Checksum from (code, sequence) pairs already in employee_code byte order.
func Checksum(pairs [][2]string) string {
	h := sha256.New()
	for _, p := range pairs {
		h.Write([]byte(p[0] + ":" + p[1] + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// EmployeesSummary reads count, checksum and head from one snapshot of the data.
func (s *Service) EmployeesSummary(ctx context.Context) (Summary, error) {
	var out Summary
	err := s.st.Tx(ctx, func(tx store.Q) error {
		head, err := s.Head(ctx, tx)
		if err != nil {
			return err
		}
		var rows []struct {
			Code string `bson:"employee_code"`
			Seq  int64  `bson:"sequence"`
		}
		if err := tx.C(collPublished).FindInto(ctx, bson.M{}, &rows, findSortLimit(bson.D{{Key: "employee_code", Value: 1}}, 0)); err != nil {
			return err
		}
		pairs := make([][2]string, len(rows))
		for i, r := range rows {
			pairs[i] = [2]string{r.Code, strconv.FormatInt(r.Seq, 10)}
		}
		out = Summary{SchemaVersion: SchemaVersion, Count: int64(len(rows)), Checksum: Checksum(pairs), Algorithm: ChecksumAlgorithm,
			Head: head, GeneratedAt: utc(s.now())}
		return nil
	})
	return out, err
}

// SummaryHandler serves GET /api/mdm/employees/summary.
func (s *Service) SummaryHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.consumer(w, r) == nil {
			return
		}
		sum, err := s.EmployeesSummary(r.Context())
		if err != nil {
			s.logf("employees summary: %v", err)
			writeError(w, http.StatusInternalServerError, "Something went wrong")
			return
		}
		writeJSON(w, http.StatusOK, sum)
	})
}
