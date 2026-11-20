package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/jobs"
	"github.com/saltstayz/hrms/internal/realtime"
	"github.com/saltstayz/hrms/internal/store"
)

// Realtime India attendance portal (owner, 16 Nov 2026). The office's punch machines report to Realtime's web
// portal, which has no API; internal/realtime signs in with the account in the environment (REALTIME_URL,
// REALTIME_USER, REALTIME_PASS), downloads the Daily report "All Raw Punch Report" of a day as Excel, and reads each
// employee's swipes. Here the swipes are matched to employees and stored like any other punch (ingestPunches,
// source "Realtime portal"); each day's in and out are then worked out as for punch machines.
//
// Matching: the employee's Attendance code (employee.attendance_code, set by Bulk update) against the portal's
// EmpCode or CardNo, leading zeros ignored. Only when the setting "Match by Employee ID number" is on (off by default:
// the HRMS's own IDs need not be the portal's codes, and the trial matched portal code 1008 to an unrelated
// EMP-1008): when no employee has that code, the employee whose Employee ID has that number (EMP-1050 ↔ 1050 or
// 00001050), when exactly one does and they have no Attendance code of their own. A portal row that matches nobody
// is listed on the setup page; it never stops the others.
//
// Settings and the last result are company_setting.realtime_pull. Off by default. When on, the job pulls today
// every N minutes (15 to 1,440, default 30) and, until 10:00, yesterday too (late punches). "Pull now" pulls a
// range of up to 31 days in the background.

const (
	rtSource      = "Realtime portal"
	rtDefEvery    = 30
	rtMinEvery    = 15
	rtMaxDays     = 31
	rtUnmatchedN  = 200
	rtStaleAfter  = 45 * time.Minute
	rtYesterdayTo = 10 // hour (IST) until which the automatic pull also reads yesterday
)

// rtEnv is the portal account from the environment (main loads backend/.env into it).
var rtEnv = func() (realtime.Config, bool) { return realtime.FromEnv(os.Getenv) }

// rtFetch downloads and reads one day's report. Tests replace it.
var rtFetch = func(ctx context.Context, cfg realtime.Config, day time.Time) ([]realtime.Row, error) {
	c, err := realtime.New(cfg)
	if err != nil {
		return nil, err
	}
	b, err := c.Report(ctx, day)
	if err != nil {
		return nil, err
	}
	return realtime.Parse(b)
}

// rtBusy lets one pull run at a time in this process.
var rtBusy = make(chan struct{}, 1)

func rtSettings(ctx context.Context, q store.Q) (bson.M, error) {
	r, err := q.C("company_setting").FindOne(ctx, bson.M{"_id": 1}, options.FindOne().SetProjection(bson.M{"realtime_pull": 1}))
	if err != nil {
		return nil, err
	}
	out := bson.M{}
	if r != nil {
		switch m := r["realtime_pull"].(type) {
		case map[string]any:
			for k, v := range m {
				out[k] = v
			}
		case bson.M:
			for k, v := range m {
				out[k] = v
			}
		}
	}
	if timectxInt(out["every"]) < rtMinEvery {
		out["every"] = rtDefEvery
	}
	if _, ok := out["on"].(bool); !ok {
		out["on"] = false
	}
	return out, nil
}

func rtSet(ctx context.Context, q store.Q, set bson.M) error {
	cur, err := rtSettings(ctx, q)
	if err != nil {
		return err
	}
	for k, v := range set {
		cur[k] = v
	}
	_, err = q.C("company_setting").Update(ctx, bson.M{"_id": 1}, bson.M{"$set": bson.M{"realtime_pull": map[string]any(cur)}})
	return err
}

// rtRunning is the pull in progress (nil when none, or when it started so long ago that its process must have stopped).
func rtRunning(st bson.M) any {
	m, _ := st["running"].(map[string]any)
	if m == nil {
		if b, ok := st["running"].(bson.M); ok {
			m = b
		}
	}
	if m == nil {
		return nil
	}
	if t, ok := m["since"].(time.Time); ok && time.Since(t) > rtStaleAfter {
		return nil
	}
	return m
}

// ---------- Matching ----------

// rtNorm is a code without spaces and leading zeros, upper case ("00001050" → "1050"; "0000" → "0").
func rtNorm(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	if t := strings.TrimLeft(s, "0"); t != "" {
		return t
	}
	return "0"
}

// reIDNum takes the number of an Employee ID (EMP-1050, EMP1050, SS/1050 → 1050).
var reIDNum = regexp.MustCompile(`^[A-Z]*[-_/ ]?0*(\d+)$`)

type rtMatcher struct {
	byCode map[string][]string // normalised attendance code → employees
	byNum  map[string][]string // number of the Employee ID → employees without an attendance code
}

func newRTMatcher(emps []store.Row, byID bool) *rtMatcher {
	m := &rtMatcher{byCode: map[string][]string{}, byNum: map[string][]string{}}
	for _, e := range emps {
		id := str(e["id"])
		if code := rtNorm(str(e["attendance_code"])); code != "" {
			m.byCode[code] = append(m.byCode[code], id)
			continue
		}
		if !byID {
			continue
		}
		if x := reIDNum.FindStringSubmatch(strings.ToUpper(id)); x != nil {
			m.byNum[x[1]] = append(m.byNum[x[1]], id)
		}
	}
	return m
}

// match finds the employee of a portal row: by attendance code first, then by the Employee ID's number.
func (m *rtMatcher) match(r realtime.Row) string {
	var codes []string
	for _, c := range []string{rtNorm(r.EmpCode), rtNorm(r.CardNo)} {
		if c != "" && (len(codes) == 0 || codes[0] != c) {
			codes = append(codes, c)
		}
	}
	for _, c := range codes {
		if ids := m.byCode[c]; len(ids) == 1 {
			return ids[0]
		}
	}
	for _, c := range codes {
		if len(m.byCode[c]) > 1 { // two employees share the code: neither is guessed
			return ""
		}
	}
	for _, c := range codes {
		if ids := m.byNum[c]; len(ids) == 1 {
			return ids[0]
		}
	}
	return ""
}

func rtLoadMatcher(ctx context.Context, q store.Q, byID bool) (*rtMatcher, error) {
	emps, err := q.C("employee").Find(ctx, bson.M{"status": bson.M{"$ne": "Pre-boarding"}}, options.Find().SetProjection(bson.M{"_id": 1, "attendance_code": 1}))
	if err != nil {
		return nil, err
	}
	return newRTMatcher(emps, byID), nil
}

// ---------- A run ----------

type rtUnmatched struct {
	Code   string `json:"code"`
	Card   string `json:"card"`
	Name   string `json:"name"`
	Dept   string `json:"dept"`
	Swipes int    `json:"swipes"`
}

type rtResult struct {
	At        time.Time     `json:"at"`
	By        string        `json:"by"`
	From      string        `json:"from"`
	To        string        `json:"to"`
	OK        bool          `json:"ok"`
	Days      int           `json:"days"`
	Rows      int           `json:"rows"`
	WithSwipe int           `json:"withSwipes"`
	Swipes    int           `json:"swipes"`
	Added     int           `json:"added"`
	Matched   int           `json:"matched"`
	Unmatched int           `json:"unmatchedCount"`
	List      []rtUnmatched `json:"unmatched"`
	Errors    []string      `json:"errors"`
	Note      string        `json:"note"`
}

func (r rtResult) bson() bson.M {
	l := make([]bson.M, 0, len(r.List))
	for _, u := range r.List {
		l = append(l, bson.M{"code": u.Code, "card": u.Card, "name": u.Name, "dept": u.Dept, "swipes": u.Swipes})
	}
	return bson.M{"at": r.At, "by": r.By, "from": r.From, "to": r.To, "ok": r.OK, "days": r.Days, "rows": r.Rows, "withSwipes": r.WithSwipe,
		"swipes": r.Swipes, "added": r.Added, "matched": r.Matched, "unmatchedCount": r.Unmatched, "unmatched": l, "errors": r.Errors, "note": r.Note}
}

func rtDays(from, to string) ([]time.Time, error) {
	f, err1 := time.ParseInLocation("2006-01-02", from, ist)
	t, err2 := time.ParseInLocation("2006-01-02", to, ist)
	if err1 != nil || err2 != nil {
		return nil, bad("Give the dates as yyyy-mm-dd")
	}
	if t.Before(f) {
		return nil, bad("The end date is before the start date")
	}
	if t.After(time.Now().In(ist)) {
		return nil, bad("The end date is after today")
	}
	var out []time.Time
	for d := f; !d.After(t); d = d.AddDate(0, 0, 1) {
		out = append(out, d)
	}
	if len(out) > rtMaxDays {
		return nil, bad("Pull at most %d days at a time", rtMaxDays)
	}
	return out, nil
}

// rtRun pulls the days from..to, stores the punches and records the result. It waits for a pull already running.
func (s *Server) rtRun(ctx context.Context, from, to, by string) (rtResult, error) {
	select {
	case rtBusy <- struct{}{}:
	case <-ctx.Done():
		return rtResult{}, ctx.Err()
	}
	defer func() { <-rtBusy }()
	res := rtResult{By: by, From: from, To: to, Errors: []string{}, List: []rtUnmatched{}}
	finish := func() (rtResult, error) {
		res.At = time.Now()
		res.OK = len(res.Errors) == 0
		res.Note = rtNote(res)
		err := rtSet(context.WithoutCancel(ctx), s.st.DB, bson.M{"last": res.bson(), "running": nil})
		return res, err
	}
	days, err := rtDays(from, to)
	if err != nil {
		res.Errors = append(res.Errors, err.Error())
		return finish()
	}
	cfg, ok := rtEnv()
	if !ok {
		res.Errors = append(res.Errors, "The portal account is not set: add REALTIME_URL, REALTIME_USER and REALTIME_PASS to the server's environment (backend/.env) and restart")
		return finish()
	}
	_ = rtSet(ctx, s.st.DB, bson.M{"running": bson.M{"since": time.Now(), "by": by, "from": from, "to": to}})
	stg, err := rtSettings(ctx, s.st.DB)
	if err != nil {
		return res, err
	}
	m, err := rtLoadMatcher(ctx, s.st.DB, stg["by_id"] == true)
	if err != nil {
		return res, err
	}
	matched := map[string]bool{}
	unmatched := map[string]*rtUnmatched{}
	for _, day := range days {
		rows, err := rtFetch(ctx, cfg, day)
		if err != nil {
			res.Errors = append(res.Errors, day.Format("2 Jan")+": "+err.Error())
			if ctx.Err() != nil {
				break
			}
			continue
		}
		res.Days++
		var punches []punchIn
		for _, r := range rows {
			res.Rows++
			sw := r.Swipes(ist)
			if len(sw) == 0 {
				continue
			}
			res.WithSwipe++
			res.Swipes += len(sw)
			id := m.match(r)
			if id == "" {
				k := or(rtNorm(r.EmpCode), rtNorm(r.CardNo))
				if u := unmatched[k]; u != nil {
					u.Swipes += len(sw)
				} else {
					unmatched[k] = &rtUnmatched{Code: r.EmpCode, Card: r.CardNo, Name: r.Name, Dept: r.Department, Swipes: len(sw)}
				}
				continue
			}
			matched[id] = true
			for _, t := range sw {
				punches = append(punches, punchIn{EmployeeID: id, Time: t.In(ist).Format("2006-01-02 15:04")})
			}
		}
		if len(punches) == 0 {
			continue
		}
		var n int
		if err := s.st.Tx(ctx, func(tx store.Q) error {
			var err error
			n, _, err = ingestPunches(ctx, tx, punches, rtSource) // a punch already stored counts once; a future one is skipped
			return err
		}); err != nil {
			res.Errors = append(res.Errors, day.Format("2 Jan")+": the punches could not be stored")
			continue
		}
		res.Added += n
	}
	res.Matched = len(matched)
	res.Unmatched = len(unmatched)
	for _, u := range unmatched {
		res.List = append(res.List, *u)
	}
	sort.Slice(res.List, func(i, j int) bool {
		if res.List[i].Swipes != res.List[j].Swipes {
			return res.List[i].Swipes > res.List[j].Swipes
		}
		return res.List[i].Code < res.List[j].Code
	})
	if len(res.List) > rtUnmatchedN {
		res.List = res.List[:rtUnmatchedN]
	}
	return finish()
}

func rtNote(r rtResult) string {
	if r.Days == 0 && len(r.Errors) > 0 {
		return r.Errors[0]
	}
	n := fmt.Sprintf("%d employee%s with swipes, %d swipe%s, %d new punch%s", r.WithSwipe, rtPl(r.WithSwipe, "s"), r.Swipes, rtPl(r.Swipes, "s"), r.Added, rtPl(r.Added, "es"))
	if r.Unmatched > 0 {
		n += fmt.Sprintf("; %d portal code%s match no employee", r.Unmatched, rtPl(r.Unmatched, "s"))
	}
	if len(r.Errors) > 0 {
		n += "; " + r.Errors[0]
	}
	return n
}

func rtPl(n int, s string) string {
	if n == 1 {
		return ""
	}
	return s
}

// RealtimeDue is the automatic pull: when on and the interval has passed, today (and yesterday until 10:00).
func (s *Server) RealtimeDue(ctx context.Context) error {
	st, err := rtSettings(ctx, s.st.DB)
	if err != nil || st["on"] != true {
		return err
	}
	if _, ok := rtEnv(); !ok {
		return nil // the setup page says so
	}
	now := time.Now()
	if t, ok := st["last_auto_at"].(time.Time); ok && now.Before(t.Add(time.Duration(timectxInt(st["every"]))*time.Minute)) {
		return nil
	}
	if len(rtBusy) > 0 {
		return nil // a pull is running; the next tick looks again
	}
	if err := rtSet(ctx, s.st.DB, bson.M{"last_auto_at": now}); err != nil {
		return err
	}
	day := now.In(ist)
	from := day.Format("2006-01-02")
	if day.Hour() < rtYesterdayTo {
		from = day.AddDate(0, 0, -1).Format("2006-01-02")
	}
	_, err = s.rtRun(ctx, from, day.Format("2006-01-02"), "Automatic")
	return err
}

// ---------- Jobs ----------

// RealtimeTickArgs is the every-minute check for a due automatic pull.
type RealtimeTickArgs struct{}

func (RealtimeTickArgs) Kind() string { return "realtime_tick" }

func (RealtimeTickArgs) InsertOpts() jobs.InsertOpts {
	return jobs.InsertOpts{UniqueOpts: jobs.UniqueOpts{ByPeriod: time.Minute}}
}

type realtimeTickWorker struct {
	jobs.WorkerDefaults[RealtimeTickArgs]
	s *Server
}

func (w *realtimeTickWorker) Timeout(*jobs.Job[RealtimeTickArgs]) time.Duration {
	return 30 * time.Minute
}

func (w *realtimeTickWorker) Work(ctx context.Context, _ *jobs.Job[RealtimeTickArgs]) error {
	if err := w.s.RealtimeDue(ctx); err != nil {
		log.Printf("realtime portal pull: %v", err) // the result is on the setup page; the next interval tries again
	}
	return nil
}

// RealtimeRunArgs is a "Pull now" of a date range.
type RealtimeRunArgs struct {
	From string `json:"from"`
	To   string `json:"to"`
	By   string `json:"by"`
}

func (RealtimeRunArgs) Kind() string { return "realtime_run" }

func (RealtimeRunArgs) InsertOpts() jobs.InsertOpts { return jobs.InsertOpts{MaxAttempts: 1} }

type realtimeRunWorker struct {
	jobs.WorkerDefaults[RealtimeRunArgs]
	s *Server
}

func (w *realtimeRunWorker) Timeout(*jobs.Job[RealtimeRunArgs]) time.Duration { return 2 * time.Hour }

func (w *realtimeRunWorker) Work(ctx context.Context, j *jobs.Job[RealtimeRunArgs]) error {
	if _, err := w.s.rtRun(ctx, j.Args.From, j.Args.To, j.Args.By); err != nil {
		log.Printf("realtime portal pull %s–%s: %v", j.Args.From, j.Args.To, err)
	}
	return nil
}

// ---------- Routes ----------

func (s *Server) realtimeGet(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 1); err != nil {
		return nil, err
	}
	st, err := rtSettings(r.Context(), s.st.DB)
	if err != nil {
		return nil, err
	}
	cfg, ok := rtEnv()
	running := rtRunning(st)
	return map[string]any{"on": st["on"], "every": st["every"], "byId": st["by_id"] == true, "minEvery": rtMinEvery, "maxDays": rtMaxDays, "envSet": ok,
		"host": map[bool]string{true: cfg.Host(), false: ""}[ok], "last": st["last"], "lastAutoAt": st["last_auto_at"], "running": running}, nil
}

func (s *Server) realtimePut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 2); err != nil {
		return nil, err
	}
	var b struct {
		On    bool  `json:"on"`
		Every int   `json:"every"`
		ByID  *bool `json:"byId"` // match by Employee ID number; left as it is when not sent
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Every < rtMinEvery || b.Every > 1440 {
		return nil, bad("Pull every %d to 1,440 minutes", rtMinEvery)
	}
	if _, ok := rtEnv(); b.On && !ok {
		return nil, bad("Add REALTIME_URL, REALTIME_USER and REALTIME_PASS to the server's environment first")
	}
	ctx := r.Context()
	before, err := rtSettings(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	byID := before["by_id"] == true
	if b.ByID != nil {
		byID = *b.ByID
	}
	if err := s.st.Tx(ctx, func(tx store.Q) error {
		if err := rtSet(ctx, tx, bson.M{"on": b.On, "every": b.Every, "by_id": byID}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Realtime portal pull settings saved", "company_setting", "1",
			map[string]any{"on": before["on"], "every": before["every"], "byId": before["by_id"] == true}, map[string]any{"on": b.On, "every": b.Every, "byId": byID})
	}); err != nil {
		return nil, err
	}
	return s.realtimeGet(r, c)
}

// realtimePull starts a pull of a date range (default today) in the background.
func (s *Server) realtimePull(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 2); err != nil {
		return nil, err
	}
	var b struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := decodeLoose(r, &b); err != nil {
		return nil, err
	}
	t := time.Now().In(ist).Format("2006-01-02")
	b.From, b.To = or(strings.TrimSpace(b.From), t), or(strings.TrimSpace(b.To), or(strings.TrimSpace(b.From), t))
	if _, err := rtDays(b.From, b.To); err != nil {
		return nil, err
	}
	if _, ok := rtEnv(); !ok {
		return nil, bad("Add REALTIME_URL, REALTIME_USER and REALTIME_PASS to the server's environment first")
	}
	ctx := r.Context()
	st, err := rtSettings(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	if rtRunning(st) != nil {
		return nil, bad("A pull is already running. Wait for it to finish")
	}
	by := c.Name
	if err := rtSet(ctx, s.st.DB, bson.M{"running": bson.M{"since": time.Now(), "by": by, "from": b.From, "to": b.To}}); err != nil {
		return nil, err
	}
	_ = audit(ctx, s.st.DB, c, fmt.Sprintf("Punches pulled from the Realtime portal for %s to %s", b.From, b.To), "company_setting", "1", nil, nil)
	if s.jobs != nil {
		if _, err := s.jobs.Insert(context.WithoutCancel(ctx), RealtimeRunArgs{From: b.From, To: b.To, By: by}, nil); err != nil {
			return nil, err
		}
	} else {
		go func() { _, _ = s.rtRun(context.WithoutCancel(ctx), b.From, b.To, by) }()
	}
	return map[string]any{"started": true, "from": b.From, "to": b.To}, nil
}

// PullRealtime pulls the days from..to at once (the "hrms realtime-pull" command) and returns the recorded note and
// whether every day was read.
func (s *Server) PullRealtime(ctx context.Context, from, to string) (string, bool, error) {
	if _, err := rtDays(from, to); err != nil {
		return "", false, err
	}
	r, err := s.rtRun(ctx, from, to, "Command line")
	return r.Note, r.OK, err
}
