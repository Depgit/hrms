package realtime

import (
	"context"
	"errors"
	"html"
	"net/http"
	"regexp"
	"strings"
)

// The portal's machine list (17 Nov 2026), for the punch-machine pool (api/machines.go). MachineList.aspx shows a
// grid (MainContent_Grid1) of the machines that report to the portal: Select, Machine No, Machine Model, Machine
// Serial No, Device Name, Device In_Out Mode and Branch Name, one row per machine (40 on 17 Nov 2026), and the
// buttons Add, Delete and Close. The client only opens the page (a GET) and reads the grid; it never presses a button
// there. The grid has no online/offline column; when a status column appears (Status, Online, Connection) it is read.

// Machine is one row of the portal's machine list.
type Machine struct {
	No     string `json:"no,omitempty"` // Machine No (1, 2, …)
	Model  string `json:"model,omitempty"`
	Serial string `json:"serial"`
	Name   string `json:"name"`
	Mode   string `json:"mode,omitempty"` // In, Out, In/Out
	Branch string `json:"branch,omitempty"`
	Status string `json:"status,omitempty"` // as the portal shows it, when it has a status column
	Online *bool  `json:"online,omitempty"` // nil when the page does not say
}

// MachineList signs in and returns the machine list page (HTML).
func (c *Client) MachineList(ctx context.Context) ([]byte, error) {
	if err := c.SignIn(ctx); err != nil {
		return nil, err
	}
	res, b, err := c.do(ctx, http.MethodGet, "/MachineList.aspx", nil, nil)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK || !strings.EqualFold(res.Request.URL.Path, "/machinelist.aspx") {
		return nil, errors.New("the machine list page did not open (signed out?)")
	}
	return b, nil
}

var (
	reTable = regexp.MustCompile(`(?is)<table\b.*?</table>`)
	reRow   = regexp.MustCompile(`(?is)<tr\b[^>]*>(.*?)</tr>`)
	reCell  = regexp.MustCompile(`(?is)<t[dh]\b[^>]*>(.*?)</t[dh]>`)
	reTag   = regexp.MustCompile(`(?s)<[^>]*>`)
	reImgS  = regexp.MustCompile(`(?is)<img\b[^>]*\b(?:src|alt|title)="([^"]*)"`)
	reSpace = regexp.MustCompile(`\s+`)
)

// cellText is a table cell's text; an image-only cell (a status light) gives its src/alt/title instead.
func cellText(s string) string {
	t := strings.TrimSpace(reSpace.ReplaceAllString(html.UnescapeString(reTag.ReplaceAllString(s, " ")), " "))
	if t == "" {
		if m := reImgS.FindStringSubmatch(s); m != nil {
			return m[1]
		}
	}
	return t
}

// machineCols finds the columns by their headings (-1 when absent).
type machineCols struct{ no, model, serial, name, mode, branch, status int }

func findMachineCols(head []string) (machineCols, bool) {
	m := machineCols{-1, -1, -1, -1, -1, -1, -1}
	for i, h := range head {
		k := strings.ToLower(strings.Join(strings.Fields(h), ""))
		switch {
		case strings.Contains(k, "serial"):
			m.serial = i
		case strings.Contains(k, "model"):
			m.model = i
		case strings.Contains(k, "mode"):
			m.mode = i
		case strings.Contains(k, "branch"):
			m.branch = i
		case strings.Contains(k, "status") || strings.Contains(k, "online") || strings.Contains(k, "connect"):
			m.status = i
		case k == "machineno" || k == "deviceno" || k == "no":
			m.no = i
		case strings.Contains(k, "name"):
			if m.name < 0 {
				m.name = i
			}
		}
	}
	return m, m.serial >= 0
}

// ParseMachines reads the machine list page: the first table whose heading row has a serial number column. A row
// without a serial number (a pager, a footer) is skipped.
func ParseMachines(page []byte) ([]Machine, error) {
	for _, tb := range reTable.FindAllString(string(page), -1) {
		rows := reRow.FindAllStringSubmatch(tb, -1)
		hi := -1
		var mc machineCols
		for i, r := range rows {
			cells := reCell.FindAllStringSubmatch(r[1], -1)
			head := make([]string, len(cells))
			for j, c := range cells {
				head[j] = cellText(c[1])
			}
			if m, ok := findMachineCols(head); ok {
				hi, mc = i, m
				break
			}
		}
		if hi < 0 {
			continue
		}
		out := []Machine{}
		for _, r := range rows[hi+1:] {
			cells := reCell.FindAllStringSubmatch(r[1], -1)
			get := func(i int) string {
				if i >= 0 && i < len(cells) {
					return cellText(cells[i][1])
				}
				return ""
			}
			m := Machine{No: get(mc.no), Model: get(mc.model), Serial: get(mc.serial), Name: get(mc.name), Mode: get(mc.mode), Branch: get(mc.branch), Status: get(mc.status)}
			if m.Serial == "" || len(cells) < 2 {
				continue
			}
			if m.Status != "" {
				on := isOnline(m.Status)
				m.Online = &on
			}
			out = append(out, m)
		}
		return out, nil
	}
	return nil, errors.New("the machine list page has no table with serial numbers")
}

// isOnline reads the portal's status text (or a status light's image name).
func isOnline(s string) bool {
	s = strings.ToLower(s)
	if strings.Contains(s, "offline") || strings.Contains(s, "off-line") || strings.Contains(s, "disconnect") {
		return false
	}
	return strings.Contains(s, "online") || strings.Contains(s, "on-line") || strings.Contains(s, "connected") || strings.Contains(s, "green") ||
		s == "on" || s == "active" || s == "true" || s == "1"
}
