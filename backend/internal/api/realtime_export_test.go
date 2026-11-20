package api

import (
	"context"
	"time"

	"github.com/saltstayz/hrms/internal/realtime"
	"github.com/saltstayz/hrms/internal/store"
)

// SetRealtime replaces the portal account and the download of a day's report (tests), returning the undo.
func SetRealtime(cfg realtime.Config, ok bool, fetch func(context.Context, realtime.Config, time.Time) ([]realtime.Row, error)) (undo func()) {
	oldEnv, oldFetch := rtEnv, rtFetch
	rtEnv = func() (realtime.Config, bool) { return cfg, ok }
	if fetch != nil {
		rtFetch = fetch
	}
	return func() { rtEnv, rtFetch = oldEnv, oldFetch }
}

// RealtimeRun runs a pull at once (tests) and returns the recorded result.
func (s *Server) RealtimeRun(ctx context.Context, from, to, by string) (map[string]any, error) {
	r, err := s.rtRun(ctx, from, to, by)
	return r.bson(), err
}

// RealtimeMatch matches a portal row against employees given as {id, attendance_code} (tests); byID is the
// "Match by Employee ID number" setting.
func RealtimeMatch(emps []store.Row, byID bool, empCode, cardNo string) string {
	return newRTMatcher(emps, byID).match(realtime.Row{EmpCode: empCode, CardNo: cardNo})
}
