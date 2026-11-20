package store

import (
	"regexp"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The schema registry: every former PostgreSQL table with its columns, keys, indexes, foreign keys and CHECKs.
// schema_gen.go holds the data (generated once from the Postgres schema, edited by hand since). The write wrapper
// enforces it and migration 001 builds the collections and indexes from it.

// ColType is a column's PostgreSQL type.
type ColType string

const (
	TText        ColType = "text"
	TUUID        ColType = "uuid"
	TChar        ColType = "char" // character(n): blank-padded to Len
	TInt2        ColType = "int2" // read back as int16, as pgx did
	TInt4        ColType = "int4" // int32
	TInt8        ColType = "int8" // int64
	TNumeric     ColType = "numeric"
	TBool        ColType = "bool"
	TDate        ColType = "date"        // stored "YYYY-MM-DD"
	TTime        ColType = "time"        // stored "HH:MM"
	TTimestamptz ColType = "timestamptz" // BSON date
	TJSONB       ColType = "jsonb"       // embedded document / array, numbers as doubles (as JSON had them)
	TBytea       ColType = "bytea"
)

// DefaultKind says how a missing column is filled on insert.
type DefaultKind int

const (
	DefNone  DefaultKind = iota // NULL
	DefConst                    // Value (coerced to the column type, so "{}" is an empty jsonb object)
	DefNow                      // now(): the transaction's instant
	DefToday                    // current_date in the business zone
	DefUUID                     // gen_random_uuid()
	DefSeq                      // nextval(Seq)
)

// Default is a column default.
type Default struct {
	Kind  DefaultKind
	Value any
	Seq   string
}

// Column is one column. Array columns (text[], uuid[], date[]) hold elements of Type.
type Column struct {
	Name    string
	Type    ColType
	Array   bool
	NotNull bool
	Default Default
	Prec    int // numeric(p,s): p (0: unconstrained)
	Scale   int // numeric(p,s): s
	Len     int // character(n): n
}

// IndexKey is one key of an index; Field is the stored field name (`_id` for an id primary key).
type IndexKey struct {
	Field string
	Desc  bool
}

// Index is a unique constraint/index or a plain index.
type Index struct {
	Name     string
	Keys     []IndexKey
	Unique   bool
	Partial  bson.D // partialFilterExpression (unique indexes on nullable columns, WHERE … indexes)
	Lower    bool   // unique on lower(col): collation strength 2
	Wildcard bool   // gin index on a jsonb column: a wildcard index on its paths
	Note     string // the original definition, when it was translated loosely
}

// FK is a foreign key column → RefTable.RefCol (all of them were NO ACTION / RESTRICT).
type FK struct {
	Name     string
	Col      string
	RefTable string
	RefCol   string
}

// Cond is one comparison of a CHECK: Col Op Val, or Col Op ValCol when ValCol is set.
// Op: "in" (Val []any), "=", "<>", ">", ">=", "<", "<=" (Val float64 or string), "~" (Val regexp).
type Cond struct {
	Col    string
	Op     string
	Val    any
	ValCol string
}

// Check is a CHECK constraint: the AND of its conditions; like SQL, it passes when a condition is NULL.
type Check struct {
	Name  string
	Expr  string
	Conds []Cond
}

// Table is one former table, now a collection of the same name.
type Table struct {
	Name    string
	Columns []Column
	// PK lists the primary key columns. A single-column key named id is stored as _id (IDIsPK); any other key
	// gets an automatic ObjectID _id plus a unique index on the key columns (in Indexes).
	PK      []string
	IDIsPK  bool
	Indexes []Index
	FKs     []FK
	Checks  []Check
	// AppendOnly is the error message of a trigger that refused UPDATE and DELETE (audit_log).
	AppendOnly string

	cols  map[string]*Column
	refBy []refFrom // foreign keys that reference this table
}

type refFrom struct {
	table *Table
	fk    FK
}

// Sequence is a former Postgres sequence, now a document in the counters collection.
type Sequence struct {
	Name  string
	Start int64
}

var (
	tableByName = map[string]*Table{}
	seqStart    = map[string]int64{}
)

func init() {
	for _, t := range schemaTables {
		t.cols = map[string]*Column{}
		for i := range t.Columns {
			t.cols[t.Columns[i].Name] = &t.Columns[i]
		}
		tableByName[t.Name] = t
	}
	for _, t := range schemaTables {
		for _, fk := range t.FKs {
			if rt := tableByName[fk.RefTable]; rt != nil {
				rt.refBy = append(rt.refBy, refFrom{t, fk})
			}
		}
		for i := range t.Checks {
			for j := range t.Checks[i].Conds {
				c := &t.Checks[i].Conds[j]
				if c.Op == "~" {
					if s, ok := c.Val.(string); ok {
						c.Val = regexp.MustCompile(s)
					}
				}
			}
		}
	}
	for _, s := range schemaSequences {
		seqStart[s.Name] = s.Start
	}
}

// Tables lists the registry's tables in name order.
func Tables() []*Table { return schemaTables }

// TableOf returns the registry entry of a collection, or nil.
func TableOf(name string) *Table { return tableByName[name] }

// Sequences lists the registry's sequences.
func Sequences() []Sequence { return schemaSequences }

// Col returns a column, or nil.
func (t *Table) Col(name string) *Column {
	if t == nil {
		return nil
	}
	return t.cols[name]
}

// field is the stored field of a column (`_id` for an id primary key).
func (t *Table) field(col string) string {
	if t != nil && t.IDIsPK && col == "id" {
		return "_id"
	}
	return col
}

// colOf is the column stored in field f (the id column for `_id`).
func (t *Table) colOf(f string) *Column {
	if t == nil {
		return nil
	}
	if f == "_id" {
		if t.IDIsPK {
			return t.cols["id"]
		}
		return nil
	}
	return t.cols[f]
}

func (c *Column) sqlType() string {
	s := string(c.Type)
	switch c.Type {
	case TChar:
		s = "character"
	case TInt2:
		s = "smallint"
	case TInt4:
		s = "integer"
	case TInt8:
		s = "bigint"
	case TBool:
		s = "boolean"
	case TTime:
		s = "time without time zone"
	case TTimestamptz:
		s = "timestamp with time zone"
	}
	if c.Array {
		s += "[]"
	}
	return s
}

func isOperatorDoc(m map[string]any) bool {
	for k := range m {
		if strings.HasPrefix(k, "$") {
			return true
		}
	}
	return false
}
