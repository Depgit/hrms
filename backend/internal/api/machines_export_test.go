package api

import (
	"context"

	"github.com/saltstayz/hrms/internal/realtime"
)

// SetMachinePortal replaces the portal account and the download of its machine list (tests), returning the undo.
func SetMachinePortal(cfg realtime.Config, ok bool, fetch func(context.Context, realtime.Config) ([]realtime.Machine, error)) (undo func()) {
	oldEnv, oldFetch := rtEnv, mpFetchMachines
	rtEnv = func() (realtime.Config, bool) { return cfg, ok }
	if fetch != nil {
		mpFetchMachines = fetch
	}
	return func() { rtEnv, mpFetchMachines = oldEnv, oldFetch }
}

// MachinePlan exposes the record placement (tests): machines as {id, location, capacity}, people as {id, location,
// doj}; it returns employee → machine id and who did not fit.
func MachinePlan(machines [][3]any, people [][3]string) (map[string]int, []string) {
	ms := []*mpMachine{}
	for _, m := range machines {
		ms = append(ms, &mpMachine{ID: m[0].(int), LocID: m[1].(string), LocName: m[1].(string), Name: "M", Cap: m[2].(int)})
	}
	ps := []mpPerson{}
	for _, p := range people {
		ps = append(ps, mpPerson{ID: p[0], LocID: p[1], DOJ: p[2]})
	}
	a, _, left := mpPlan(ms, ps)
	out := map[string]int{}
	for k, v := range a {
		out[k] = v.ID
	}
	return out, left
}
