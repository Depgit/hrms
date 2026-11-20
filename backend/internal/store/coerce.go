package store

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// ---------- reads: BSON → Row values ----------

// rawToGo converts a BSON value: documents → map[string]any, arrays → []any, dates → time.Time in the business
// zone, int32/int64/double as they are, ObjectIDs → hex strings, the hidden _lock field dropped.
func rawToGo(v bson.RawValue) any {
	switch v.Type {
	case bson.TypeDouble:
		return v.Double()
	case bson.TypeString:
		return v.StringValue()
	case bson.TypeEmbeddedDocument:
		return rawDocToMap(v.Document())
	case bson.TypeArray:
		vals, _ := v.Array().Values()
		out := make([]any, len(vals))
		for i, e := range vals {
			out[i] = rawToGo(e)
		}
		return out
	case bson.TypeBinary:
		_, b := v.Binary()
		return append([]byte(nil), b...)
	case bson.TypeObjectID:
		return v.ObjectID().Hex()
	case bson.TypeBoolean:
		return v.Boolean()
	case bson.TypeDateTime:
		return time.UnixMilli(v.DateTime()).In(Loc())
	case bson.TypeNull, bson.TypeUndefined:
		return nil
	case bson.TypeInt32:
		return v.Int32()
	case bson.TypeInt64:
		return v.Int64()
	case bson.TypeDecimal128:
		f, _ := strconv.ParseFloat(v.Decimal128().String(), 64)
		return f
	case bson.TypeTimestamp:
		t, _ := v.Timestamp()
		return time.Unix(int64(t), 0).In(Loc())
	case bson.TypeRegex:
		p, _ := v.Regex()
		return p
	}
	return v.String()
}

func rawDocToMap(d bson.Raw) map[string]any {
	els, _ := d.Elements()
	m := make(map[string]any, len(els))
	for _, el := range els {
		if el.Key() == lockField {
			continue
		}
		m[el.Key()] = rawToGo(el.Value())
	}
	return m
}

const lockField = "_lock"

// rowOf turns a stored document into a Row: `_id` → `id` (dropped when it is an ObjectID), registry typing for the
// collection's columns, _lock dropped.
func rowOf(t *Table, d bson.Raw) Row {
	els, _ := d.Elements()
	r := make(Row, len(els))
	var idv any
	hasID := false
	for _, el := range els {
		k := el.Key()
		if k == lockField {
			continue
		}
		v := el.Value()
		if k == "_id" {
			if v.Type == bson.TypeObjectID {
				continue
			}
			idv, hasID = rawToGo(v), true
			continue
		}
		g := rawToGo(v)
		if c := t.Col(k); c != nil {
			g = readTyped(c, g)
		}
		r[k] = g
	}
	if hasID {
		if _, clash := r["id"]; clash {
			r["_id"] = idv
		} else {
			if c := t.Col("id"); c != nil {
				idv = readTyped(c, idv)
			}
			r["id"] = idv
		}
	}
	return r
}

// readTyped gives a stored value the Go type a pgx read of the column gave.
func readTyped(c *Column, v any) any {
	if v == nil {
		return nil
	}
	if c.Array {
		l, ok := v.([]any)
		if !ok {
			return v
		}
		out := make([]any, len(l))
		for i, e := range l {
			out[i] = readScalar(c, e)
		}
		return out
	}
	return readScalar(c, v)
}

func readScalar(c *Column, v any) any {
	if v == nil {
		return nil
	}
	switch c.Type {
	case TInt2:
		if f, ok := asFloat(v); ok {
			return int16(f)
		}
	case TInt4:
		if f, ok := asFloat(v); ok {
			return int32(f)
		}
	case TInt8:
		switch x := v.(type) {
		case int64:
			return x
		case int32:
			return int64(x)
		case float64:
			return int64(x)
		}
	case TNumeric:
		if f, ok := asFloat(v); ok {
			if c.Prec > 0 {
				return roundScale(f, c.Scale)
			}
			return f
		}
	case TDate:
		if t, ok := v.(time.Time); ok {
			return DateOf(t)
		}
	case TTime:
		if s, ok := v.(string); ok && len(s) > 5 && s[2] == ':' {
			return s[:5]
		}
	case TJSONB:
		return jsonish(v)
	}
	return v
}

// jsonish makes an embedded value look as if it came from JSON: numbers float64, dates RFC 3339 strings.
func jsonish(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = jsonish(e)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = jsonish(e)
		}
		return x
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	case time.Time:
		return x.Format(time.RFC3339Nano)
	}
	return v
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	case int16:
		return float64(x), true
	case float32:
		return float64(x), true
	}
	return 0, false
}

// ---------- writes: Go values → stored values ----------

// coerce converts a value written to column c into its stored form, or returns a store Error with the code
// PostgreSQL gave for the same input.
func coerce(t *Table, c *Column, v any) (any, error) {
	v = deref(v)
	if v == nil {
		return nil, nil
	}
	if c.Type == TJSONB {
		return toJSONValue(v)
	}
	if c.Array {
		if l, ok := v.(bson.A); ok {
			v = []any(l)
		}
		rv := reflect.ValueOf(v)
		if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
			return nil, errf("22P02", "", "malformed array literal for column %s.%s: %v", t.Name, c.Name, v)
		}
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return nil, nil // a nil slice was NULL for pgx too
		}
		out := make([]any, rv.Len())
		for i := range out {
			e, err := coerceScalar(t, c, rv.Index(i).Interface())
			if err != nil {
				return nil, err
			}
			out[i] = e
		}
		return out, nil
	}
	return coerceScalar(t, c, v)
}

func deref(v any) any {
	if v == nil {
		return nil
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	return rv.Interface()
}

func badValue(t *Table, c *Column, v any) error {
	return errf("22P02", "", "invalid input syntax for type %s: %q (column %s.%s)", c.sqlType(), fmt.Sprint(v), t.Name, c.Name)
}

func coerceScalar(t *Table, c *Column, v any) (any, error) {
	v = deref(v)
	if v == nil {
		return nil, nil
	}
	switch c.Type {
	case TText, TChar:
		var s string
		switch x := v.(type) {
		case string:
			s = x
		case []byte:
			s = string(x)
		case fmt.Stringer:
			s = x.String()
		case bool:
			s = strconv.FormatBool(x)
		default:
			if f, ok := asNumber(v); ok {
				s = f
			} else {
				return nil, badValue(t, c, v)
			}
		}
		if c.Type == TChar && c.Len > 0 {
			s = strings.TrimRight(s, " ")
			if n := len([]rune(s)); n > c.Len {
				return nil, errf("22001", "", "value too long for type character(%d) (column %s.%s)", c.Len, t.Name, c.Name)
			} else if n < c.Len {
				s += strings.Repeat(" ", c.Len-n)
			}
		}
		return s, nil
	case TUUID:
		switch x := v.(type) {
		case string:
			if u, ok := parseUUID(x); ok {
				return u, nil
			}
		case [16]byte:
			return formatUUID(x), nil
		case fmt.Stringer:
			if u, ok := parseUUID(x.String()); ok {
				return u, nil
			}
		}
		return nil, badValue(t, c, v)
	case TInt2, TInt4, TInt8:
		n, ok := toInt(v)
		if !ok {
			return nil, badValue(t, c, v)
		}
		switch c.Type {
		case TInt2:
			if n < math.MinInt16 || n > math.MaxInt16 {
				return nil, errf("22003", "", "smallint out of range (column %s.%s)", t.Name, c.Name)
			}
			return int32(n), nil // stored as int32, read back as int16
		case TInt4:
			if n < math.MinInt32 || n > math.MaxInt32 {
				return nil, errf("22003", "", "integer out of range (column %s.%s)", t.Name, c.Name)
			}
			return int32(n), nil
		}
		return n, nil
	case TNumeric:
		f, ok := toFloat(v)
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, badValue(t, c, v)
		}
		if c.Prec > 0 {
			f = roundScale(f, c.Scale)
			if math.Abs(f) >= math.Pow10(c.Prec-c.Scale) {
				return nil, errf("22003", fmt.Sprintf("A field with precision %d, scale %d must round to an absolute value less than 10^%d.", c.Prec, c.Scale, c.Prec-c.Scale),
					"numeric field overflow (column %s.%s)", t.Name, c.Name)
			}
		}
		return f, nil
	case TBool:
		switch x := v.(type) {
		case bool:
			return x, nil
		case string:
			switch strings.ToLower(strings.TrimSpace(x)) {
			case "t", "true", "y", "yes", "on", "1":
				return true, nil
			case "f", "false", "n", "no", "off", "0":
				return false, nil
			}
		}
		return nil, badValue(t, c, v)
	case TDate:
		switch x := v.(type) {
		case string:
			if d, ok := parseDate(x); ok {
				return d, nil
			}
			return nil, errf("22007", "", "invalid input syntax for type date: %q (column %s.%s)", x, t.Name, c.Name)
		case time.Time:
			return x.Format("2006-01-02"), nil // pgx used the time's own calendar date
		case bson.DateTime:
			return DateOf(x.Time()), nil
		}
		return nil, badValue(t, c, v)
	case TTime:
		switch x := v.(type) {
		case string:
			if s, ok := parseClock(x); ok {
				return s, nil
			}
			return nil, errf("22007", "", "invalid input syntax for type time: %q (column %s.%s)", x, t.Name, c.Name)
		case time.Time:
			return x.Format("15:04"), nil
		}
		return nil, badValue(t, c, v)
	case TTimestamptz:
		switch x := v.(type) {
		case time.Time:
			return x, nil
		case bson.DateTime:
			return x.Time(), nil
		case string:
			if tm, ok := parseTimestamp(x); ok {
				return tm, nil
			}
			return nil, errf("22007", "", "invalid input syntax for type timestamp with time zone: %q (column %s.%s)", x, t.Name, c.Name)
		}
		return nil, badValue(t, c, v)
	case TBytea:
		switch x := v.(type) {
		case []byte:
			return x, nil
		case string:
			return []byte(x), nil
		case bson.Binary:
			return x.Data, nil
		}
		return nil, badValue(t, c, v)
	}
	return v, nil
}

func asNumber(v any) (string, bool) {
	switch x := v.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return fmt.Sprint(x), true
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case json.Number:
		return x.String(), true
	}
	return "", false
}

func toInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int8:
		return int64(x), true
	case int16:
		return int64(x), true
	case int32:
		return int64(x), true
	case int64:
		return x, true
	case uint:
		return int64(x), true
	case uint8:
		return int64(x), true
	case uint16:
		return int64(x), true
	case uint32:
		return int64(x), true
	case uint64:
		if x > math.MaxInt64 {
			return 0, false
		}
		return int64(x), true
	case float32:
		return toInt(float64(x))
	case float64:
		if x != math.Trunc(x) || math.IsInf(x, 0) || math.IsNaN(x) || math.Abs(x) > 9.2e18 {
			return 0, false
		}
		return int64(x), true
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n, true
		}
		if f, err := x.Float64(); err == nil {
			return toInt(f)
		}
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		f, _ := strconv.ParseFloat(strconv.FormatFloat(float64(x), 'g', -1, 32), 64)
		return f, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	case bson.Decimal128:
		f, err := strconv.ParseFloat(x.String(), 64)
		return f, err == nil
	}
	if n, ok := toInt(v); ok {
		return float64(n), true
	}
	return 0, false
}

// roundScale rounds like numeric(p,s) did: to s decimals, half away from zero, on the shortest decimal form of f
// (what pgx sent), so 2.675 → 2.68.
func roundScale(f float64, scale int) float64 {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return f
	}
	neg := r.Sign() < 0
	if neg {
		r.Neg(r)
	}
	p := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	r.Mul(r, new(big.Rat).SetInt(p))
	r.Add(r, big.NewRat(1, 2))
	q := new(big.Int).Quo(r.Num(), r.Denom()) // floor for a positive value
	out, _ := new(big.Rat).SetFrac(q, p).Float64()
	if neg {
		out = -out
	}
	return out
}

var hexRe = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

// parseUUID accepts what Postgres's uuid input accepted (hyphens, braces, upper case) and returns the canonical form.
func parseUUID(s string) (string, bool) {
	x := strings.TrimSpace(s)
	if strings.HasPrefix(x, "{") && strings.HasSuffix(x, "}") {
		x = x[1 : len(x)-1]
	}
	x = strings.ReplaceAll(x, "-", "")
	if !hexRe.MatchString(x) {
		return "", false
	}
	var b [16]byte
	if _, err := hex.Decode(b[:], []byte(x)); err != nil {
		return "", false
	}
	return formatUUID(b), true
}

var dateRe = regexp.MustCompile(`^(\d{4})-(\d{1,2})-(\d{1,2})(?:$|[T ])`)

// parseDate accepts YYYY-MM-DD (also with a time after it, which the date type ignored) and checks the day exists.
func parseDate(s string) (string, bool) {
	s = strings.TrimSpace(s)
	m := dateRe.FindStringSubmatch(s)
	if m == nil {
		if len(s) == 8 && strings.Trim(s, "0123456789") == "" {
			m = []string{s, s[:4], s[4:6], s[6:]}
		} else {
			return "", false
		}
	}
	y, _ := strconv.Atoi(m[1])
	mo, _ := strconv.Atoi(m[2])
	d, _ := strconv.Atoi(m[3])
	t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
	if t.Year() != y || int(t.Month()) != mo || t.Day() != d || y < 1 {
		return "", false
	}
	return t.Format("2006-01-02"), true
}

var clockRe = regexp.MustCompile(`^(\d{1,2}):(\d{2})(?::(\d{2})(?:\.\d+)?)?$`)

// parseClock accepts HH:MM[:SS[.ffffff]] and returns HH:MM (what reads of a time column always gave).
func parseClock(s string) (string, bool) {
	m := clockRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", false
	}
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	sec := 0
	if m[3] != "" {
		sec, _ = strconv.Atoi(m[3])
	}
	if h > 24 || mi > 59 || sec > 60 || (h == 24 && (mi > 0 || sec > 0)) {
		return "", false
	}
	return fmt.Sprintf("%02d:%02d", h, mi), true
}

var tsLayouts = []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999Z07", "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999Z07"}
var tsLocal = []string{"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"}

// parseTimestamp accepts ISO timestamps; without a zone they are in the business zone (the session zone).
func parseTimestamp(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, l := range tsLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	for _, l := range tsLocal {
		if t, err := time.ParseInLocation(l, s, Loc()); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// toJSONValue stores a jsonb value as JSON had it: strings and []byte are JSON text, anything else is encoded with
// encoding/json (json tags, as pgx did) and decoded again, so numbers become doubles and structs plain documents.
func toJSONValue(v any) (any, error) {
	var raw []byte
	switch x := v.(type) {
	case string:
		raw = []byte(x)
	case []byte:
		raw = x
	case json.RawMessage:
		raw = x
	default:
		b, err := json.Marshal(plainJSON(v))
		if err != nil {
			return nil, errf("22P02", "", "invalid input syntax for type json: %v", err)
		}
		raw = b
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errf("22P02", "", "invalid input syntax for type json: %v", err)
	}
	return out, nil
}

// plainJSON turns BSON container types (bson.D keeps its pairs as a slice) into maps and slices json can encode.
func plainJSON(v any) any {
	switch x := v.(type) {
	case bson.D:
		m := make(map[string]any, len(x))
		for _, e := range x {
			m[e.Key] = plainJSON(e.Value)
		}
		return m
	case bson.M:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = plainJSON(e)
		}
		return m
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = plainJSON(e)
		}
		return m
	case bson.A:
		l := make([]any, len(x))
		for i, e := range x {
			l[i] = plainJSON(e)
		}
		return l
	case []any:
		l := make([]any, len(x))
		for i, e := range x {
			l[i] = plainJSON(e)
		}
		return l
	case bson.DateTime:
		return x.Time()
	}
	return v
}
