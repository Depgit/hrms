package api

// Sending the offer letter again (owner, 27 Nov 2026): a candidate whose email does not work asks for the letter on
// WhatsApp or at another address. HR (onb.raise / onb.pipeline Edit) can, once the letter is out (released, or
// released at joining):
//   - email it to any address (the offer's or another one), optionally keeping that address on the offer;
//   - share it on WhatsApp: the browser opens wa.me with the letter's text (nothing goes out from the server), and
//     the HRMS records who shared it with which number.
// Each send is audited and written on the candidate's timeline. The server never calls WhatsApp.

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/mail"
	"github.com/saltstayz/hrms/internal/store"
)

// offerLetterOut loads the offer and its letter text, refusing when the letter is not out to the candidate.
func (s *Server) offerLetterOut(ctx context.Context, q store.Q, c *Ctx, id string) (store.Row, string, error) {
	o, err := s.loadOffer(ctx, q, c, id)
	if err != nil {
		return nil, "", err
	}
	switch str(o["status"]) {
	case "Declined", "Lapsed", "Withdrawn", "Rejected", "Cancelled":
		return nil, "", bad("This offer is %s: its letter is not sent", strings.ToLower(str(o["status"])))
	}
	letterView(ctx, q, o)
	if o["released_at"] == nil && o["letter_released_at"] == nil {
		return nil, "", bad("The offer letter can be sent once the offer is released")
	}
	if o["letterHeld"] == true {
		return nil, "", bad("The offer letter is held until joining: it is generated when the candidate is marked joined")
	}
	d := rowData(o)
	j := offerJob{Type: str(o["type"]), Gross: o["gross"].(float64), LocID: str(o["location_id"]), DOJ: str(o["doj"])}
	letter, err := s.offerLetter(ctx, o, offerPay(ctx, q, j, d))
	if err != nil || strings.TrimSpace(letter) == "" {
		return nil, "", bad("The offer letter could not be made: check the offer letter template")
	}
	return o, letter, nil
}

func offerLetterMail(company, name, to, letter string) mail.Message {
	text := fmt.Sprintf("Dear %s,\n\nPlease find your offer letter from %s below.\n\n%s\n\nIf you have a question, reply to this email.\n\n%s HR\n", name, company, letter, company)
	e := html.EscapeString
	h := fmt.Sprintf(`<div style="font-family:Arial,sans-serif;font-size:15px;line-height:1.5;color:#111;max-width:640px">
<p>Dear %s,</p><p>Please find your offer letter from %s below.</p>
<div style="white-space:pre-wrap;border:1px solid #ddd;border-radius:8px;padding:16px;font-size:14px">%s</div>
<p>If you have a question, reply to this email.</p><p>%s HR</p></div>`, e(name), e(company), e(letter), e(company))
	return mail.Message{To: to, Subject: "Your offer letter from " + company, Text: text, HTML: h}
}

// offerLetterSend: POST /api/offers/{id}/letter/send {via: email|whatsapp, to, save}.
func (s *Server) offerLetterSend(r *http.Request, c *Ctx) (any, error) {
	if !onbEditor(c) {
		return nil, forbidden("")
	}
	var b struct {
		Via  string `json:"via"`
		To   string `json:"to"`
		Save bool   `json:"save"` // email: keep this address on the offer from now on
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	to := strings.TrimSpace(b.To)
	switch b.Via {
	case "email":
		to = strings.ToLower(to)
		if !reEmail.MatchString(to) {
			return nil, bad("Enter a valid email address")
		}
		if s.mail == nil {
			return nil, bad("Email is not set up on this server. Share the letter on WhatsApp instead")
		}
	case "whatsapp":
		to = strings.TrimPrefix(strings.ReplaceAll(strings.ReplaceAll(to, " ", ""), "-", ""), "+91")
		if !reMobile.MatchString(to) {
			return nil, bad("Enter a 10-digit mobile number")
		}
	default:
		return nil, bad("Choose email or WhatsApp")
	}
	o, letter, err := s.offerLetterOut(ctx, s.st.DB, c, id)
	if err != nil {
		return nil, err
	}
	name, oid, cand := str(o["candidate_name"]), str(o["id"]), str(o["candidate_id"])
	// The HR who raised the offer is copied on the email (owner, 27 Nov 2026), when they have an email on record.
	hr, hrName := offerHR(ctx, s.st.DB, o)
	if b.Via == "email" {
		m := offerLetterMail(companyName(ctx, s.st.DB), name, to, letter)
		if hr != "" {
			m.Cc = []string{hr}
		}
		if err := s.mail.Send(ctx, m); err != nil {
			return nil, bad("The email could not be sent (%s). Share the letter on WhatsApp instead", err.Error())
		}
	}
	what := map[string]string{"email": "Offer letter emailed to " + to, "whatsapp": "Offer letter shared on WhatsApp with " + to}[b.Via]
	if b.Via == "email" && hr != "" && !strings.EqualFold(hr, to) {
		what += " (copy to " + hrName + ")"
	}
	err = s.st.Tx(ctx, func(tx store.Q) error {
		if b.Via == "email" && b.Save {
			d := rowData(o)
			if old := d["email"]; !strings.EqualFold(old, to) {
				if _, err := tx.C("offer").Update(ctx, bson.D{{Key: "_id", Value: oid}}, bson.D{{Key: "$set", Value: bson.D{{Key: "data.email", Value: to}}}}); err != nil {
					return err
				}
				if err := audit(ctx, tx, c, fmt.Sprintf("Email on offer %s changed to %s", oid, to), "offer", oid,
					map[string]string{"email": old}, map[string]string{"email": to}); err != nil {
					return err
				}
			}
		}
		if cand != "" {
			if err := lifeEvent(ctx, tx, c, lifeEv{Cand: cand, Emp: str(o["employee_id"]), Action: what}); err != nil {
				return err
			}
		}
		return audit(ctx, tx, c, what+" ("+name+")", "offer", oid, nil, map[string]string{"via": b.Via, "to": to})
	})
	if err != nil {
		return nil, err
	}
	out := map[string]any{"ok": true, "message": what}
	if b.Via == "whatsapp" {
		out["text"] = fmt.Sprintf("Dear %s,\n\nYour offer letter from %s:\n\n%s", name, companyName(ctx, s.st.DB), letter)
	}
	return out, nil
}

// offerHR is the email and name of the HR who raised the offer ("" when they have no email on record).
func offerHR(ctx context.Context, q store.Q, o store.Row) (string, string) {
	id := "" // the loaded offer carries the raiser's name: read the stored offer
	if x, _ := q.C("offer").FindOne(ctx, bson.M{"_id": o["id"]}); x != nil {
		id = str(x["raised_by"])
	}
	if id == "" {
		return "", ""
	}
	e, _ := q.C("employee").FindOne(ctx, bson.M{"_id": id})
	name := id
	if e != nil {
		name = or(str(e["name"]), id)
	}
	return authEmail(ctx, q, id), name
}
