package store

// Registry additions for uniform sizes (owner's request of 13 Nov 2026: record each employee's uniform sizes, issue
// uniform by size, and refund the uniform recovery as the company's policy says):
//
//   - uniform_size: one document per employee with the sizes on record: shirt (shirt or T-shirt), waist and length
//     (trousers, in inches) and shoe (UK size). All optional; who changed them last and when.
//   - inv_item.size_type: which size a uniform item is issued by: shirt, trouser, shoes or other. Null reads as
//     "other" until HR Admin tags the item in Inventory › Setup.
//   - uniform_issue.refund_rule: the refund rule the issue was scheduled under, in words (null on issues made
//     before it existed; their refund line in the ledger is what counts).
//   - company_setting.uniform (jsonb, no registry change): refundBasis ("deduction": N months after the last
//     deduction, the rule so far and the default; "service": after N months of employment), serviceMonths, and
//     sizes, the option lists HR Admin keeps.
//   - onb_field: four Form setup fields in a new section, Uniform (uniShirt, uniWaist, uniLength, uniShoe), asked
//     at joining as optional, filled by the joiner.
//
// uniform_size never existed in PostgreSQL, so it is kept out of schemaTables like doc_window. Migration 008 creates
// it, adds the four Form setup fields when missing and tags the uniform items whose name says what they are
// (shirt, T-shirt, trouser, pant, shoe). It never touches a uniform issue or a pay line.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// UniformSizeColl holds each employee's uniform sizes.
const UniformSizeColl = "uniform_size"

// Size types of a uniform item.
const (
	SizeShirt   = "shirt"
	SizeTrouser = "trouser"
	SizeShoes   = "shoes"
	SizeOther   = "other"
)

// SizeTypes are the size types in their display order.
var SizeTypes = []string{SizeShirt, SizeTrouser, SizeShoes, SizeOther}

// DefaultUniformSizes are the size lists while HR Admin has not changed them: shirt sizes, trouser waist (even
// inches 26 to 44), trouser length (28 to 36 inches) and shoe sizes (UK 4 to 12).
func DefaultUniformSizes() map[string][]string {
	waist, length, shoe := []string{}, []string{}, []string{}
	for n := 26; n <= 44; n += 2 {
		waist = append(waist, fmt.Sprint(n))
	}
	for n := 28; n <= 36; n++ {
		length = append(length, fmt.Sprint(n))
	}
	for n := 4; n <= 12; n++ {
		shoe = append(shoe, fmt.Sprint(n))
	}
	return map[string][]string{"shirt": {"XS", "S", "M", "L", "XL", "XXL", "3XL", "4XL"}, "waist": waist, "length": length, "shoe": shoe}
}

// UniformField is one Form setup field of the Uniform section: its key, label and the size list it offers.
type UniformField struct{ Key, Label, List string }

// UniformFields are the Form setup fields of the Uniform section, in order.
var UniformFields = []UniformField{
	{"uniShirt", "Shirt / T-shirt size", "shirt"},
	{"uniWaist", "Trouser waist (inches)", "waist"},
	{"uniLength", "Trouser length (inches)", "length"},
	{"uniShoe", "Shoe size (UK)", "shoe"},
}

// UniformSection is the Form setup section of the uniform sizes.
const UniformSection = "Uniform"

var uniformSizeTable = &Table{
	Name: UniformSizeColl,
	Columns: []Column{
		{Name: "employee_id", Type: TText, NotNull: true},
		{Name: "shirt", Type: TText},
		{Name: "waist", Type: TText},
		{Name: "length", Type: TText},
		{Name: "shoe", Type: TText},
		{Name: "updated_at", Type: TTimestamptz},
		{Name: "updated_by", Type: TText},
	},
	PK: []string{"employee_id"},
	Indexes: []Index{
		{Name: "uniform_size_employee_key", Keys: []IndexKey{{Field: "employee_id"}}, Unique: true},
	},
	FKs: []FK{
		{Name: "uniform_size_employee_id_fkey", Col: "employee_id", RefTable: "employee", RefCol: "id"},
	},
}

var _ = registerUniform()

func registerUniform() bool {
	for _, t := range schemaTables {
		if t.cols != nil {
			continue
		}
		switch t.Name {
		case "inv_item":
			t.Columns = append(t.Columns, Column{Name: "size_type", Type: TText})
			t.Checks = append(t.Checks, Check{Name: "inv_item_size_type_check",
				Conds: []Cond{{Col: "size_type", Op: "in", Val: []any{SizeShirt, SizeTrouser, SizeShoes, SizeOther}}}})
		case "uniform_issue":
			t.Columns = append(t.Columns, Column{Name: "refund_rule", Type: TText})
		}
	}
	joinerSections[UniformSection] = true
	t := uniformSizeTable
	t.cols = map[string]*Column{}
	for i := range t.Columns {
		t.cols[t.Columns[i].Name] = &t.Columns[i]
	}
	tableByName[t.Name] = t
	if emp := tableByName["employee"]; emp != nil {
		emp.refBy = append(emp.refBy, refFrom{t, t.FKs[0]})
	}
	return true
}

// ensureUniformSize creates uniform_size and its index when missing. It never touches a document.
func ensureUniformSize(ctx context.Context, db *mongo.Database) error {
	names, err := db.ListCollectionNames(ctx, bson.D{{Key: "name", Value: UniformSizeColl}})
	if err != nil {
		return err
	}
	if len(names) == 0 {
		if err := db.CreateCollection(ctx, UniformSizeColl); err != nil {
			var ce mongo.CommandError
			if !(errors.As(err, &ce) && ce.Code == 48) { // NamespaceExists
				return fmt.Errorf("create %s: %w", UniformSizeColl, err)
			}
		}
	}
	return mdmCreateIndexes(ctx, db, UniformSizeColl, uniformSizeTable.Indexes)
}

// UniformSizeLists reads the size lists HR Admin keeps (company_setting.uniform.sizes), each list falling back to
// its default when it is not set.
func UniformSizeLists(ctx context.Context, q Q) map[string][]string {
	out := DefaultUniformSizes()
	r, err := q.C("company_setting").FindOne(ctx, bson.M{"_id": 1})
	if err != nil || r == nil {
		return out
	}
	u, _ := r["uniform"].(map[string]any)
	sz, _ := u["sizes"].(map[string]any)
	for k := range out {
		l, ok := sz[k].([]any)
		if !ok || len(l) == 0 {
			continue
		}
		vals := []string{}
		for _, v := range l {
			if s, ok := v.(string); ok && s != "" {
				vals = append(vals, s)
			}
		}
		if len(vals) > 0 {
			out[k] = vals
		}
	}
	return out
}

// AddUniformFields adds the Uniform section's Form setup fields that are missing (after the last field, asked at
// joining as optional, filled by the joiner, for every employment type). It returns how many it added; a field
// already there is left as HR set it.
func AddUniformFields(ctx context.Context, q Q) (int, error) {
	lists := UniformSizeLists(ctx, q)
	n := 0
	for _, f := range UniformFields {
		if ok, err := q.C("onb_field").Exists(ctx, bson.M{"key": f.Key}); err != nil || ok {
			if err != nil {
				return n, err
			}
			continue
		}
		sort := int32(1)
		rows, err := q.C("onb_field").Find(ctx, bson.M{})
		if err != nil {
			return n, err
		}
		for _, r := range rows {
			if v := toInt32(r["sort"]); v >= sort {
				sort = v + 1
			}
		}
		if _, err := q.C("onb_field").Insert(ctx, bson.M{"key": f.Key, "label": f.Label, "section": UniformSection, "kind": "sel",
			"options": strings.Join(lists[f.List], ","), "on_form": true, "sys": false, "custom": false, "offer_state": int32(0), "join_state": int32(1),
			"types": []string{"Permanent", "Retainer", "Contractual"}, "sort": sort, "filled_by": FilledByJoiner}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func toInt32(v any) int32 {
	switch x := v.(type) {
	case int32:
		return x
	case int64:
		return int32(x)
	case int16:
		return int32(x)
	case int:
		return int32(x)
	case float64:
		return int32(x)
	}
	return 0
}

var (
	reShirtItem   = regexp.MustCompile(`(?i)\b(shirt|t-shirt|tshirt|tee|polo|kurta|tunic|jacket|blazer|coat)s?\b`)
	reTrouserItem = regexp.MustCompile(`(?i)\b(trousers?|pants?|jeans|chinos?)\b`)
	reShoeItem    = regexp.MustCompile(`(?i)\b(shoes?|boots?|footwear)\b`)
)

// GuessSizeType is the size type an item's name suggests ("" when it says nothing).
func GuessSizeType(name string) string {
	switch {
	case reShoeItem.MatchString(name):
		return SizeShoes
	case reTrouserItem.MatchString(name):
		return SizeTrouser
	case reShirtItem.MatchString(name):
		return SizeShirt
	}
	return ""
}

// TagUniformItems sets size_type on uniform items that have none and whose name says what they are. Items already
// tagged, and names that say nothing, are left alone. It returns how many it tagged.
func TagUniformItems(ctx context.Context, q Q) (int, error) {
	rows, err := q.C("inv_item").Find(ctx, bson.M{"kind": "uniform", "size_type": nil})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		name, _ := r["name"].(string)
		if t := GuessSizeType(name); t != "" {
			if _, err := q.C("inv_item").Update(ctx, bson.M{"_id": r["id"], "size_type": nil}, bson.M{"$set": bson.M{"size_type": t}}); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// migrateUniform is migration 008: the uniform_size collection, the Uniform section of Form setup and the size
// type of the uniform items whose name says it. Existing uniform issues and pay lines are not touched.
func migrateUniform(ctx context.Context, s *Store) error {
	if err := ensureUniformSize(ctx, s.DB.db); err != nil {
		return err
	}
	return s.Tx(ctx, func(tx Q) error {
		if _, err := AddUniformFields(ctx, tx); err != nil {
			return err
		}
		_, err := TagUniformItems(ctx, tx)
		return err
	})
}

// healUniform keeps uniform_size, the Uniform Form setup fields and the size types of uniform items that never had
// one in place on every start (a database restored from before them, or an import from PostgreSQL). Cheap when they
// are there; a field or size type HR set is left alone.
func healUniform(ctx context.Context, s *Store, log func(string)) error {
	if err := ensureUniformSize(ctx, s.DB.db); err != nil {
		return err
	}
	return s.Tx(ctx, func(tx Q) error {
		n, err := AddUniformFields(ctx, tx)
		if err == nil && n > 0 {
			log(fmt.Sprintf("added %d uniform size field(s) to Form setup", n))
		}
		if err != nil {
			return err
		}
		k, err := TagUniformItems(ctx, tx)
		if err == nil && k > 0 {
			log(fmt.Sprintf("set the size type of %d uniform item(s) from their names", k))
		}
		return err
	})
}
