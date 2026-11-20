package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/jobs"
	"github.com/saltstayz/hrms/internal/store"
)

// Pulling punches from the office server's API (spec 11.2, prototype "Punch machine setup"): every minute the job
// checks whether the configured interval has passed since the last pull, asks the server for punches since the
// last successful pull (with a 10-minute overlap, so a punch that reached the server late is not missed), maps
// the fields and stores them like pushed punches. The result is kept on the settings row for the setup page.

// pullDo sends one request for punches since `since` (India time, "2006-01-02 15:04:05") and returns the answer.
// The saved key goes only to the configured address; loopback and link-local addresses are refused (pullDialer).
func pullDo(ctx context.Context, p pullConfig, secret, since string, maxBytes int64) (int, string, []byte, error) {
	u, err := url.Parse(p.URL)
	if err != nil {
		return 0, "", nil, err
	}
	var body io.Reader
	if p.Method == "GET" {
		if p.Since != "" {
			q := u.Query()
			q.Set(p.Since, since)
			u.RawQuery = q.Encode()
		}
	} else {
		m := map[string]string{}
		if p.Since != "" {
			m[p.Since] = since
		}
		jb, _ := json.Marshal(m)
		body = bytes.NewReader(jb)
	}
	tctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(tctx, p.Method, u.String(), body)
	if err != nil {
		return 0, "", nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch p.Auth {
	case "API key":
		req.Header.Set("X-API-Key", secret)
	case "Bearer token":
		req.Header.Set("Authorization", "Bearer "+secret)
	case "Username and password":
		user, pass, _ := strings.Cut(secret, ":")
		req.SetBasicAuth(user, pass)
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: pullDialer.DialContext, TLSHandshakeTimeout: 5 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, maxBytes))
	return res.StatusCode, res.Header.Get("Content-Type"), data, nil
}

// PullArgs is the every-minute check for a due pull.
type PullArgs struct{}

func (PullArgs) Kind() string { return "punch_pull" }

func (PullArgs) InsertOpts() jobs.InsertOpts {
	return jobs.InsertOpts{UniqueOpts: jobs.UniqueOpts{ByPeriod: time.Minute}}
}

type pullWorker struct {
	jobs.WorkerDefaults[PullArgs]
	s *Server
}

func (w *pullWorker) Work(ctx context.Context, _ *jobs.Job[PullArgs]) error {
	if _, err := w.s.PullDue(ctx, false); err != nil {
		log.Printf("punch pull: %v", err) // recorded on the settings row; a failed pull is retried next interval
	}
	return nil
}

// PullDue pulls when an address is set and the interval has passed (or always when force). It returns how many
// punches were stored.
func (s *Server) PullDue(ctx context.Context, force bool) (int, error) {
	p, secret, _, err := loadPull(ctx, s.st.DB)
	if err != nil || p.URL == "" {
		return 0, err
	}
	var last, cursor *time.Time
	if row, _ := s.st.DB.C("attendance_pull_config").FindOne(ctx, bson.D{{Key: "_id", Value: 1}}); row != nil {
		if t, ok := row["last_pull_at"].(time.Time); ok {
			last = &t
		}
		if t, ok := row["pull_cursor"].(time.Time); ok {
			cursor = &t
		}
	}
	now := time.Now()
	if !force && last != nil && now.Before(last.Add(time.Duration(p.Every)*time.Minute)) {
		return 0, nil
	}
	from := now.Add(-24 * time.Hour)
	if cursor != nil {
		from = cursor.Add(-10 * time.Minute)
	}
	since := from.In(ist).Format("2006-01-02 15:04:05")
	record := func(ok bool, note string, n int, next *time.Time) error {
		set := bson.M{"last_pull_at": time.Now(), "last_pull_ok": ok, "last_pull_note": note, "last_pull_count": n}
		if next != nil { // a failed pull keeps the cursor
			set["pull_cursor"] = *next
		}
		_, err := s.st.DB.C("attendance_pull_config").Update(ctx, bson.D{{Key: "_id", Value: 1}}, bson.M{"$set": set})
		return err
	}
	status, _, data, err := pullDo(ctx, p, secret, since, 8<<20)
	if err != nil {
		return 0, record(false, pullErr(err), 0, nil)
	}
	if status < 200 || status >= 300 {
		return 0, record(false, fmt.Sprintf("The server answered %d %s", status, http.StatusText(status)), 0, nil)
	}
	recs, ok := pullRecords(data)
	if !ok {
		return 0, record(false, "The answer is not JSON", 0, nil)
	}
	punches := make([]punchIn, 0, len(recs))
	for _, x := range recs {
		if x[p.EmpF] == nil || x[p.TimeF] == nil {
			continue
		}
		dir := ""
		if p.DirF != "" && x[p.DirF] != nil {
			switch fmt.Sprint(x[p.DirF]) {
			case p.InV:
				dir = "IN"
			case p.OutV:
				dir = "OUT"
			}
		}
		punches = append(punches, punchIn{EmployeeID: fmt.Sprint(x[p.EmpF]), Time: fmt.Sprint(x[p.TimeF]), Direction: dir})
	}
	var stored int
	var rejected []punchRej
	if err := s.st.Tx(ctx, func(tx store.Q) error {
		var err error
		stored, rejected, err = ingestPunches(ctx, tx, punches, "Pull API")
		return err
	}); err != nil {
		return 0, record(false, "Punches could not be stored", 0, nil)
	}
	note := fmt.Sprintf("%d record%s, %d new punch%s", len(recs), map[bool]string{true: "", false: "s"}[len(recs) == 1], stored, map[bool]string{true: "", false: "es"}[stored == 1])
	if len(rejected) > 0 {
		note += fmt.Sprintf(", %d not stored (for example: %s)", len(rejected), rejected[0].Why)
	}
	return stored, record(true, note, stored, &now)
}

// pullNow runs a pull at once (the setup page's "Pull now") and returns the stored count with the updated status.
func (s *Server) pullNow(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 2); err != nil {
		return nil, err
	}
	ctx := r.Context()
	p, _, _, err := loadPull(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	if p.URL == "" {
		return nil, bad("Save the server API address first")
	}
	n, err := s.PullDue(ctx, true)
	if err != nil {
		return nil, err
	}
	_, _, meta, err := loadPull(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	_ = audit(ctx, s.st.DB, c, fmt.Sprintf("Punches pulled from the server API: %v", meta["last_pull_note"]), "attendance_pull_config", "1", nil, nil)
	return map[string]any{"stored": n, "last_pull_at": meta["last_pull_at"], "last_pull_ok": meta["last_pull_ok"], "last_pull_note": meta["last_pull_note"]}, nil
}
