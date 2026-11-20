package store

// Registry addition for the ICICI bank upload file (owner, 14 Nov 2026): the company's debit account that every
// salary payment in the upload is paid from, kept with the pay schedule (Payroll › Pay schedule and components):
//
//   - icici_debit_acc: digits only, 9 to 18 (the API checks the format). Text, so leading zeros are kept.
//
// It is null until someone saves it; the app then reads DefaultICICIDebitAcc, so no migration is needed. The
// PostgreSQL build never had it; an import leaves it null.

// DefaultICICIDebitAcc is the debit account the owner gave. ICICI's template says the debit account has 12 digits;
// this one has 11. It is kept as given and can be changed in Pay schedule.
const DefaultICICIDebitAcc = "38605005062"

var _ = registerBankFile()

func registerBankFile() bool {
	for _, t := range schemaTables {
		if t.Name == "company_setting" && t.cols == nil {
			t.Columns = append(t.Columns, Column{Name: "icici_debit_acc", Type: TText})
		}
	}
	return true
}
