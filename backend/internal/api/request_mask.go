package api

import (
	"fmt"
	"regexp"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/store"
)

// maskRequestPayload hides what a request's payload says about the subject's bank account from a viewer without
// the bank right (ESS-151, ESS-152, PPL-043): the account number and IFSC of a bank change follow the viewer's
// bank sensitivity (Full / Masked / Hidden), as the employee record does. The subject and the person who raised it
// see their own values in full. The payload is copied, never changed in place.
func maskRequestPayload(c *Ctx, row store.Row) {
	p, ok := row["payload"].(map[string]any)
	if !ok || p == nil {
		return
	}
	if p["accountNo"] == nil && p["ifsc"] == nil {
		return
	}
	owner := approvalSubjectID(row)
	if raised, _ := row["raised_by"].(string); raised == c.EmployeeID && owner != c.EmployeeID {
		owner = c.EmployeeID // what the caller typed
	}
	cp := make(map[string]any, len(p))
	for k, v := range p {
		cp[k] = v
	}
	for _, k := range []string{"accountNo", "ifsc"} {
		if v, ok := cp[k]; ok {
			if m := c.Show("bank", v, owner); m == nil {
				delete(cp, k)
			} else {
				cp[k] = m
			}
		}
	}
	row["payload"] = cp
}

// rePANInText finds PAN numbers written into free text, such as a joining verification's correction note.
var rePANInText = regexp.MustCompile(`\b[A-Z]{5}[0-9]{4}[A-Z]\b`)

// maskHistoryIDs masks PAN numbers in a request's history notes for a viewer who does not see government IDs in
// full (HIRE-121): the verifier's correction note keeps "PAN old → new" for Payroll, while a Line Manager who opens
// the verification reads it masked.
func maskHistoryIDs(c *Ctx, owner string, rows []store.Row) {
	if owner == c.EmployeeID || c.Sensitive["govId"] == access.Full {
		return
	}
	for _, h := range rows {
		if n, ok := h["note"].(string); ok && rePANInText.MatchString(n) {
			h["note"] = rePANInText.ReplaceAllStringFunc(n, func(p string) string {
				if m := c.Show("govId", p, owner); m != nil {
					return fmt.Sprint(m)
				}
				return "(hidden)"
			})
		}
	}
}
