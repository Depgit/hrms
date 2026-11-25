package store

// Registry additions for "Offer at joining" (owner, 26 Nov 2026): lower-level hires get no offer after the interview;
// the offer is raised on the joining day, approved as usual, and joining is the acceptance (api/offer_at_joining.go).
//
//   - candidate.oaj_at / oaj_by / oaj_doj / oaj_note: HR chose "Offer at joining" for this selected candidate, with the
//     expected joining date and an optional note. Cleared when HR switches back to the usual offer.
//   - offer.at_joining: the offer was raised at joining. It is never released or answered: Mark joined (after
//     approval) records the acceptance, issues the Offer ID, freezes the version and releases the letter.
//
// No migration: every new field reads as before when missing.

var _ = registerOfferJoin()

func registerOfferJoin() bool {
	for _, t := range schemaTables {
		if t.cols != nil {
			continue
		}
		switch t.Name {
		case "candidate":
			t.Columns = append(t.Columns, Column{Name: "oaj_at", Type: TTimestamptz}, Column{Name: "oaj_by", Type: TText},
				Column{Name: "oaj_doj", Type: TDate}, Column{Name: "oaj_note", Type: TText})
		case "offer":
			t.Columns = append(t.Columns, Column{Name: "at_joining", Type: TBool})
		}
	}
	return true
}
