package api

import (
	"context"

	"github.com/saltstayz/hrms/internal/store"
)

// PayEligible is the payroll's filter of who is paid in month ym (tests).
func PayEligible(ctx context.Context, q store.Q, ym string) (any, error) {
	return payEligible(ctx, q, ym)
}
