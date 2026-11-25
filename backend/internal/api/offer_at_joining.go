package api

// Offer at joining (owner, 26 Nov 2026). Lower-level hires get no offer after the interview: the offer is given when
// they join. For any selected candidate HR may choose "Offer at joining" (the right to raise an offer) instead of
// "Raise offer v1":
//
//   - Choosing it asks the expected joining date and an optional note. The candidate waits in its own pipeline column
//     ("Offer at joining"). HR can still raise the usual offer (that switches back) or close the candidate while no
//     offer is raised; Reschedule joining moves the expected date; No show / Offer declined close as usual.
//   - On the joining day HR presses "Raise offer at joining": the same offer form (the date prefilled), saved as
//     offer v1 with offer.at_joining, and it goes through Offer approval exactly as usual. It is never released or
//     answered, and the hiring sweep never lapses it.
//   - Mark joined waits for that approval ("Waiting for offer approval"). Once approved, Mark joined records the
//     acceptance (joining is the acceptance): the Offer ID is issued, the version frozen, the letter released at
//     joining, then the usual joining (hireJoin). Everything after joining is unchanged.

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// oajOpen: HR chose Offer at joining and the candidate has not joined (nor been closed) yet.
func oajOpen(x store.Row) bool {
	return x["stage"] == "Offer" && x["oaj_at"] != nil && x["accepted_at"] == nil && x["declined_at"] == nil
}

// oajDOJ is the expected joining date: the offer's once raised, else the one HR gave when choosing.
func oajDOJ(x store.Row) string {
	if d := str(x["doj"]); d != "" && x["offer_id"] != nil {
		return d
	}
	return str(x["oaj_doj"])
}

// oajOfferLive: an offer at joining is raised and not rejected or withdrawn.
func oajOfferLive(x store.Row) bool {
	return x["offer_id"] != nil && x["offer_status"] != "Rejected" && x["offer_status"] != "Cancelled"
}

// oajReady says why Mark joined must wait for an offer-at-joining candidate ("" when it can go ahead).
func oajReady(x store.Row) string {
	switch {
	case x["offer_id"] == nil:
		return "Raise the offer at joining first. It goes for Offer approval; Mark joined comes after the approval"
	case x["at_joining"] != true:
		return "Mark joined once the candidate has accepted the offer"
	case x["offer_status"] == "Pending":
		return "Waiting for offer approval. Mark joined once the offer at joining is approved"
	case x["offer_status"] != "Approved":
		return "The offer at joining was not approved. Revise it or close the candidate"
	}
	return ""
}

// oajAccept records the acceptance of an approved offer at joining at Mark joined: the offer released and accepted
// now, its letter held for joining (hireJoin releases it), the Offer ID issued and the version frozen.
func oajAccept(ctx context.Context, tx store.Q, c *Ctx, x store.Row, date string) error {
	cand := str(x["id"])
	o, err := tx.C("offer").FindOne(ctx, bson.D{{Key: "candidate_id", Value: cand}})
	if err != nil || o == nil {
		return orNotFound(err)
	}
	oi := str(x["offer_no"])
	if oi == "" {
		oi = stageID("OI", toInt(x["no"]))
	}
	now := tx.Now()
	if _, err := tx.C("offer").Update(ctx, bson.D{{Key: "_id", Value: o["id"]}}, bson.D{{Key: "$set", Value: bson.D{{Key: "released_at", Value: now},
		{Key: "accepted_at", Value: now}, {Key: "letter_at", Value: "joining"}, {Key: "offer_no", Value: oi}}}}); err != nil {
		return err
	}
	if _, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: cand}}, bson.D{{Key: "$set", Value: bson.D{{Key: "offer_no", Value: oi}}}}); err != nil {
		return err
	}
	ver := max(1, toInt(o["version"]))
	if err := offerVersionSet(ctx, tx, cand, ver, bson.D{{Key: "status", Value: "Accepted"}, {Key: "frozen", Value: true}, {Key: "released_at", Value: now},
		{Key: "answered_at", Value: now}, {Key: "answered_by", Value: c.Name}, {Key: "answer", Value: "Accepted at joining"}}); err != nil {
		return err
	}
	x["accepted_at"], x["offer_no"] = now, oi
	return lifeEvent(ctx, tx, c, lifeEv{Cand: cand, Action: "Offer accepted at joining: Offer ID " + oi, From: "Offer at joining approved", To: "Offer accepted",
		Date: date, Reason: fmt.Sprintf("Joining is the acceptance. Offer v%d frozen", ver)})
}

// oajCancelPending withdraws an offer at joining still waiting for approval when the candidate is closed.
func oajCancelPending(ctx context.Context, tx store.Q, c *Ctx, x store.Row) error {
	if x["offer_status"] != "Pending" || str(x["offer_request_id"]) == "" {
		return nil
	}
	id := str(x["offer_request_id"])
	if _, err := tx.C("approval_request").Update(ctx, bson.M{"_id": id, "status": "Pending"}, bson.M{"$set": bson.M{"status": "Cancelled"}}); err != nil {
		return err
	}
	by := "System"
	if c != nil {
		by = c.Name
	}
	return history(ctx, tx, id, "", "Cancelled", by, "The candidate was closed before joining")
}

// oajSwitchBack clears the choice when HR raises the usual offer instead (offerCreate).
func oajSwitchBack(ctx context.Context, tx store.Q, c *Ctx, cand store.Row) error {
	if cand["oaj_at"] == nil {
		return nil
	}
	id := str(cand["id"])
	if _, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: id}}, bson.D{{Key: "$set", Value: bson.D{{Key: "oaj_at", Value: nil},
		{Key: "oaj_by", Value: nil}, {Key: "oaj_doj", Value: nil}, {Key: "oaj_note", Value: nil}}}}); err != nil {
		return err
	}
	return lifeEvent(ctx, tx, c, lifeEv{Cand: id, Action: "Switched back to the usual offer", From: "Offer at joining", To: "Selected",
		Reason: "HR raised the offer to be released and answered before joining"})
}

// candidateOfferAtJoining chooses Offer at joining for a selected candidate (POST /api/candidates/{id}/offer-at-joining).
func (s *Server) candidateOfferAtJoining(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("onb.raise", 2); err != nil {
		return nil, err
	}
	var b struct {
		Date string `json:"date"`
		Note string `json:"note"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Note = strings.TrimSpace(b.Note)
	switch {
	case !isDate(b.Date):
		return nil, bad("Enter the expected joining date")
	case b.Date < today():
		return nil, bad("The expected joining date is today or later")
	case len(b.Note) > 1000:
		return nil, bad("Keep the note under 1,000 characters")
	}
	ctx := r.Context()
	var msg string
	err := s.st.Tx(ctx, func(tx store.Q) error {
		x, err := s.loadCandidate(ctx, tx, c, r.PathValue("id"), true)
		if err != nil {
			return err
		}
		id := str(x["id"])
		switch {
		case x["stage"] != "Offer":
			return bad("Offer at joining is chosen for a selected candidate, before the offer")
		case x["oaj_at"] != nil:
			return bad("Offer at joining is already chosen, expected on %s. Use Reschedule joining to move the date", longDate(str(x["oaj_doj"])))
		case oajOfferLive(x):
			return bad("An offer is already raised for %s", id)
		case x["waiting_window"] != nil:
			return bad("%s is waiting (no-show). Finish that first", id)
		}
		if _, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: id}}, bson.D{{Key: "$set", Value: bson.D{{Key: "oaj_at", Value: tx.Now()},
			{Key: "oaj_by", Value: c.Name}, {Key: "oaj_doj", Value: b.Date}, {Key: "oaj_note", Value: strPtr(b.Note)}}}}); err != nil {
			return err
		}
		why := "Expected to join on " + longDate(b.Date) + ". The offer is raised on the joining day, approved as usual; joining is the acceptance"
		if b.Note != "" {
			why += ". " + b.Note
		}
		if err := lifeEvent(ctx, tx, c, lifeEv{Cand: id, Action: "Offer at joining chosen", From: "Selected", To: "Offer at joining", Date: b.Date, Reason: why}); err != nil {
			return err
		}
		msg = fmt.Sprintf("%s: offer at joining, expected on %s", x["name"], longDate(b.Date))
		return audit(ctx, tx, c, fmt.Sprintf("Offer at joining chosen for %s (%s)", x["name"], id), "candidate", id, nil, b)
	})
	return map[string]string{"result": msg}, err
}
