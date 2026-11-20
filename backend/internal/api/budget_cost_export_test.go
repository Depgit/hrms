package api

import (
	"context"

	"github.com/saltstayz/hrms/internal/store"
)

// BudgetOver is the offer budget check for a hire of this type, property and gross (tests).
func BudgetOver(ctx context.Context, q store.Q, typ, locID string, gross float64) []string {
	return budgetOver(ctx, q, offerJob{Type: typ, LocID: locID, Gross: gross}, "")
}

// LocMonthlyCost is a property's projected total monthly cost (tests).
func LocMonthlyCost(ctx context.Context, q store.Q, loc string) (float64, error) {
	m, err := locMonthlyCost(ctx, q, map[string]any{"location_id": loc})
	return m[loc], err
}

// OfferMonthlyCost is a new hire's projected total monthly cost (tests).
func OfferMonthlyCost(ctx context.Context, q store.Q, typ, loc string, gross float64) float64 {
	return offerMonthlyCost(ctx, q, offerJob{Type: typ, LocID: loc, Gross: gross})
}
