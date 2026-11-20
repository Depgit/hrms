// Package api is the REST API (spec 20.1). Every handler derives scope from the role
// mapping and rejects data outside it.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/saltstayz/hrms/internal/store"
)

type apiError struct {
	Status  int    `json:"-"`
	Message string `json:"error"`
	Details any    `json:"details,omitempty"`
}

func (e *apiError) Error() string { return e.Message }

func bad(msg string, a ...any) error { return &apiError{Status: 400, Message: fmt.Sprintf(msg, a...)} }
func badD(msg string, details any) error {
	return &apiError{Status: 400, Message: msg, Details: details}
}
func forbidden(msg string) error {
	if msg == "" {
		msg = "You do not have access to this"
	}
	return &apiError{Status: 403, Message: msg}
}
func notFound(msg string) error {
	if msg == "" {
		msg = "Not found"
	}
	return &apiError{Status: 404, Message: msg}
}
func unauthorized(msg string) error { return &apiError{Status: 401, Message: msg} }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		writeJSON(w, ae.Status, ae)
		return
	}
	switch code, detail := store.ErrCode(err); code {
	case "23505":
		writeJSON(w, 409, apiError{Message: "That already exists", Details: detail})
		return
	case "23503":
		writeJSON(w, 400, apiError{Message: "A referenced record does not exist", Details: detail})
		return
	case "23514", "22P02", "22007", "22008":
		writeJSON(w, 400, apiError{Message: "A value is not valid", Details: detail})
		return
	}
	log.Printf("error: %v", err)
	writeJSON(w, 500, apiError{Message: "Something went wrong"})
}

// decode reads a JSON body strictly into v.
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 20<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return bad("Check the fields: %s", strings.TrimPrefix(err.Error(), "json: "))
	}
	return nil
}

type handlerFn func(r *http.Request) (any, error)

func (s *Server) h(fn handlerFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := fn(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		if v == nil {
			v = map[string]bool{"ok": true}
		}
		writeJSON(w, 200, v)
	}
}

type authedFn func(r *http.Request, c *Ctx) (any, error)

// a wraps a handler that needs a signed-in caller.
func (s *Server) a(fn authedFn) http.HandlerFunc {
	return s.h(func(r *http.Request) (any, error) {
		c, err := s.authenticate(r)
		if err != nil {
			return nil, err
		}
		s.resolveAlias(r)         // an old Joining ID finds the record under its Employee ID (hiring.go)
		r = machineOverrideReq(r) // "go ahead" into a full punch-machine cluster (machines.go)
		// HR skips approval of the request this call raises (approvals.go skipApprovalTx).
		if q := r.URL.Query(); r.Method != http.MethodGet && q.Has("skipApproval") {
			c.Skip, c.SkipReason = true, strings.TrimSpace(q.Get("skipApproval"))
		}
		return fn(r, c)
	})
}
