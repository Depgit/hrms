package realtime

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// Discover is the read-only look at the portal that confirms where the employee form is (FormSpec): it signs in,
// opens the landing page and the employee list (GET), lists the pages they link to (menu links, window.open,
// PostBackUrl targets) and every button of the employee list with what it runs, then opens (GET only) each linked
// page whose name is about employees and lists its controls (label, name, kind, required, choices) and buttons.
// It never posts anything but the sign-in, never presses a button, and never opens a page whose name says it
// changes data (neverOpen). The employee list's grid (it shows device passwords) is not printed. Used by
// `hrms realtime-forms`.
func (c *Client) Discover(ctx context.Context, out io.Writer) error {
	if err := c.SignIn(ctx); err != nil {
		return err
	}
	pages := map[string]bool{}
	for _, p := range []string{"/Welcome.aspx", "/EmployeeList.aspx"} {
		res, b, err := c.do(ctx, http.MethodGet, p, nil, nil)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "== %s (%d)\n", p, res.StatusCode)
		page := string(b)
		for _, l := range pageLinks(page) {
			pages[l] = true
		}
		if strings.EqualFold(p, "/EmployeeList.aspx") {
			for _, bt := range buttonsWithScript(page) {
				fmt.Fprintf(out, "  button %q name=%s %s\n", bt[1], bt[0], bt[2])
			}
			for _, l := range gridLinkPatterns(page) {
				fmt.Fprintf(out, "  grid link %s\n", l)
			}
		}
	}
	list := make([]string, 0, len(pages))
	for p := range pages {
		list = append(list, p)
	}
	sort.Strings(list)
	fmt.Fprintf(out, "== linked pages\n  %s\n", strings.Join(list, "\n  "))
	for _, p := range list {
		if !reEmpPage.MatchString(p) || neverOpen.MatchString(strings.ToLower(p)) {
			continue
		}
		res, b, err := c.discoverGet(ctx, p)
		if err != nil {
			fmt.Fprintf(out, "== %s: %v\n", p, err)
			continue
		}
		fmt.Fprintf(out, "== form %s (%d, ended at %s)\n", p, res.StatusCode, res.Request.URL.Path)
		f := ParseForm(string(b))
		for _, fl := range f.Fields {
			if fl.Kind == "hidden" {
				continue
			}
			req := ""
			if fl.Required {
				req = " REQUIRED"
			}
			opts := ""
			if n := len(fl.Options); n > 0 {
				show := []string{}
				for i, o := range fl.Options {
					if i == 40 {
						show = append(show, fmt.Sprintf("… %d more", n-40))
						break
					}
					show = append(show, o.Value+"="+o.Text)
				}
				opts = " [" + strings.Join(show, " | ") + "]"
			}
			fmt.Fprintf(out, "  %-28q %-8s %s role=%s%s%s\n", fl.Label, fl.Kind, fl.Name, fl.Role, req, opts)
		}
		for _, bt := range f.Buttons {
			fmt.Fprintf(out, "  button %q name=%s\n", bt.Caption, bt.Name)
		}
	}
	return nil
}

var (
	reEmpPage = regexp.MustCompile(`(?i)^/[a-z0-9_\-/]*emp[a-z0-9_\-]*\.aspx(\?.*)?$`)
	// Pages never opened, even with a GET: their names say they change data or devices.
	neverOpen  = regexp.MustCompile(`delete|del\b|upload|import|update|transfer|gate|backup|erp|process|device|machine|batch|shift|password|reset|login|logout|signout`)
	reHref     = regexp.MustCompile(`(?is)\bhref=["']([^"'#]+\.aspx[^"']*)["']`)
	reJSPage   = regexp.MustCompile(`(?is)(?:window\.open|location\.href\s*=|location\s*=|PostBackOptions\([^)]*?)\s*\(?\s*["']([^"']+\.aspx[^"']*)["']`)
	reGridLink = regexp.MustCompile(`(?is)\bhref=["']([^"']*\.aspx\?[^"']*)["']`)
)

// pageLinks are the .aspx pages a page links to, as paths.
func pageLinks(page string) []string {
	seen := map[string]bool{}
	var out []string
	for _, re := range []*regexp.Regexp{reHref, reJSPage} {
		for _, m := range re.FindAllStringSubmatch(page, -1) {
			l := html.UnescapeString(m[1])
			if i := strings.Index(l, "://"); i >= 0 {
				continue // another site
			}
			l = strings.TrimPrefix(strings.TrimPrefix(l, "~"), ".")
			if !strings.HasPrefix(l, "/") {
				l = "/" + l
			}
			if i := strings.IndexByte(l, '?'); i >= 0 {
				l = l[:i]
			}
			if !seen[strings.ToLower(l)] {
				seen[strings.ToLower(l)] = true
				out = append(out, l)
			}
		}
	}
	return out
}

// buttonsWithScript lists the page's buttons: name, caption and any onclick or PostBackUrl target.
func buttonsWithScript(page string) [][3]string {
	var out [][3]string
	for _, t := range reInput.FindAllString(page, -1) {
		ty, _ := attr(reType, t)
		if !strings.EqualFold(ty, "submit") && !strings.EqualFold(ty, "button") && !strings.EqualFold(ty, "image") {
			continue
		}
		n, _ := attr(reName, t)
		v, _ := attr(reValue, t)
		on, _ := attr(reAttr("onclick"), t)
		out = append(out, [3]string{n, v, on})
	}
	return out
}

// gridLinkPatterns are the distinct links with a query string (an edit link per row), values replaced by {v}.
func gridLinkPatterns(page string) []string {
	seen := map[string]bool{}
	var out []string
	reVal := regexp.MustCompile(`=([^&]*)`)
	for _, m := range reGridLink.FindAllStringSubmatch(page, -1) {
		l := reVal.ReplaceAllString(html.UnescapeString(m[1]), "={v}")
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

// discoverGet opens a page for Discover: GET only, the portal's own host, an employee page whose name does not
// say it changes anything.
func (c *Client) discoverGet(ctx context.Context, path string) (*http.Response, []byte, error) {
	if !reEmpPage.MatchString(path) || neverOpen.MatchString(strings.ToLower(path)) {
		return nil, nil, ErrNotAllowed
	}
	u, err := c.base.Parse(path)
	if err != nil || u.Host != c.base.Host {
		return nil, nil, ErrNotAllowed
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 HRMS-connector")
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, nil, stripURL(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	return res, b, err
}
