package realtime

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/extrame/xls"
)

// Row is one employee's line of the All Raw Punch Report: the portal's codes and name, the day, and the day's
// swipes in the order listed (HH:MM, India time).
type Row struct {
	EmpCode    string
	CardNo     string
	Name       string
	Department string
	Date       string   // yyyy-mm-dd
	Times      []string // HH:MM
}

// Swipes are the row's swipes as times in India. A time earlier than the one before it in the row is taken to be
// after midnight, on the next day.
func (r Row) Swipes(loc *time.Location) []time.Time {
	d, err := time.ParseInLocation("2006-01-02", r.Date, loc)
	if err != nil {
		return nil
	}
	var out []time.Time
	var prev time.Time
	day := d
	for _, s := range r.Times {
		t, err := time.Parse("15:04", s)
		if err != nil {
			continue
		}
		at := time.Date(day.Year(), day.Month(), day.Day(), t.Hour(), t.Minute(), 0, 0, loc)
		if !prev.IsZero() && at.Before(prev) {
			day = day.AddDate(0, 0, 1)
			at = at.AddDate(0, 0, 1)
		}
		out = append(out, at)
		prev = at
	}
	return out
}

var (
	reTime    = regexp.MustCompile(`^([01]?\d|2[0-3]):[0-5]\d$`)
	reRowDate = regexp.MustCompile(`(\d{2})-(\d{2})-(\d{4})`)
)

// ErrLayout is returned when the file is not the All Raw Punch Report.
var ErrLayout = errors.New("the file is not the portal's All Raw Punch Report (no Sr.No / EmpCode / Swipe Record header)")

// Parse reads the exported report (Excel 97–2003 .xls). A file the reader cannot handle is an error, never a panic.
func Parse(b []byte) (rows []Row, err error) {
	defer func() {
		if p := recover(); p != nil {
			rows, err = nil, fmt.Errorf("the Excel file could not be read (%v)", p)
		}
	}()
	return parse(b)
}

// sheetRow is row i, or nil when the sheet has no such row (the reader's Row does not check).
func sheetRow(sh *xls.WorkSheet, i int) (r *xls.Row) {
	defer func() {
		if recover() != nil {
			r = nil
		}
	}()
	return sh.Row(i)
}

func parse(b []byte) ([]Row, error) {
	wb, err := xls.OpenReader(bytes.NewReader(b), "utf-8")
	if err != nil {
		return nil, fmt.Errorf("the Excel file could not be read: %w", err)
	}
	sh := wb.GetSheet(0)
	if sh == nil {
		return nil, ErrLayout
	}
	col := map[string]int{}
	var out []Row
	for i := 0; i <= int(sh.MaxRow); i++ {
		r := sheetRow(sh, i)
		if r == nil {
			continue
		}
		cell := func(name string) string {
			j, ok := col[name]
			if !ok {
				return ""
			}
			return strings.TrimSpace(r.Col(j))
		}
		if len(col) == 0 { // looking for the header
			found := map[string]int{}
			for j := r.FirstCol(); j < r.LastCol(); j++ {
				k := strings.ToLower(strings.Join(strings.Fields(r.Col(j)), " "))
				switch k {
				case "sr.no", "department", "empcode", "cardno", "name", "date", "swipe record":
					found[k] = j
				}
			}
			if _, a := found["empcode"]; a {
				if _, b := found["swipe record"]; b {
					col = found
				}
			}
			continue
		}
		code, card := cell("empcode"), cell("cardno")
		if code == "" && card == "" {
			continue
		}
		if strings.EqualFold(code, "EmpCode") { // the header again on a later page
			continue
		}
		row := Row{EmpCode: code, CardNo: card, Name: cell("name"), Department: cell("department")}
		if m := reRowDate.FindStringSubmatch(cell("date")); m != nil {
			row.Date = m[3] + "-" + m[2] + "-" + m[1]
		}
		for _, t := range strings.Fields(cell("swipe record")) {
			if reTime.MatchString(t) {
				if len(t) == 4 {
					t = "0" + t
				}
				row.Times = append(row.Times, t)
			}
		}
		out = append(out, row)
	}
	if len(col) == 0 {
		return nil, ErrLayout
	}
	return out, nil
}
