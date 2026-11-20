package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/saltstayz/hrms/internal/jobs"
	"github.com/saltstayz/hrms/internal/mdm"
	"github.com/saltstayz/hrms/internal/store"
)

// Master data sync (SOP-ARCH-MDM-01, internal/mdm): HRMS publishes employee events as the source of truth for
// employee_code, and keeps a read-only copy of the CMS's properties (property_id) that locations link to.
//
// Consumer routes (no HRMS session):
//
//	GET  /api/mdm/events?after=&limit=        pull feed            Authorization: Bearer <API key>
//	GET  /api/mdm/employees?after=&limit=     published snapshots  Authorization: Bearer <API key>
//	GET  /api/mdm/employees/summary           count + checksum     Authorization: Bearer <API key>
//	POST /api/mdm/cms/events                  CMS property events  X-CMS-Timestamp + X-CMS-Signature
//
// Settings (Masters › Company › Integrations) are HR Admin only, like the Google Drive connection (drive.go).

// mdmInit makes the service; the key is the one that seals the Google Drive token (Options.SecretKey).
func (s *Server) mdmInit() {
	s.mdm = mdm.New(s.st, mdm.Options{Key: s.drive.key})
}

// mdmStartJobs registers the delivery and reconciliation workers and their periodic jobs (StartJobs, before the
// client is made).
func (s *Server) mdmStartJobs(workers *jobs.Workers) []*jobs.PeriodicJob {
	s.mdm.AddWorkers(workers)
	return []*jobs.PeriodicJob{mdm.DeliverPeriodicJob(), mdm.ReconcilePeriodicJob()}
}

// mdmRun starts the change-stream watcher (one server at a time holds its lease) once the job client runs.
func (s *Server) mdmRun(ctx context.Context, c *jobs.Client) {
	s.mdm.Start(context.WithoutCancel(ctx), c, c.ID())
}

func (s *Server) mdmRoutes(m *http.ServeMux) {
	m.Handle("GET /api/mdm/events", s.mdm.EventsHandler())
	m.Handle("GET /api/mdm/employees", s.mdm.EmployeesHandler())
	m.Handle("GET /api/mdm/employees/summary", s.mdm.SummaryHandler())
	m.Handle("POST /api/mdm/cms/events", s.mdm.CMSHandler())

	m.HandleFunc("GET /api/mdm/settings", s.a(s.mdmOverview))
	m.HandleFunc("PUT /api/mdm/settings/cms", s.a(s.mdmSaveCMS))
	m.HandleFunc("POST /api/mdm/settings/cms/secret", s.a(s.mdmRotateInbound))
	m.HandleFunc("POST /api/mdm/subscribers", s.a(s.mdmAddSubscriber))
	m.HandleFunc("PATCH /api/mdm/subscribers/{id}", s.a(s.mdmUpdateSubscriber))
	m.HandleFunc("DELETE /api/mdm/subscribers/{id}", s.a(s.mdmDeleteSubscriber))
	m.HandleFunc("POST /api/mdm/subscribers/{id}/secret", s.a(s.mdmRotateSubscriber))
	m.HandleFunc("POST /api/mdm/subscribers/{id}/test", s.a(s.mdmTestSubscriber))
	m.HandleFunc("POST /api/mdm/keys", s.a(s.mdmAddKey))
	m.HandleFunc("PATCH /api/mdm/keys/{id}", s.a(s.mdmUpdateKey))
	m.HandleFunc("DELETE /api/mdm/keys/{id}", s.a(s.mdmDeleteKey))
	m.HandleFunc("POST /api/mdm/keys/{id}/secret", s.a(s.mdmRotateKey))
	m.HandleFunc("GET /api/mdm/outbox", s.a(s.mdmOutbox))
	m.HandleFunc("GET /api/mdm/reconcile", s.a(s.mdmRuns))
	m.HandleFunc("POST /api/mdm/reconcile", s.a(s.mdmReconcileNow))

	// Locations master: the CMS's properties to pick from, and linking an existing location (once).
	m.HandleFunc("GET /api/mdm/properties", s.a(s.mdmProperties))
	m.HandleFunc("POST /api/mdm/locations/{id}/link", s.a(s.mdmLinkLocation))
}

func mdmAdmin(c *Ctx) error {
	if c.Role != "HR Admin" {
		return forbidden("Only HR Admin can manage integrations")
	}
	return nil
}

// mdmErr shows a refusal from internal/mdm as the API's own.
func mdmErr(err error) error {
	if e, ok := mdm.AsError(err); ok {
		return &apiError{Status: e.Status, Message: e.Message}
	}
	return err
}

func mdmBy(c *Ctx) string { return c.Name + " (" + c.EmployeeID + ")" }

// mdmLimit reads ?limit= (def when absent, at most hi).
func mdmLimit(r *http.Request, def, hi int64) (int64, error) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 1 || n > hi {
		return 0, bad("limit must be a number from 1 to %d", hi)
	}
	return n, nil
}

func (s *Server) mdmOverview(r *http.Request, c *Ctx) (any, error) {
	if err := mdmAdmin(c); err != nil {
		return nil, err
	}
	return s.mdm.Overview(r.Context())
}

func (s *Server) mdmSaveCMS(r *http.Request, c *Ctx) (any, error) {
	if err := mdmAdmin(c); err != nil {
		return nil, err
	}
	var in mdm.SettingsInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	out, err := s.mdm.SaveSettings(r.Context(), mdmBy(c), in)
	return out, mdmErr(err)
}

func (s *Server) mdmRotateInbound(r *http.Request, c *Ctx) (any, error) {
	if err := mdmAdmin(c); err != nil {
		return nil, err
	}
	sec, view, err := s.mdm.RotateInboundSecret(r.Context(), mdmBy(c))
	if err != nil {
		return nil, mdmErr(err)
	}
	return map[string]any{"secret": sec, "settings": view}, nil
}

func (s *Server) mdmAddSubscriber(r *http.Request, c *Ctx) (any, error) {
	if err := mdmAdmin(c); err != nil {
		return nil, err
	}
	var in mdm.SubscriberInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	sub, sec, err := s.mdm.AddSubscriber(r.Context(), mdmBy(c), in)
	if err != nil {
		return nil, mdmErr(err)
	}
	return map[string]any{"subscriber": sub, "secret": sec}, nil
}

func (s *Server) mdmUpdateSubscriber(r *http.Request, c *Ctx) (any, error) {
	if err := mdmAdmin(c); err != nil {
		return nil, err
	}
	var in mdm.SubscriberInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	out, err := s.mdm.UpdateSubscriber(r.Context(), r.PathValue("id"), in)
	return out, mdmErr(err)
}

func (s *Server) mdmDeleteSubscriber(r *http.Request, c *Ctx) (any, error) {
	if err := mdmAdmin(c); err != nil {
		return nil, err
	}
	return nil, mdmErr(s.mdm.DeleteSubscriber(r.Context(), r.PathValue("id")))
}

func (s *Server) mdmRotateSubscriber(r *http.Request, c *Ctx) (any, error) {
	if err := mdmAdmin(c); err != nil {
		return nil, err
	}
	sub, sec, err := s.mdm.RotateSubscriberSecret(r.Context(), r.PathValue("id"))
	if err != nil {
		return nil, mdmErr(err)
	}
	return map[string]any{"subscriber": sub, "secret": sec}, nil
}

func (s *Server) mdmTestSubscriber(r *http.Request, c *Ctx) (any, error) {
	if err := mdmAdmin(c); err != nil {
		return nil, err
	}
	out, err := s.mdm.SendTest(r.Context(), r.PathValue("id"))
	return out, mdmErr(err)
}

func (s *Server) mdmAddKey(r *http.Request, c *Ctx) (any, error) {
	if err := mdmAdmin(c); err != nil {
		return nil, err
	}
	var b struct {
		Name string `json:"name"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	k, key, err := s.mdm.AddKey(r.Context(), mdmBy(c), b.Name)
	if err != nil {
		return nil, mdmErr(err)
	}
	return map[string]any{"key": k, "secret": key}, nil
}

func (s *Server) mdmUpdateKey(r *http.Request, c *Ctx) (any, error) {
	if err := mdmAdmin(c); err != nil {
		return nil, err
	}
	var b struct {
		Active *bool `json:"active"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Active == nil {
		return nil, bad("Nothing to change")
	}
	out, err := s.mdm.SetKeyActive(r.Context(), r.PathValue("id"), *b.Active)
	return out, mdmErr(err)
}

func (s *Server) mdmDeleteKey(r *http.Request, c *Ctx) (any, error) {
	if err := mdmAdmin(c); err != nil {
		return nil, err
	}
	return nil, mdmErr(s.mdm.DeleteKey(r.Context(), r.PathValue("id")))
}

func (s *Server) mdmRotateKey(r *http.Request, c *Ctx) (any, error) {
	if err := mdmAdmin(c); err != nil {
		return nil, err
	}
	k, key, err := s.mdm.RotateKey(r.Context(), r.PathValue("id"))
	if err != nil {
		return nil, mdmErr(err)
	}
	return map[string]any{"key": k, "secret": key}, nil
}

func (s *Server) mdmOutbox(r *http.Request, c *Ctx) (any, error) {
	if err := mdmAdmin(c); err != nil {
		return nil, err
	}
	n, err := mdmLimit(r, 50, 500)
	if err != nil {
		return nil, err
	}
	return s.mdm.RecentEvents(r.Context(), n)
}

func (s *Server) mdmRuns(r *http.Request, c *Ctx) (any, error) {
	if err := mdmAdmin(c); err != nil {
		return nil, err
	}
	n, err := mdmLimit(r, 20, 200)
	if err != nil {
		return nil, err
	}
	return s.mdm.Runs(r.Context(), n)
}

func (s *Server) mdmReconcileNow(r *http.Request, c *Ctx) (any, error) {
	if err := mdmAdmin(c); err != nil {
		return nil, err
	}
	out, err := s.mdm.Reconcile(r.Context(), "manual")
	return out, mdmErr(err)
}

// mdmProperties is for the Locations master: whoever may see the org masters' edit forms may pick a property.
func (s *Server) mdmProperties(r *http.Request, c *Ctx) (any, error) {
	if c.Role != "HR Admin" && c.Level("mas.org") < 1 {
		return nil, forbidden("")
	}
	return s.mdm.CMSProperties(r.Context())
}

// mdmLinkLocation links an existing location to its CMS property, once (Masters › Locations).
func (s *Server) mdmLinkLocation(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("mas.org", 2); err != nil {
		return nil, err
	}
	var b struct {
		PropertyID string `json:"propertyId"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	var notes []string
	var row store.Row
	err := s.st.Tx(r.Context(), func(tx store.Q) error {
		var err error
		if notes, err = s.mdm.LinkLocation(r.Context(), tx, r.PathValue("id"), b.PropertyID, mdm.Actor{Name: mdmBy(c), Role: c.Role}); err != nil {
			return err
		}
		row, err = tx.C("location").FindOne(r.Context(), map[string]any{"_id": r.PathValue("id")})
		if err != nil || row == nil {
			return err
		}
		return mdm.LocationFlags(r.Context(), tx, []store.Row{row})
	})
	if err != nil {
		return nil, mdmErr(err)
	}
	if notes == nil {
		notes = []string{}
	}
	return map[string]any{"location": row, "notes": notes}, nil
}
