package realtime

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// The portal's employee list (16 Nov 2026), for matching portal codes to HRMS employees (api/realtime_match.go).
// EmployeeList.aspx shows the list with its filter on "Active" (the page's default; the other choices are All and
// Resigned Staff) and seven buttons: Add, Delete, Upload Name in Device, Import from Excel, Export Excel, Assign
// Batch Shift and Close. The client presses only "Export Excel", found by its caption, which answers with a CSV
// file (TotalEmployee.csv): EmpName, FatherName, EmpCode, CardNo, ProximityCardNo, EmailID, PhoneNo, Department,
// Designation, BranchName, OfficeTimePolicy, DateOfBirth, DateOfJoin (dd/mm/yyyy), ShiftStartDate, ShiftCode,
// WeeklyOff, CompanyName. The page also shows each employee's device password; it is never read or kept.

// Employee is one row of the portal's employee list.
type Employee struct {
	Name   string `json:"name"`
	Father string `json:"father,omitempty"`
	Code   string `json:"code"` // EmpCode (e.g. 1050)
	Card   string `json:"card"` // CardNo (e.g. 00001050)
	Email  string `json:"email,omitempty"`
	Phone  string `json:"phone,omitempty"` // the last 10 digits
	Dept   string `json:"dept,omitempty"`
	Desig  string `json:"desig,omitempty"`
	Branch string `json:"branch,omitempty"`
	DOB    string `json:"dob,omitempty"` // yyyy-mm-dd
	DOJ    string `json:"doj,omitempty"` // yyyy-mm-dd
}

// exportCaption is the only button of EmployeeList.aspx the client presses.
const exportCaption = "Export Excel"

// EmployeeList signs in and returns the portal's list of active employees as the exported CSV file.
func (c *Client) EmployeeList(ctx context.Context) ([]byte, error) {
	if err := c.SignIn(ctx); err != nil {
		return nil, err
	}
	res, b, err := c.do(ctx, http.MethodGet, "/EmployeeList.aspx", nil, nil)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK || !strings.EqualFold(res.Request.URL.Path, "/employeelist.aspx") {
		return nil, errors.New("the employee list page did not open (signed out?)")
	}
	f, err := exportForm(string(b))
	if err != nil {
		return nil, err
	}
	res, b, err = c.do(ctx, http.MethodPost, "/EmployeeList.aspx", f, nil)
	if err != nil {
		return nil, err
	}
	head := strings.ToLower(string(bytes.TrimLeft(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), " \t")))
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(head, "empname") {
		return nil, fmt.Errorf("the employee list export failed (%d, %s)", res.StatusCode, res.Header.Get("Content-Type"))
	}
	return b, nil
}

// exportForm is the employee list form as a browser sends it when "Export Excel" is pressed: the hidden fields,
// the checked filter (Active) and that one button. The device list box is left unselected, as on the page.
func exportForm(page string) (url.Values, error) {
	if i := strings.Index(page, "<form"); i >= 0 {
		page = page[i:]
	}
	f := url.Values{}
	button := ""
	for _, t := range reInput.FindAllString(page, -1) {
		n, ok := attr(reName, t)
		if !ok {
			continue
		}
		ty, _ := attr(reType, t)
		ty = strings.ToLower(ty)
		v, hasV := attr(reValue, t)
		switch ty {
		case "submit", "button", "image":
			if strings.TrimSpace(v) == exportCaption {
				button = n
			}
			continue
		case "file", "reset":
			continue
		case "checkbox", "radio":
			if !reChecked.MatchString(t) {
				continue
			}
			if !hasV && ty == "checkbox" {
				v = "on"
			}
		}
		f.Add(n, v)
	}
	if button == "" {
		return nil, errors.New("the employee list page has no Export Excel button")
	}
	f.Set(button, exportCaption)
	return f, nil
}

var reDigits = regexp.MustCompile(`\D`)

// ParseEmployees reads the exported employee list. Columns are found by their headings; a row without EmpCode or
// CardNo is skipped.
func ParseEmployees(b []byte) ([]Employee, error) {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	r := csv.NewReader(bytes.NewReader(b))
	r.LazyQuotes, r.FieldsPerRecord, r.TrimLeadingSpace = true, -1, true
	head, err := r.Read()
	if err != nil {
		return nil, errors.New("the employee list is empty")
	}
	col := map[string]int{}
	for i, h := range head {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, need := range []string{"empname", "empcode", "cardno"} {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("the employee list has no %s column", need)
		}
	}
	var out []Employee
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("the employee list could not be read: %w", err)
		}
		get := func(name string) string {
			if i, ok := col[name]; ok && i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		e := Employee{Name: get("empname"), Father: get("fathername"), Code: get("empcode"), Card: get("cardno"), Email: get("emailid"),
			Phone: phone10(get("phoneno")), Dept: get("department"), Desig: get("designation"), Branch: get("branchname"),
			DOB: ddmmyyyy(get("dateofbirth")), DOJ: ddmmyyyy(get("dateofjoin"))}
		if e.Code == "" && e.Card == "" {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// phone10 is the last 10 digits of a phone number ("" when it has fewer).
func phone10(s string) string {
	d := reDigits.ReplaceAllString(s, "")
	if len(d) < 10 {
		return ""
	}
	return d[len(d)-10:]
}

// ddmmyyyy turns the portal's 13/11/2026 into 2026-11-13 ("" when it is not a date, or a placeholder before 1920).
func ddmmyyyy(s string) string {
	if i := strings.IndexByte(s, ' '); i > 0 {
		s = s[:i]
	}
	t, err := time.Parse("2/1/2006", s)
	if err != nil || t.Year() < 1920 {
		return ""
	}
	return t.Format("2006-01-02")
}
