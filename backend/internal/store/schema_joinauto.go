package store

// Registry additions for what happens by itself at joining (user, 19 Nov 2026):
//
//   - offer.data.accom ("Yes"/"No") and offer.data.accomCluster: the offer's "Company accommodation" tag (inside the
//     offer's data, no new column). At Mark joined the HRMS books a bed in that cluster (api/hst_auto.go).
//   - employee.hst_wait (jsonb): on the "Waiting for a bed" list: cluster_id, since (the joining date), offer_id.
//     Null: not waiting. Cleared when a bed is booked (by the sweep or by hand), when they leave, or by HR.
//   - employee.portal_sync gains want ("create" from Mark joined), phase (waiting, sending, failed, blocked),
//     phase_action, phase_msg, phase_at and not_same: the automatic writes to the Realtime portal
//     (api/portal_auto.go). Inside the existing jsonb; no change to its column.
//   - company_setting.portal_write.auto_since: the day automatic sending began (exits before it of people the HRMS
//     never sent are not marked left; joiners from it are created).
//
// Migration 022 records auto_since (once). Nothing else is rewritten; every new field reads as before when missing.

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

var _ = registerJoinAuto()

func registerJoinAuto() bool {
	for _, t := range schemaTables {
		if t.cols != nil {
			continue
		}
		if t.Name == "employee" {
			t.Columns = append(t.Columns, Column{Name: "hst_wait", Type: TJSONB})
		}
	}
	migrations = append(migrations, migration{"022_join_auto", false, migrateJoinAuto})
	return true
}

// migrateJoinAuto is migration 022: the start of automatic sending to the Realtime portal. Safe to run again.
func migrateJoinAuto(ctx context.Context, s *Store) error {
	q := s.DB
	cs, err := q.C("company_setting").FindOne(ctx, bson.M{"_id": 1})
	if err != nil || cs == nil {
		return err
	}
	pw := map[string]any{}
	switch m := cs["portal_write"].(type) {
	case map[string]any:
		for k, v := range m {
			pw[k] = v
		}
	case bson.M:
		for k, v := range m {
			pw[k] = v
		}
	}
	if v, ok := pw["auto_since"].(string); ok && v != "" {
		return nil
	}
	pw["auto_since"] = time.Now().Format("2006-01-02")
	_, err = q.C("company_setting").Update(ctx, bson.M{"_id": 1}, bson.M{"$set": bson.M{"portal_write": pw}})
	return err
}
