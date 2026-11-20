package api

import "time"

// SetBulkTxBudget changes how long one transaction of a bulk upload works rows (tests), returning the undo.
func SetBulkTxBudget(d time.Duration) (undo func()) {
	old := bulkTxBudget
	bulkTxBudget = d
	return func() { bulkTxBudget = old }
}
