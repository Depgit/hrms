package store

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Error is a database rule the store refused a write for, with the PostgreSQL error code the old database
// gave for the same thing (23505 duplicate, 23503 foreign key, 23514 check or not null, 22P02 bad value,
// 22007 bad date or time, 22003 out of range, 22001 too long, P0001 append-only table, 42703 unknown column).
type Error struct {
	Code    string
	Message string
	Detail  string
	Err     error // the driver error, if any
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return e.Message + " (" + e.Detail + ")"
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

func errf(code, detail, msg string, a ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(msg, a...), Detail: detail}
}

// ErrCode returns the PostgreSQL-style error code and detail of a store error (or of a raw driver duplicate-key
// error), or "", "" (it replaces PgCode).
func ErrCode(err error) (code, detail string) {
	var e *Error
	if errors.As(err, &e) {
		return e.Code, e.Detail
	}
	if err != nil && mongo.IsDuplicateKeyError(err) {
		d := dupError(nil, err)
		return d.Code, d.Detail
	}
	return "", ""
}

// mapErr turns a driver error from a write on table t into a store Error where the old database had a code.
func mapErr(t *Table, err error) error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return err
	}
	if mongo.IsDuplicateKeyError(err) {
		return dupError(t, err)
	}
	return err
}

var dupIndexRe = regexp.MustCompile(`index: (\S+) dup key`)

// dupError builds 23505 with a Postgres-like detail: Key (col, col)=(val, val) already exists.
func dupError(t *Table, err error) *Error {
	e := &Error{Code: "23505", Err: err}
	var keyValue bson.Raw
	var keyPattern bson.Raw
	index := ""
	collect := func(raw bson.Raw, msg string) {
		if raw != nil {
			if v, err := raw.LookupErr("keyValue"); err == nil {
				keyValue, _ = v.DocumentOK()
			}
			if v, err := raw.LookupErr("keyPattern"); err == nil {
				keyPattern, _ = v.DocumentOK()
			}
		}
		if m := dupIndexRe.FindStringSubmatch(msg); m != nil {
			index = m[1]
		}
	}
	var we mongo.WriteException
	var bwe mongo.BulkWriteException
	var ce mongo.CommandError
	switch {
	case errors.As(err, &we):
		for _, w := range we.WriteErrors {
			if w.Code == 11000 || w.Code == 11001 || w.Code == 12582 {
				collect(w.Raw, w.Message)
				break
			}
		}
	case errors.As(err, &bwe):
		for _, w := range bwe.WriteErrors {
			if w.Code == 11000 || w.Code == 11001 || w.Code == 12582 {
				collect(w.Raw, w.Message)
				break
			}
		}
	case errors.As(err, &ce):
		collect(ce.Raw, ce.Message)
	}
	var cols, vals []string
	src := keyValue
	if src == nil {
		src = keyPattern
	}
	if src != nil {
		els, _ := src.Elements()
		for _, el := range els {
			k := el.Key()
			if k == "_id" {
				k = "id"
			}
			cols = append(cols, k)
			if keyValue != nil {
				vals = append(vals, pgText(rawToGo(el.Value())))
			}
		}
	}
	table := ""
	if t != nil {
		table = t.Name
	}
	if index == "" || index == "_id_" {
		if t != nil {
			index = t.Name + "_pkey"
		}
	}
	e.Message = fmt.Sprintf("duplicate key value violates unique constraint %q", index)
	if len(cols) > 0 && len(vals) == len(cols) {
		e.Detail = fmt.Sprintf("Key (%s)=(%s) already exists.", strings.Join(cols, ", "), strings.Join(vals, ", "))
	} else if len(cols) > 0 {
		e.Detail = fmt.Sprintf("Key (%s) already exists.", strings.Join(cols, ", "))
	} else if table != "" {
		e.Detail = fmt.Sprintf("A document in %s already has this key.", table)
	}
	return e
}

// pgText formats a value as Postgres printed it in error details.
func pgText(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case time.Time:
		return x.In(Loc()).Format("2006-01-02 15:04:05.999999-07")
	case []any:
		s := make([]string, len(x))
		for i, e := range x {
			s[i] = pgText(e)
		}
		return "{" + strings.Join(s, ",") + "}"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		s := make([]string, len(keys))
		for i, k := range keys {
			s[i] = fmt.Sprintf("%q: %s", k, pgText(x[k]))
		}
		return "{" + strings.Join(s, ", ") + "}"
	}
	return fmt.Sprint(v)
}

// appendOnly is the error of the audit_log trigger (RAISE EXCEPTION → P0001).
func appendOnly(t *Table) error {
	return &Error{Code: "P0001", Message: t.AppendOnly}
}

// Transient reports whether err (or an error it wraps) is a transient transaction error, such as the write
// conflict a Lock gets while another transaction holds the document. Tx runs the transaction again on such an
// error, which is how a Lock waits as SELECT … FOR UPDATE did; a handler must return it unchanged rather than
// turn it into a "not found" or other answer.
func Transient(err error) bool {
	var le mongo.LabeledError
	return err != nil && errors.As(err, &le) && le.HasErrorLabel("TransientTransactionError")
}
