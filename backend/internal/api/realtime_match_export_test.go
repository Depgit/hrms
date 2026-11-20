package api

import (
	"context"

	"github.com/saltstayz/hrms/internal/realtime"
)

// SetRealtimeEmployees replaces the portal account and the download of its employee list (tests), returning the
// undo.
func SetRealtimeEmployees(cfg realtime.Config, ok bool, fetch func(context.Context, realtime.Config) ([]realtime.Employee, error)) (undo func()) {
	oldEnv, oldFetch := rtEnv, rtFetchEmployees
	rtEnv = func() (realtime.Config, bool) { return cfg, ok }
	if fetch != nil {
		rtFetchEmployees = fetch
	}
	return func() { rtEnv, rtFetchEmployees = oldEnv, oldFetch }
}
