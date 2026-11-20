package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Coll is one collection, bound to a Q (and so to its transaction, if any). Reads return Rows typed from the
// registry; writes apply the registry's rules. Collections outside the registry (counters, locks,
// schema_migrations) pass through untouched.
type Coll struct {
	q    Q
	name string
	t    *Table
	mc   *mongo.Collection
}

// Name is the collection name.
func (c Coll) Name() string { return c.name }

// Raw is the driver's collection. Writes made with it bypass the store's rules.
func (c Coll) Raw() *mongo.Collection { return c.mc }

// bind attaches the transaction's session (or strips any session outside a transaction).
func (c Coll) bind(ctx context.Context) context.Context { return c.q.Bind(ctx) }

// lister is an options lister built from other listers plus a final adjustment.
type lister[T any] struct{ fns []func(*T) error }

func (l *lister[T]) List() []func(*T) error { return l.fns }

func nilLister(o any) bool {
	if o == nil {
		return true
	}
	v := reflect.ValueOf(o)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

func adjust[T any](opts []options.Lister[T], fix func(*T)) *lister[T] {
	var fns []func(*T) error
	for _, o := range opts {
		if !nilLister(o) {
			fns = append(fns, o.List()...)
		}
	}
	fns = append(fns, func(t *T) error { fix(t); return nil })
	return &lister[T]{fns}
}

func apply[T any](opts []options.Lister[T]) *T {
	var t T
	for _, o := range opts {
		if nilLister(o) {
			continue
		}
		for _, f := range o.List() {
			_ = f(&t)
		}
	}
	return &t
}

// ---------- filters ----------

// filter translates a filter for this collection: top-level `id` → `_id` (also inside $and/$or/$nor) when the
// primary key is id, and time.Time values compared with a date column → YYYY-MM-DD. nil matches everything.
func (c Coll) filter(f any) any {
	if f == nil {
		return bson.D{}
	}
	if c.t == nil {
		return f
	}
	switch x := f.(type) {
	case bson.D:
		out := make(bson.D, 0, len(x))
		for _, e := range x {
			k, v := c.filterPair(e.Key, e.Value)
			out = append(out, bson.E{Key: k, Value: v})
		}
		return out
	case bson.M:
		out := make(bson.M, len(x))
		for k, v := range x {
			k2, v2 := c.filterPair(k, v)
			out[k2] = v2
		}
		return out
	case map[string]any:
		out := make(bson.M, len(x))
		for k, v := range x {
			k2, v2 := c.filterPair(k, v)
			out[k2] = v2
		}
		return out
	}
	return f
}

func (c Coll) filterPair(k string, v any) (string, any) {
	switch k {
	case "$and", "$or", "$nor":
		switch l := v.(type) {
		case bson.A:
			out := make(bson.A, len(l))
			for i, e := range l {
				out[i] = c.filter(e)
			}
			return k, out
		case []any:
			out := make([]any, len(l))
			for i, e := range l {
				out[i] = c.filter(e)
			}
			return k, out
		case []bson.M:
			out := make([]any, len(l))
			for i, e := range l {
				out[i] = c.filter(e)
			}
			return k, out
		case []bson.D:
			out := make([]any, len(l))
			for i, e := range l {
				out[i] = c.filter(e)
			}
			return k, out
		}
		return k, v
	}
	if k == "id" && c.t.IDIsPK {
		k = "_id"
	}
	if col := c.t.colOf(k); col != nil && col.Type == TDate && !col.Array {
		v = dateFilterValue(v)
	}
	return k, v
}

// dateFilterValue turns time.Time values (also inside $eq/$gt/…/$in) into YYYY-MM-DD, as Postgres compared them.
func dateFilterValue(v any) any {
	switch x := v.(type) {
	case time.Time:
		return x.Format("2006-01-02")
	case *time.Time:
		if x != nil {
			return x.Format("2006-01-02")
		}
	case bson.M:
		out := make(bson.M, len(x))
		for k, e := range x {
			out[k] = dateFilterValue(e)
		}
		return out
	case bson.D:
		out := make(bson.D, len(x))
		for i, e := range x {
			out[i] = bson.E{Key: e.Key, Value: dateFilterValue(e.Value)}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = dateFilterValue(e)
		}
		return out
	case bson.A:
		out := make(bson.A, len(x))
		for i, e := range x {
			out[i] = dateFilterValue(e)
		}
		return out
	case []time.Time:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = e.Format("2006-01-02")
		}
		return out
	}
	return v
}

// keys translates `id` → `_id` in a sort or projection document.
func (c Coll) keys(v any) any {
	if v == nil || c.t == nil || !c.t.IDIsPK {
		return v
	}
	switch x := v.(type) {
	case bson.D:
		out := make(bson.D, len(x))
		for i, e := range x {
			if e.Key == "id" {
				e.Key = "_id"
			}
			out[i] = e
		}
		return out
	case bson.M:
		out := make(bson.M, len(x))
		for k, e := range x {
			if k == "id" {
				k = "_id"
			}
			out[k] = e
		}
		return out
	case map[string]any:
		out := make(bson.M, len(x))
		for k, e := range x {
			if k == "id" {
				k = "_id"
			}
			out[k] = e
		}
		return out
	}
	return v
}

func (c Coll) findOpts(opts []options.Lister[options.FindOptions]) *lister[options.FindOptions] {
	return adjust(opts, func(o *options.FindOptions) {
		o.Sort, o.Projection = c.keys(o.Sort), c.keys(o.Projection)
	})
}

func (c Coll) findOneOpts(opts []options.Lister[options.FindOneOptions]) *lister[options.FindOneOptions] {
	return adjust(opts, func(o *options.FindOneOptions) {
		o.Sort, o.Projection = c.keys(o.Sort), c.keys(o.Projection)
	})
}

// ---------- reads ----------

// findRows returns every matching document as a Row (an empty, non-nil slice when none match).
func (c Coll) findRows(ctx context.Context, filter any, opts ...options.Lister[options.FindOptions]) ([]Row, error) {
	ctx = c.bind(ctx)
	cur, err := c.mc.Find(ctx, c.filter(filter), c.findOpts(opts))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []Row{}
	for cur.Next(ctx) {
		out = append(out, rowOf(c.t, cur.Current))
	}
	return out, cur.Err()
}

// findOneRow returns the first matching document, or nil, nil when none matches.
func (c Coll) findOneRow(ctx context.Context, filter any, opts ...options.Lister[options.FindOneOptions]) (Row, error) {
	raw, err := c.mc.FindOne(c.bind(ctx), c.filter(filter), c.findOneOpts(opts)).Raw()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rowOf(c.t, raw), nil
}

// findInto decodes every matching document into *[]T with the driver's decoder (bson tags; `_id` stays `_id`).
// A *[]Row (or *[]map[string]any) gets Rows as Find returns them.
func (c Coll) findInto(ctx context.Context, filter, outSlicePtr any, opts ...options.Lister[options.FindOptions]) error {
	switch p := outSlicePtr.(type) {
	case *[]Row:
		rows, err := c.Find(ctx, filter, opts...)
		if err == nil {
			*p = rows
		}
		return err
	}
	ctx = c.bind(ctx)
	cur, err := c.mc.Find(ctx, c.filter(filter), c.findOpts(opts))
	if err != nil {
		return err
	}
	return cur.All(ctx, outSlicePtr)
}

// findOneInto decodes the first matching document into *T (a *Row gets a Row); ErrNoRows when none matches.
func (c Coll) findOneInto(ctx context.Context, filter, outPtr any, opts ...options.Lister[options.FindOneOptions]) error {
	if p, ok := outPtr.(*Row); ok {
		r, err := c.FindOne(ctx, filter, opts...)
		if err != nil {
			return err
		}
		if r == nil {
			return ErrNoRows
		}
		*p = r
		return nil
	}
	return c.mc.FindOne(c.bind(ctx), c.filter(filter), c.findOneOpts(opts)).Decode(outPtr)
}

// aggregateRows runs a pipeline and returns Rows: top-level `_id` → `id` (unless the result also has an `id`;
// ObjectIDs dropped), and the registry typing of this collection's columns where a result field has a column's
// name and a compatible value. Write `_id` inside the pipeline; documents joined by $lookup keep their `_id`.
func (c Coll) aggregateRows(ctx context.Context, pipeline any) ([]Row, error) {
	ctx = c.bind(ctx)
	cur, err := c.mc.Aggregate(ctx, pipelineOf(pipeline))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []Row{}
	for cur.Next(ctx) {
		out = append(out, rowOf(c.t, cur.Current))
	}
	return out, cur.Err()
}

// aggregateInto decodes a pipeline's results into *[]T with the driver's decoder (*[]Row gets Aggregate's Rows).
func (c Coll) aggregateInto(ctx context.Context, pipeline, outSlicePtr any) error {
	if p, ok := outSlicePtr.(*[]Row); ok {
		rows, err := c.Aggregate(ctx, pipeline)
		if err == nil {
			*p = rows
		}
		return err
	}
	ctx = c.bind(ctx)
	cur, err := c.mc.Aggregate(ctx, pipelineOf(pipeline))
	if err != nil {
		return err
	}
	return cur.All(ctx, outSlicePtr)
}

func pipelineOf(p any) any {
	switch x := p.(type) {
	case nil:
		return mongo.Pipeline{}
	case []bson.M:
		out := make(bson.A, len(x))
		for i, e := range x {
			out[i] = e
		}
		return out
	}
	return p
}

// count is count(*) of the matching documents.
func (c Coll) count(ctx context.Context, filter any) (int64, error) {
	return c.mc.CountDocuments(c.bind(ctx), c.filter(filter))
}

// exists reports whether a document matches.
func (c Coll) exists(ctx context.Context, filter any) (bool, error) {
	err := c.mc.FindOne(c.bind(ctx), c.filter(filter), options.FindOne().SetProjection(bson.D{{Key: "_id", Value: 1}})).Err()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return false, nil
	}
	return err == nil, err
}

// distinct returns the distinct values of a field (typed like Row values) among the matching documents, sorted
// (by Mongo's order: numbers, then strings in byte order, …).
func (c Coll) distinct(ctx context.Context, field string, filter any) ([]any, error) {
	if field == "id" && c.t != nil && c.t.IDIsPK {
		field = "_id"
	}
	res := c.mc.Distinct(c.bind(ctx), field, c.filter(filter))
	if err := res.Err(); err != nil {
		return nil, err
	}
	arr, err := res.Raw()
	if err != nil {
		return nil, err
	}
	vals, err := arr.Values()
	if err != nil {
		return nil, err
	}
	col := c.t.colOf(field)
	out := make([]any, 0, len(vals))
	for _, v := range vals {
		g := rawToGo(v)
		if col != nil && !col.Array {
			g = readScalar(col, g)
		}
		out = append(out, g)
	}
	return out, nil
}

// ---------- documents given to writes ----------

// pairsOf turns a document (bson.D, bson.M, map[string]any, Row, or a struct) into ordered pairs.
func pairsOf(doc any) (bson.D, error) {
	switch x := doc.(type) {
	case nil:
		return bson.D{}, nil
	case bson.D:
		return x, nil
	case bson.M:
		return sortedPairs(x), nil
	case map[string]any:
		return sortedPairs(x), nil
	case bson.Raw:
		var d bson.D
		err := bson.Unmarshal(x, &d)
		return d, err
	}
	rv := reflect.ValueOf(doc)
	if rv.Kind() == reflect.Map && rv.Type().Key().Kind() == reflect.String {
		m := map[string]any{}
		for _, k := range rv.MapKeys() {
			m[k.String()] = rv.MapIndex(k).Interface()
		}
		return sortedPairs(m), nil
	}
	b, err := bson.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("store: cannot use %T as a document: %w", doc, err)
	}
	var d bson.D
	err = bson.Unmarshal(b, &d)
	return d, err
}

func sortedPairs(m map[string]any) bson.D {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	d := make(bson.D, len(keys))
	for i, k := range keys {
		d[i] = bson.E{Key: k, Value: m[k]}
	}
	return d
}

func lookup(d bson.D, k string) (any, bool) {
	for _, e := range d {
		if e.Key == k {
			return e.Value, true
		}
	}
	return nil, false
}

func isPipeline(u any) bool {
	switch u.(type) {
	case bson.A, []any, []bson.D, []bson.M, mongo.Pipeline:
		return true
	}
	return false
}

func topField(k string) string {
	if i := strings.IndexByte(k, '.'); i >= 0 {
		return k[:i]
	}
	return k
}
