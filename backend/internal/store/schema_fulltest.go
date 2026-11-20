package store

// Registry additions from the full test of 13 Nov 2026 (areas other than payroll). Every column is new and null on
// records written before; no migration is needed. The PostgreSQL build never had them; an import leaves them null.
//
//   - auth_otp.verify_hash (AUTH-023): the hash of the one-time token /api/auth/otp/verify returns; setting the
//     password must present the token, so knowing the Employee ID is not enough. An OTP verified before this change
//     has none: the person verifies again.

var _ = registerFullTest()

func registerFullTest() bool {
	add := map[string][]Column{
		"auth_otp": {{Name: "verify_hash", Type: TText}},
	}
	for _, t := range schemaTables {
		if cols, ok := add[t.Name]; ok && t.cols == nil {
			t.Columns = append(t.Columns, cols...)
		}
	}
	return true
}
