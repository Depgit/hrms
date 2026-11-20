package api

import "net/http"

// routesB1 registers the routes of the 14 Nov 2026 additions (job roles and skills use the Masters routes).
func (s *Server) routesB1(m *http.ServeMux) {
	// PAN verification: the provider setting (HR Admin) and "Verify PAN" on an employee (b1_pan.go).
	m.HandleFunc("GET /api/settings/pan-verify", s.a(s.panSettingsGet))
	m.HandleFunc("PUT /api/settings/pan-verify", s.a(s.panSettingsPut))
	m.HandleFunc("POST /api/employees/{id}/pan/verify", s.a(s.employeePanVerify))
	// Bulk update › ESIC and medical cards: a ZIP, checked then applied (b1_bulk.go). More specific than
	// /api/bulk/{tpl}/{action}, so it takes the ZIP before the sheet reader sees it.
	m.HandleFunc("POST /api/bulk/cards/{action}", s.a(s.cardsRun))
}
