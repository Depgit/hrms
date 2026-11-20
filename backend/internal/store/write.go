package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// docVals is a document as stored: field (`_id` for an id primary key) → value.
type docVals map[string]any

func valsOf(t *Table, raw bson.Raw) docVals {
	els, _ := raw.Elements()
	d := make(docVals, len(els))
	for _, el := range els {
		if el.Key() == lockField {
			continue
		}
		v := rawToGo(el.Value())
		if c := t.colOf(el.Key()); c != nil {
			v = readTyped(c, v)
		}
		d[el.Key()] = v
	}
	if v, err := raw.LookupErr("_id"); err == nil && v.Type == bson.TypeObjectID {
		d["_id"] = v.ObjectID()
	}
	return d
}

func unknownColumn(t *Table, k string) error {
	return errf("42703", "", "column %q of relation %q does not exist", k, t.Name)
}

// failingRow is the detail Postgres gave for a NOT NULL or CHECK violation.
func (t *Table) failingRow(get func(string) any) string {
	s := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		s[i] = pgText(get(c.Name))
	}
	return "Failing row contains (" + strings.Join(s, ", ") + ")."
}

func (t *Table) getter(d docVals) func(string) any {
	return func(col string) any { return d[t.field(col)] }
}

// checkRow enforces NOT NULL and every CHECK on a whole row (23514).
func (t *Table) checkRow(d docVals, jsonNull map[string]bool) error {
	get := t.getter(d)
	for _, c := range t.Columns {
		if c.NotNull && get(c.Name) == nil && !jsonNull[c.Name] {
			return &Error{Code: "23514", Message: fmt.Sprintf("null value in column %q of relation %q violates not-null constraint", c.Name, t.Name), Detail: t.failingRow(get)}
		}
	}
	for _, ch := range t.Checks {
		if !checkPasses(t, ch, get) {
			return &Error{Code: "23514", Message: fmt.Sprintf("new row for relation %q violates check constraint %q", t.Name, ch.Name), Detail: t.failingRow(get)}
		}
	}
	return nil
}

// checkPasses evaluates a CHECK like SQL: false only when some condition is false (a NULL makes it pass).
func checkPasses(t *Table, ch Check, get func(string) any) bool {
	for _, cd := range ch.Conds {
		v := get(cd.Col)
		if v == nil {
			continue
		}
		if s, ok := v.(string); ok {
			if c := t.Col(cd.Col); c != nil && c.Type == TChar {
				v = strings.TrimRight(s, " ")
			}
		}
		var rhs any = cd.Val
		if cd.ValCol != "" {
			rhs = get(cd.ValCol)
			if rhs == nil {
				continue
			}
		}
		switch cd.Op {
		case "in":
			found := false
			for _, x := range cd.Val.([]any) {
				if equalValues(v, x) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		case "~":
			re, _ := rhs.(*regexp.Regexp)
			s, _ := v.(string)
			if re != nil && !re.MatchString(s) {
				return false
			}
		default:
			cmp, ok := compareValues(v, rhs)
			if !ok {
				continue
			}
			pass := false
			switch cd.Op {
			case "=":
				pass = cmp == 0
			case "<>":
				pass = cmp != 0
			case ">":
				pass = cmp > 0
			case ">=":
				pass = cmp >= 0
			case "<":
				pass = cmp < 0
			case "<=":
				pass = cmp <= 0
			}
			if !pass {
				return false
			}
		}
	}
	return true
}

func equalValues(a, b any) bool {
	if fa, ok := asFloat(a); ok {
		fb, ok := asFloat(b)
		return ok && fa == fb
	}
	return a == b
}

func compareValues(a, b any) (int, bool) {
	if fa, ok := asFloat(a); ok {
		fb, ok := asFloat(b)
		if !ok {
			return 0, false
		}
		switch {
		case fa < fb:
			return -1, true
		case fa > fb:
			return 1, true
		}
		return 0, true
	}
	sa, ok1 := a.(string)
	sb, ok2 := b.(string)
	if ok1 && ok2 {
		return strings.Compare(sa, sb), true
	}
	return 0, false
}

// defaultOf fills a missing column on insert.
func (c Coll) defaultOf(ctx context.Context, col *Column, now time.Time, seq func(string) (int64, error)) (any, error) {
	switch col.Default.Kind {
	case DefConst:
		return coerce(c.t, col, col.Default.Value)
	case DefNow:
		return now, nil
	case DefToday:
		return DateOf(now), nil
	case DefUUID:
		return NewID(), nil
	case DefSeq:
		n, err := seq(col.Default.Seq)
		if err != nil {
			return nil, err
		}
		return coerce(c.t, col, n)
	}
	return nil, nil
}

func isTypedNilContainer(v any) bool {
	if v == nil {
		return false
	}
	rv := reflect.ValueOf(v)
	return (rv.Kind() == reflect.Map || rv.Kind() == reflect.Slice) && rv.IsNil()
}

// prepareInsert builds the stored document of an INSERT: every column, given values coerced, the rest from
// their defaults or null, then NOT NULL and CHECKs.
func (c Coll) prepareInsert(ctx context.Context, pairs bson.D, now time.Time, seq func(string) (int64, error)) (bson.D, docVals, error) {
	t := c.t
	given := make(map[string]any, len(pairs))
	var oid any
	for _, e := range pairs {
		k := e.Key
		if k == "id" && t.IDIsPK {
			k = "_id"
		}
		if k == "_id" && !t.IDIsPK {
			oid = e.Value
			continue
		}
		if t.colOf(k) == nil {
			return nil, nil, unknownColumn(t, e.Key)
		}
		if _, dup := given[k]; dup {
			return nil, nil, errf("42701", "", "column %q specified more than once", e.Key)
		}
		given[k] = e.Value
	}
	vals := make(docVals, len(t.Columns)+1)
	jsonNull := map[string]bool{}
	for i := range t.Columns {
		col := &t.Columns[i]
		f := t.field(col.Name)
		var v any
		var err error
		if g, ok := given[f]; ok {
			v, err = coerce(t, col, g)
			if col.Type == TJSONB && isTypedNilContainer(g) {
				jsonNull[col.Name] = true // a nil map/slice was JSON null, which NOT NULL allowed
			}
		} else {
			v, err = c.defaultOf(ctx, col, now, seq)
		}
		if err != nil {
			return nil, nil, err
		}
		vals[f] = v
	}
	if err := t.checkRow(vals, jsonNull); err != nil {
		return nil, nil, err
	}
	out := make(bson.D, 0, len(t.Columns)+1)
	if t.IDIsPK {
		out = append(out, bson.E{Key: "_id", Value: vals["_id"]})
	} else if oid != nil {
		out = append(out, bson.E{Key: "_id", Value: oid})
		vals["_id"] = oid
	}
	for _, col := range t.Columns {
		if f := t.field(col.Name); f != "_id" {
			out = append(out, bson.E{Key: f, Value: vals[f]})
		}
	}
	return out, vals, nil
}

func normKey(v any) string {
	if f, ok := asFloat(v); ok {
		return fmt.Sprintf("n:%v", f)
	}
	return fmt.Sprintf("%T:%v", v, v)
}

// checkFKs checks that every foreign key value written exists (23503), inside q's transaction. Rows of the same
// batch count, as Postgres checked at the end of the statement.
func (c Coll) checkFKs(ctx context.Context, docs []docVals) error {
	t := c.t
	for _, fk := range t.FKs {
		f := t.field(fk.Col)
		rt := tableByName[fk.RefTable]
		if rt == nil {
			continue
		}
		rf := rt.field(fk.RefCol)
		found := map[string]bool{}
		if rt == t {
			for _, d := range docs {
				if v, ok := d[rf]; ok && v != nil {
					found[normKey(v)] = true
				}
			}
		}
		var need []any
		seen := map[string]bool{}
		for _, d := range docs {
			v, ok := d[f]
			if !ok || v == nil {
				continue
			}
			k := normKey(v)
			if found[k] || seen[k] {
				continue
			}
			seen[k] = true
			need = append(need, v)
		}
		if len(need) == 0 {
			continue
		}
		bctx := c.bind(ctx)
		cur, err := c.q.db.Collection(rt.Name).Find(bctx, bson.D{{Key: rf, Value: bson.D{{Key: "$in", Value: need}}}},
			options.Find().SetProjection(bson.D{{Key: rf, Value: 1}}))
		if err != nil {
			return err
		}
		for cur.Next(bctx) {
			if v, err := cur.Current.LookupErr(rf); err == nil {
				found[normKey(readScalarOf(rt.colOf(rf), rawToGo(v)))] = true
			}
		}
		err = cur.Err()
		cur.Close(bctx)
		if err != nil {
			return err
		}
		for _, v := range need {
			if !found[normKey(v)] {
				return &Error{Code: "23503",
					Message: fmt.Sprintf("insert or update on table %q violates foreign key constraint %q", t.Name, fk.Name),
					Detail:  fmt.Sprintf("Key (%s)=(%s) is not present in table %q.", fk.Col, pgText(v), fk.RefTable)}
			}
		}
	}
	return nil
}

func readScalarOf(c *Column, v any) any {
	if c == nil {
		return v
	}
	return readScalar(c, v)
}

// checkNotReferenced refuses to delete (or change the referenced key of) documents another table still points at
// (23503), as the NO ACTION foreign keys did. changed(d, col) says whether d's col goes away.
func (c Coll) checkNotReferenced(ctx context.Context, olds []docVals, changed func(d docVals, col string) bool) error {
	t := c.t
	for _, rf := range t.refBy {
		f := t.field(rf.fk.RefCol)
		var vals, ids []any
		seen := map[string]bool{}
		for _, d := range olds {
			ids = append(ids, d["_id"])
			if !changed(d, rf.fk.RefCol) {
				continue
			}
			if v := d[f]; v != nil && !seen[normKey(v)] {
				seen[normKey(v)] = true
				vals = append(vals, v)
			}
		}
		if len(vals) == 0 {
			continue
		}
		ff := rf.table.field(rf.fk.Col)
		filter := bson.D{{Key: ff, Value: bson.D{{Key: "$in", Value: vals}}}}
		if rf.table == t {
			filter = append(filter, bson.E{Key: "_id", Value: bson.D{{Key: "$nin", Value: ids}}})
		}
		raw, err := c.q.db.Collection(rf.table.Name).FindOne(c.bind(ctx), filter, options.FindOne().SetProjection(bson.D{{Key: ff, Value: 1}})).Raw()
		if errors.Is(err, mongo.ErrNoDocuments) {
			continue
		}
		if err != nil {
			return err
		}
		v := rawToGo(raw.Lookup(ff))
		return &Error{Code: "23503",
			Message: fmt.Sprintf("update or delete on table %q violates foreign key constraint %q on table %q", t.Name, rf.fk.Name, rf.table.Name),
			Detail:  fmt.Sprintf("Key (%s)=(%s) is still referenced from table %q.", rf.fk.RefCol, pgText(v), rf.table.Name)}
	}
	return nil
}

func insertedID(t *Table, vals docVals) any {
	if t.IDIsPK {
		return vals["_id"]
	}
	if t.Col("id") != nil {
		return vals["id"]
	}
	return vals["_id"]
}

// ---------- INSERT ----------

// insertOne is INSERT … RETURNING id: it fills defaults and nulls, coerces values to the column types, enforces
// NOT NULL, CHECKs and foreign keys, and returns the id (the `_id` of an id primary key, else the id column).
func (c Coll) insertOne(ctx context.Context, doc any) (any, error) {
	if c.t == nil {
		res, err := c.mc.InsertOne(c.bind(ctx), doc)
		if err != nil {
			return nil, err
		}
		return res.InsertedID, nil
	}
	pairs, err := pairsOf(doc)
	if err != nil {
		return nil, err
	}
	d, vals, err := c.prepareInsert(ctx, pairs, c.q.Now(), func(s string) (int64, error) { return NextSeq(ctx, c.q, s) })
	if err != nil {
		return nil, err
	}
	if err := c.checkFKs(ctx, []docVals{vals}); err != nil {
		return nil, err
	}
	res, err := c.mc.InsertOne(c.bind(ctx), d)
	if err != nil {
		return nil, mapErr(c.t, err)
	}
	if !c.t.IDIsPK && vals["_id"] == nil {
		vals["_id"] = res.InsertedID
	}
	return insertedID(c.t, vals), nil
}

// insertMany inserts documents in order as one statement (CopyFrom / a multi-row INSERT): one now() for all,
// sequence values reserved as a block, foreign keys checked together.
func (c Coll) insertMany(ctx context.Context, docs []any) error {
	if len(docs) == 0 {
		return nil
	}
	if c.t == nil {
		_, err := c.mc.InsertMany(c.bind(ctx), docs)
		return err
	}
	all := make([]bson.D, len(docs))
	need := map[string]int64{}
	for i, d := range docs {
		p, err := pairsOf(d)
		if err != nil {
			return err
		}
		all[i] = p
		for _, col := range c.t.Columns {
			if col.Default.Kind != DefSeq {
				continue
			}
			f := c.t.field(col.Name)
			if _, ok := lookup(p, f); ok {
				continue
			}
			if f == "_id" {
				if _, ok := lookup(p, "id"); ok {
					continue
				}
			}
			need[col.Default.Seq]++
		}
	}
	next := map[string]int64{}
	for s, n := range need {
		first, err := nextSeqBlock(ctx, c.q, s, n)
		if err != nil {
			return err
		}
		next[s] = first
	}
	seq := func(s string) (int64, error) {
		v, ok := next[s]
		if !ok {
			return NextSeq(ctx, c.q, s)
		}
		next[s] = v + 1
		return v, nil
	}
	now := c.q.Now()
	out := make([]any, len(docs))
	valsAll := make([]docVals, len(docs))
	for i, p := range all {
		d, vals, err := c.prepareInsert(ctx, p, now, seq)
		if err != nil {
			return err
		}
		out[i], valsAll[i] = d, vals
	}
	if err := c.checkFKs(ctx, valsAll); err != nil {
		return err
	}
	_, err := c.mc.InsertMany(c.bind(ctx), out)
	return mapErr(c.t, err)
}

// ---------- UPDATE ----------

type updOps struct {
	doc      any               // what is sent to Mongo
	set      docVals           // whole-column values set (coerced; $unset of a column becomes null)
	soi      docVals           // $setOnInsert values (coerced)
	arith    map[string]bson.E // column → {op, operand} for $inc/$mul/$min/$max
	touched  map[string]bool   // top-level fields any operator changes
	pipeline bool
	needPre  bool // post-image checks (multi-column CHECK, arithmetic on a checked column, a referenced key)
}

// prepareUpdate coerces an update document's values and decides which checks it needs.
func (c Coll) prepareUpdate(update any) (*updOps, error) {
	t := c.t
	ops := &updOps{set: docVals{}, soi: docVals{}, arith: map[string]bson.E{}, touched: map[string]bool{}}
	if isPipeline(update) {
		ops.doc, ops.pipeline = update, true
		return ops, nil
	}
	pairs, err := pairsOf(update)
	if err != nil {
		return nil, err
	}
	var out bson.D
	var setD, soiD bson.D
	for _, op := range pairs {
		if !strings.HasPrefix(op.Key, "$") {
			return nil, fmt.Errorf("store: an update of %s needs operators ($set, $inc, …), got field %q", t.Name, op.Key)
		}
		body, err := pairsOf(op.Value)
		if err != nil {
			return nil, err
		}
		switch op.Key {
		case "$set", "$setOnInsert":
			for _, e := range body {
				k, v, err := c.setValue(e.Key, e.Value)
				if err != nil {
					return nil, err
				}
				if op.Key == "$set" {
					setD = append(setD, bson.E{Key: k, Value: v})
					if k == topField(k) {
						ops.set[k] = v
					}
					ops.touched[topField(k)] = true
				} else {
					soiD = append(soiD, bson.E{Key: k, Value: v})
					if k == topField(k) {
						ops.soi[k] = v
					}
				}
			}
		case "$unset":
			var keep bson.D
			for _, e := range body {
				k := e.Key
				if k == "id" && t.IDIsPK {
					k = "_id"
				}
				col := t.colOf(topField(k))
				if col == nil {
					return nil, unknownColumn(t, e.Key)
				}
				if k == topField(k) {
					setD = append(setD, bson.E{Key: k, Value: nil}) // nulls are stored explicitly
					ops.set[k] = nil
				} else {
					keep = append(keep, e)
				}
				ops.touched[topField(k)] = true
			}
			if len(keep) > 0 {
				out = append(out, bson.E{Key: "$unset", Value: keep})
			}
		case "$inc", "$mul", "$min", "$max":
			var body2 bson.D
			for _, e := range body {
				k := e.Key
				if k == "id" && t.IDIsPK {
					k = "_id"
				}
				if k == lockField {
					body2 = append(body2, e)
					continue
				}
				col := t.colOf(topField(k))
				if col == nil {
					return nil, unknownColumn(t, e.Key)
				}
				v := e.Value
				if k == topField(k) {
					if op.Key == "$min" || op.Key == "$max" {
						v, err = coerce(t, col, v)
					} else {
						v, err = coerceOperand(t, col, v)
					}
					if err != nil {
						return nil, err
					}
					ops.arith[k] = bson.E{Key: op.Key, Value: v}
				} else if col.Type == TJSONB {
					v = jsonSub(v)
				}
				body2 = append(body2, bson.E{Key: k, Value: v})
				ops.touched[topField(k)] = true
			}
			out = append(out, bson.E{Key: op.Key, Value: body2})
		case "$push", "$addToSet", "$pull", "$pullAll":
			var body2 bson.D
			for _, e := range body {
				col := t.colOf(topField(e.Key))
				if col == nil {
					return nil, unknownColumn(t, e.Key)
				}
				v, err := c.elementValue(col, e.Key, e.Value, op.Key == "$pullAll")
				if err != nil {
					return nil, err
				}
				body2 = append(body2, bson.E{Key: e.Key, Value: v})
				ops.touched[topField(e.Key)] = true
			}
			out = append(out, bson.E{Key: op.Key, Value: body2})
		case "$rename":
			return nil, fmt.Errorf("store: $rename would drop a column of %s", t.Name)
		default:
			for _, e := range body {
				if t.colOf(topField(e.Key)) == nil && e.Key != lockField {
					return nil, unknownColumn(t, e.Key)
				}
				ops.touched[topField(e.Key)] = true
			}
			out = append(out, op)
		}
	}
	if len(setD) > 0 {
		out = append(bson.D{{Key: "$set", Value: setD}}, out...)
	}
	if len(soiD) > 0 {
		out = append(out, bson.E{Key: "$setOnInsert", Value: soiD})
	}
	ops.doc = out
	// Which changes need the whole row to check?
	for _, ch := range t.Checks {
		cols := map[string]bool{}
		for _, cd := range ch.Conds {
			cols[t.field(cd.Col)] = true
			if cd.ValCol != "" {
				cols[t.field(cd.ValCol)] = true
			}
		}
		hit := false
		for f := range cols {
			if ops.touched[f] {
				hit = true
			}
			if _, ok := ops.arith[f]; ok {
				ops.needPre = true
			}
		}
		if hit && len(cols) > 1 {
			ops.needPre = true
		}
		if hit && !ops.needPre {
			// One column, set to a known value: check it now.
			for f := range cols {
				if v, ok := ops.set[f]; ok {
					d := docVals{f: v}
					if !checkPasses(t, ch, t.getter(d)) {
						return nil, &Error{Code: "23514", Message: fmt.Sprintf("new row for relation %q violates check constraint %q", t.Name, ch.Name),
							Detail: fmt.Sprintf("%s = %s", t.colOf(f).Name, pgText(v))}
					}
				} else if ops.touched[f] {
					ops.needPre = true
				}
			}
		}
	}
	for f, v := range ops.set {
		if col := t.colOf(f); col != nil && col.NotNull && v == nil {
			return nil, &Error{Code: "23514", Message: fmt.Sprintf("null value in column %q of relation %q violates not-null constraint", col.Name, t.Name)}
		}
	}
	for _, rf := range t.refBy {
		if ops.touched[t.field(rf.fk.RefCol)] {
			ops.needPre = true
		}
	}
	return ops, nil
}

// setValue coerces one $set entry.
func (c Coll) setValue(key string, v any) (string, any, error) {
	t := c.t
	k := key
	if k == "id" && t.IDIsPK {
		k = "_id"
	}
	if k == lockField {
		return k, v, nil
	}
	col := t.colOf(topField(k))
	if col == nil {
		return "", nil, unknownColumn(t, key)
	}
	if k == topField(k) {
		cv, err := coerce(t, col, v)
		if err == nil && cv == nil && col.NotNull && col.Type == TJSONB && isTypedNilContainer(deref(v)) {
			return k, nil, nil
		}
		return k, cv, err
	}
	if col.Type == TJSONB {
		return k, jsonSub(v), nil
	}
	return k, v, nil
}

// jsonSub stores a value inside a jsonb column as JSON would (strings stay strings).
func jsonSub(v any) any {
	out, err := toJSONValue(mustJSON(v))
	if err != nil {
		return v
	}
	return out
}

func mustJSON(v any) []byte {
	b, err := jsonMarshal(plainJSON(v))
	if err != nil {
		return []byte("null")
	}
	return b
}

// coerceOperand coerces a $inc/$mul operand to the column's number type.
func coerceOperand(t *Table, col *Column, v any) (any, error) {
	switch col.Type {
	case TInt2, TInt4:
		n, ok := toInt(deref(v))
		if !ok {
			return nil, badValue(t, col, v)
		}
		return int32(n), nil
	case TInt8:
		n, ok := toInt(deref(v))
		if !ok {
			return nil, badValue(t, col, v)
		}
		return n, nil
	case TNumeric:
		f, ok := toFloat(deref(v))
		if !ok {
			return nil, badValue(t, col, v)
		}
		return f, nil
	}
	return nil, errf("42883", "", "operator does not exist: %s + number (column %s.%s)", col.sqlType(), t.Name, col.Name)
}

// elementValue coerces the operand of $push/$addToSet/$pull on an array or jsonb column.
func (c Coll) elementValue(col *Column, key string, v any, list bool) (any, error) {
	t := c.t
	el := func(x any) (any, error) {
		if col.Type == TJSONB {
			return jsonSub(x), nil
		}
		if col.Array {
			return coerceScalar(t, col, x)
		}
		return x, nil
	}
	if key != topField(key) {
		return v, nil
	}
	if d, ok := v.(bson.D); ok {
		if each, ok := lookup(d, "$each"); ok {
			l, err := c.elementValue(col, key, each, true)
			if err != nil {
				return nil, err
			}
			out := bson.D{}
			for _, e := range d {
				if e.Key == "$each" {
					e.Value = l
				}
				out = append(out, e)
			}
			return out, nil
		}
		return v, nil // a condition ($pull: {col: {$in: …}})
	}
	if m, ok := v.(bson.M); ok {
		if each, ok := m["$each"]; ok {
			l, err := c.elementValue(col, key, each, true)
			if err != nil {
				return nil, err
			}
			out := bson.M{}
			for k, e := range m {
				out[k] = e
			}
			out["$each"] = l
			return out, nil
		}
		if isOperatorDoc(m) {
			return v, nil
		}
	}
	if list {
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
			out := make([]any, rv.Len())
			for i := range out {
				x, err := el(rv.Index(i).Interface())
				if err != nil {
					return nil, err
				}
				out[i] = x
			}
			return out, nil
		}
	}
	return el(v)
}

// postImage applies an update's whole-column changes to a stored document, for the checks.
func postImage(old docVals, ops *updOps) docVals {
	n := make(docVals, len(old))
	for k, v := range old {
		n[k] = v
	}
	for k, v := range ops.set {
		n[k] = v
	}
	for k, e := range ops.arith {
		cur, ok1 := asFloat(n[k])
		x, ok2 := asFloat(e.Value)
		switch e.Key {
		case "$inc":
			if ok1 && ok2 {
				n[k] = cur + x
			} else if n[k] == nil && ok2 {
				n[k] = x
			}
		case "$mul":
			if ok1 && ok2 {
				n[k] = cur * x
			} else if n[k] == nil {
				n[k] = 0.0
			}
		case "$min":
			if c, ok := compareValues(e.Value, n[k]); n[k] == nil || (ok && c < 0) {
				n[k] = e.Value
			}
		case "$max":
			if c, ok := compareValues(e.Value, n[k]); n[k] == nil || (ok && c > 0) {
				n[k] = e.Value
			}
		}
	}
	return n
}

// validatePre runs the whole-row checks on documents about to be updated.
func (c Coll) validatePre(ctx context.Context, olds []docVals, ops *updOps) error {
	for _, o := range olds {
		if err := c.t.checkRow(postImage(o, ops), nil); err != nil {
			return err
		}
	}
	return c.checkNotReferenced(ctx, olds, func(d docVals, col string) bool {
		f := c.t.field(col)
		if !ops.touched[f] {
			return false
		}
		nv, known := ops.set[f]
		return !known || !reflect.DeepEqual(nv, d[f])
	})
}

func (c Coll) preRead(ctx context.Context, filter any, many bool, sort any) ([]docVals, error) {
	o := options.Find()
	if !many {
		o.SetLimit(1)
	}
	if sort != nil {
		o.SetSort(sort)
	}
	bctx := c.bind(ctx)
	cur, err := c.mc.Find(bctx, filter, o)
	if err != nil {
		return nil, err
	}
	defer cur.Close(bctx)
	var out []docVals
	for cur.Next(bctx) {
		out = append(out, valsOf(c.t, cur.Current))
	}
	return out, cur.Err()
}

func idsOf(ds []docVals) []any {
	ids := make([]any, len(ds))
	for i, d := range ds {
		ids[i] = d["_id"]
	}
	return ids
}

func (c Coll) update(ctx context.Context, filter, update any, many bool) (int64, error) {
	if c.t == nil {
		var res *mongo.UpdateResult
		var err error
		if many {
			res, err = c.mc.UpdateMany(c.bind(ctx), c.filter(filter), update)
		} else {
			res, err = c.mc.UpdateOne(c.bind(ctx), c.filter(filter), update)
		}
		if err != nil {
			return 0, err
		}
		return res.MatchedCount, nil
	}
	if c.t.AppendOnly != "" {
		return 0, appendOnly(c.t)
	}
	f := c.filter(filter)
	ops, err := c.prepareUpdate(update)
	if err != nil {
		return 0, err
	}
	if ops.needPre {
		olds, err := c.preRead(ctx, f, many, nil)
		if err != nil {
			return 0, err
		}
		if len(olds) == 0 {
			return 0, nil
		}
		if err := c.validatePre(ctx, olds, ops); err != nil {
			return 0, err
		}
		f = bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: idsOf(olds)}}}}
	}
	if err := c.checkFKs(ctx, []docVals{ops.set}); err != nil {
		return 0, err
	}
	var res *mongo.UpdateResult
	if many {
		res, err = c.mc.UpdateMany(c.bind(ctx), f, ops.doc)
	} else {
		res, err = c.mc.UpdateOne(c.bind(ctx), f, ops.doc)
	}
	if err != nil {
		return 0, mapErr(c.t, err)
	}
	return res.MatchedCount, nil
}

// equalities are the filter's field = value conditions (what an upsert inserts).
func (c Coll) equalities(f any) docVals {
	out := docVals{}
	var pairs bson.D
	switch x := f.(type) {
	case bson.D:
		pairs = x
	case bson.M:
		pairs = sortedPairs(x)
	default:
		return out
	}
	for _, e := range pairs {
		if strings.HasPrefix(e.Key, "$") || strings.Contains(e.Key, ".") {
			continue
		}
		v := e.Value
		switch x := v.(type) {
		case bson.D:
			if eq, ok := lookup(x, "$eq"); ok && len(x) == 1 {
				v = eq
			} else if len(x) > 0 && strings.HasPrefix(x[0].Key, "$") {
				continue
			}
		case bson.M:
			if eq, ok := x["$eq"]; ok && len(x) == 1 {
				v = eq
			} else if isOperatorDoc(x) {
				continue
			}
		}
		out[e.Key] = v
	}
	return out
}

// insertSide completes an upsert's $setOnInsert with every column the insert would otherwise miss (defaults or
// null), and checks the row an insert would create.
func (c Coll) insertSide(ctx context.Context, f any, ops *updOps) (bson.D, []docVals, error) {
	t := c.t
	img := docVals{}
	var add bson.D
	for k, v := range c.equalities(f) {
		col := t.colOf(k)
		if col == nil {
			return nil, nil, unknownColumn(t, k)
		}
		cv, err := coerce(t, col, v)
		if err != nil {
			return nil, nil, err
		}
		img[k] = cv
		if k != "_id" && !reflect.DeepEqual(cv, v) {
			add = append(add, bson.E{Key: k, Value: cv})
		}
	}
	for k, v := range ops.set {
		img[k] = v
	}
	for k, v := range ops.soi {
		img[k] = v
	}
	now := c.q.Now()
	for i := range t.Columns {
		col := &t.Columns[i]
		fd := t.field(col.Name)
		if _, ok := img[fd]; ok || ops.touched[fd] {
			continue
		}
		v, err := c.defaultOf(ctx, col, now, func(s string) (int64, error) { return NextSeq(ctx, c.q, s) })
		if err != nil {
			return nil, nil, err
		}
		img[fd] = v
		add = append(add, bson.E{Key: fd, Value: v})
	}
	if err := t.checkRow(img, nil); err != nil {
		return nil, nil, err
	}
	return add, []docVals{img}, nil
}

func withSetOnInsert(doc any, add bson.D) any {
	d, _ := doc.(bson.D)
	if len(add) == 0 {
		return d
	}
	for i, e := range d {
		if e.Key == "$setOnInsert" {
			body, _ := e.Value.(bson.D)
			d[i].Value = append(append(bson.D{}, body...), add...)
			return d
		}
	}
	return append(d, bson.E{Key: "$setOnInsert", Value: add})
}

// upsert is INSERT … ON CONFLICT: set is written on both paths (DO UPDATE SET; empty for DO NOTHING), setOnInsert
// only when a document is inserted; the filter's equality fields, set, setOnInsert and every column's default or
// null make up the inserted document. It reports whether a document was inserted.
func (c Coll) upsert(ctx context.Context, filter any, set, setOnInsert bson.M) (bool, error) {
	if c.t == nil {
		u := bson.D{}
		if len(set) > 0 {
			u = append(u, bson.E{Key: "$set", Value: set})
		}
		if len(setOnInsert) > 0 {
			u = append(u, bson.E{Key: "$setOnInsert", Value: setOnInsert})
		}
		res, err := c.mc.UpdateOne(c.bind(ctx), c.filter(filter), u, options.UpdateOne().SetUpsert(true))
		if err != nil {
			return false, err
		}
		return res.UpsertedCount > 0, nil
	}
	if c.t.AppendOnly != "" && len(set) > 0 {
		return false, appendOnly(c.t)
	}
	f := c.filter(filter)
	u := bson.D{}
	if len(set) > 0 {
		u = append(u, bson.E{Key: "$set", Value: bson.M(set)})
	}
	if len(setOnInsert) > 0 {
		soi := bson.M{}
		for k, v := range setOnInsert {
			if _, both := set[k]; !both {
				soi[k] = v
			}
		}
		if len(soi) > 0 {
			u = append(u, bson.E{Key: "$setOnInsert", Value: soi})
		}
	}
	ops, err := c.prepareUpdate(u)
	if err != nil {
		return false, err
	}
	add, img, err := c.insertSide(ctx, f, ops)
	if err != nil {
		return false, err
	}
	if ops.needPre && len(set) > 0 {
		olds, err := c.preRead(ctx, f, false, nil)
		if err != nil {
			return false, err
		}
		if err := c.validatePre(ctx, olds, ops); err != nil {
			return false, err
		}
	}
	fkDocs := append(img, ops.set)
	if err := c.checkFKs(ctx, fkDocs); err != nil {
		return false, err
	}
	doc := withSetOnInsert(ops.doc, add)
	if d, _ := doc.(bson.D); len(d) == 0 {
		doc = bson.D{{Key: "$setOnInsert", Value: bson.D{{Key: lockField, Value: int32(0)}}}}
	}
	res, err := c.mc.UpdateOne(c.bind(ctx), f, doc, options.UpdateOne().SetUpsert(true))
	if err != nil {
		return false, mapErr(c.t, err)
	}
	return res.UpsertedCount > 0, nil
}

// findOneAndUpdate is UPDATE … RETURNING for one document: it returns the document after the update (unless the
// options ask for the one before), or nil when nothing matched. With SetUpsert(true) a missing document is inserted
// with every column filled, as Upsert does.
func (c Coll) findOneAndUpdate(ctx context.Context, filter, update any, opts ...options.Lister[options.FindOneAndUpdateOptions]) (Row, error) {
	args := apply(opts)
	o := adjust(opts, func(x *options.FindOneAndUpdateOptions) {
		if x.ReturnDocument == nil {
			after := options.After
			x.ReturnDocument = &after
		}
		x.Sort, x.Projection = c.keys(x.Sort), c.keys(x.Projection)
	})
	f := c.filter(filter)
	if c.t == nil {
		raw, err := c.mc.FindOneAndUpdate(c.bind(ctx), f, update, o).Raw()
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return rowOf(nil, raw), nil
	}
	if c.t.AppendOnly != "" {
		return nil, appendOnly(c.t)
	}
	ops, err := c.prepareUpdate(update)
	if err != nil {
		return nil, err
	}
	upsert := args.Upsert != nil && *args.Upsert
	if ops.needPre {
		olds, err := c.preRead(ctx, f, false, c.keys(args.Sort))
		if err != nil {
			return nil, err
		}
		if len(olds) > 0 {
			if err := c.validatePre(ctx, olds, ops); err != nil {
				return nil, err
			}
			f = bson.D{{Key: "_id", Value: olds[0]["_id"]}}
		} else if !upsert {
			return nil, nil
		}
	}
	fkDocs := []docVals{ops.set}
	doc := ops.doc
	if upsert && !ops.pipeline {
		add, img, err := c.insertSide(ctx, f, ops)
		if err != nil {
			return nil, err
		}
		fkDocs = append(fkDocs, img...)
		doc = withSetOnInsert(doc, add)
	}
	if err := c.checkFKs(ctx, fkDocs); err != nil {
		return nil, err
	}
	raw, err := c.mc.FindOneAndUpdate(c.bind(ctx), f, doc, o).Raw()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, mapErr(c.t, err)
	}
	return rowOf(c.t, raw), nil
}

// ---------- DELETE ----------

func (c Coll) del(ctx context.Context, filter any, many bool) (int64, error) {
	f := c.filter(filter)
	if c.t != nil {
		if c.t.AppendOnly != "" {
			return 0, appendOnly(c.t)
		}
		if len(c.t.refBy) > 0 {
			olds, err := c.preRead(ctx, f, many, nil)
			if err != nil {
				return 0, err
			}
			if len(olds) == 0 {
				return 0, nil
			}
			if err := c.checkNotReferenced(ctx, olds, func(docVals, string) bool { return true }); err != nil {
				return 0, err
			}
			f = bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: idsOf(olds)}}}}
			many = true
		}
	}
	var res *mongo.DeleteResult
	var err error
	if many {
		res, err = c.mc.DeleteMany(c.bind(ctx), f)
	} else {
		res, err = c.mc.DeleteOne(c.bind(ctx), f)
	}
	if err != nil {
		return 0, err
	}
	return res.DeletedCount, nil
}

// ---------- locks ----------

// lock is SELECT … FOR UPDATE of one document inside a transaction: it bumps the document's hidden _lock field, so
// a concurrent transaction that locks or writes the same document conflicts and is retried after this one commits.
// It returns the document (nil if none matches). Outside a transaction it is a plain FindOne.
func (c Coll) lock(ctx context.Context, filter any, opts ...options.Lister[options.FindOneOptions]) (Row, error) {
	if !c.q.InTx() {
		return c.FindOne(ctx, filter, opts...)
	}
	args := apply(opts)
	o := options.FindOneAndUpdate().SetReturnDocument(options.After)
	if args.Sort != nil {
		o.SetSort(c.keys(args.Sort))
	}
	raw, err := c.mc.FindOneAndUpdate(c.bind(ctx), c.filter(filter), bson.D{{Key: "$inc", Value: bson.D{{Key: lockField, Value: int32(1)}}}}, o).Raw()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rowOf(c.t, raw), nil
}

// lockAll is SELECT … FOR UPDATE of every matching document (see Lock); it returns them like Find.
func (c Coll) lockAll(ctx context.Context, filter any, opts ...options.Lister[options.FindOptions]) ([]Row, error) {
	if c.q.InTx() {
		if _, err := c.mc.UpdateMany(c.bind(ctx), c.filter(filter), bson.D{{Key: "$inc", Value: bson.D{{Key: lockField, Value: int32(1)}}}}); err != nil {
			return nil, err
		}
	}
	return c.Find(ctx, filter, opts...)
}
