package realtime

import (
	"context"
	"errors"
	"html"
	"net/http"
	"regexp"
	"strings"
)

// The employee list's grid (EmployeeList.aspx, opened with a GET; 25 Nov 2026): the employee writes use it to find
// an employee's form (Employee.aspx?RowId=…, the grid links every row to it) by their EmpCode, and to check that a new
// code is not taken before a create. The grid has the columns EmpName, Cardno, EmpCode, Dept_Name, Desig_Name, pwd,
// Branch; the pwd column (the device password) is never read. The page lists the employees of its default filter
// (Active), all on one page.

// ListRow is one employee of the list's grid.
type ListRow struct {
	RowID  string `json:"rowId"`
	Name   string `json:"name"`
	Card   string `json:"card"`
	Code   string `json:"code"`
	Dept   string `json:"dept,omitempty"`
	Desig  string `json:"desig,omitempty"`
	Branch string `json:"branch,omitempty"`
}

var (
	reGridTable = regexp.MustCompile(`(?is)<table[^>]*\bid=['"]?grid2['"]?[^>]*>(.*?)</table>`)
	reGridTH    = regexp.MustCompile(`(?is)<th[^>]*>(.*?)</th>`)
	reGridTR    = regexp.MustCompile(`(?is)<tr[^>]*>(.*?)</tr>`)
	reGridTD    = regexp.MustCompile(`(?is)<td[^>]*>(.*?)</td>`)
	reGridRow   = regexp.MustCompile(`(?i)Employee\.aspx\?RowId=(\d+)`)
)

// ListRows reads the grid of the employee list page. The columns are found by their headers.
func ListRows(page string) []ListRow {
	m := reGridTable.FindStringSubmatch(page)
	if m == nil {
		return nil
	}
	col := map[string]int{}
	for i, h := range reGridTH.FindAllStringSubmatch(m[1], -1) {
		col[norm(textOf(h[1]))] = i
	}
	at := func(cells []string, name string) string {
		if i, ok := col[name]; ok && i < len(cells) {
			return textOf(cells[i])
		}
		return ""
	}
	out := []ListRow{}
	for _, tr := range reGridTR.FindAllStringSubmatch(m[1], -1) {
		id := reGridRow.FindStringSubmatch(tr[1])
		if id == nil {
			continue
		}
		var cells []string
		for _, td := range reGridTD.FindAllStringSubmatch(tr[1], -1) {
			cells = append(cells, td[1])
		}
		out = append(out, ListRow{RowID: id[1], Name: html.UnescapeString(at(cells, "empname")), Card: at(cells, "cardno"), Code: at(cells, "empcode"),
			Dept: at(cells, "deptname"), Desig: at(cells, "designame"), Branch: at(cells, "branch")})
	}
	return out
}

// FindCode is the row with the EmpCode (leading zeros ignored), or nil.
func FindCode(rows []ListRow, code string) *ListRow {
	n := rtNormCode(code)
	if n == "" {
		return nil
	}
	for i := range rows {
		if rtNormCode(rows[i].Code) == n {
			return &rows[i]
		}
	}
	return nil
}

// FindCard is the row with the card number (leading zeros ignored), or nil.
func FindCard(rows []ListRow, card string) *ListRow {
	n := rtNormCode(card)
	if n == "" {
		return nil
	}
	for i := range rows {
		if rtNormCode(rows[i].Card) == n {
			return &rows[i]
		}
	}
	return nil
}

// ActiveList signs in and reads the employee list (GET only): the portal's active employees.
func (c *Client) ActiveList(ctx context.Context) ([]ListRow, error) {
	if err := c.SignIn(ctx); err != nil {
		return nil, err
	}
	return c.listRows(ctx)
}

// listRows opens the employee list (GET; signed in) and reads its grid.
func (c *Client) listRows(ctx context.Context) ([]ListRow, error) {
	path := Spec.ListPath
	if path == "" {
		path = "/EmployeeList.aspx"
	}
	res, b, err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK || !strings.EqualFold(res.Request.URL.Path, path) {
		return nil, errors.New("the portal's employee list did not open (signed out?)")
	}
	rows := ListRows(string(b))
	if rows == nil {
		return nil, errors.New("the portal's employee list has no grid to read")
	}
	return rows, nil
}
