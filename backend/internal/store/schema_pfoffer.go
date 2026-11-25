package store

// Registry notes for the PF answer on the offer (owner, 26 Nov 2026); no new column and no migration:
//
//   - offer.data.pfWant ("Yes"/"No"): HR's record of "Does the employee want PF?" on the offer form, inside the
//     offer's data like data.accom (required for a type PF applies to; never stored for a Retainer). On saving it is
//     also copied to offer.data.pfChoice, the joining form's "Do you want PF deducted from your salary?", which the
//     joiner (or HR in the joining details) may change; the joining form's answer wins (api/pf_offer.go).
//   - employee_pay.pf_choice_by may now read PfChoiceByOffer ("Offer") when the answer in force came from the offer
//     ("Joining form" or HR's name as before).
//   - Offers raised before 26 Nov 2026 have no pfWant and read as before (the joining form's answer only).

// PfChoiceByOffer is employee_pay.pf_choice_by for a PF choice taken from the offer.
const PfChoiceByOffer = "Offer"
