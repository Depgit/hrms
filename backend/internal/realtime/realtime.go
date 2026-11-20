// Package realtime reads punches from the Realtime India (Realtime Biometrics) "Attendance & Payroll" web portal,
// which has no API: it signs in like a browser, runs the Daily report "All Raw Punch Report" for one date, and
// downloads it as an Excel file (the Microsoft Report Viewer's export), which lists every employee of the portal
// with that day's swipe times.
//
// The client reads. It requests exactly seven pages, hard-coded in allowedPaths: the sign-in page, the
// landing page a browser opens after signing in (Welcome.aspx: the portal sets up the session there, and the
// report export answers 404 without it), the Daily report form, the report viewer (to let the report load), the
// viewer's export handler, the employee list, where only "Export Excel" is pressed (employees.go), and the machine
// list, which is only opened and read (machines.go). Every other page
// of the portal — including the ones that change data or control doors ("Update software Database", "Transfer
// Data In ERP", "Open & Close Gate") — is refused before a request is sent, and so are redirects to them.
//
// The one exception is the employee form (empwrite.go, 17 Nov 2026): its pages (Spec) are opened and posted only
// when HR confirms a "Send to portal" in the HRMS, to create an employee, update their details or mark them left.
//
// Credentials come from the environment only (REALTIME_URL, REALTIME_USER, REALTIME_PASS); they are never stored
// in the database or logged.
package realtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Config is where the portal is and how to sign in.
type Config struct {
	BaseURL string // e.g. http://203.0.113.10:83
	User    string
	Pass    string
}

// FromEnv reads REALTIME_URL, REALTIME_USER and REALTIME_PASS. ok is false when any is missing.
func FromEnv(getenv func(string) string) (Config, bool) {
	c := Config{BaseURL: strings.TrimRight(strings.TrimSpace(getenv("REALTIME_URL")), "/"),
		User: strings.TrimSpace(getenv("REALTIME_USER")), Pass: getenv("REALTIME_PASS")}
	return c, c.BaseURL != "" && c.User != "" && c.Pass != ""
}

// Host is the portal's address without credentials, for the setup page.
func (c Config) Host() string {
	if u, err := url.Parse(c.BaseURL); err == nil && u.Host != "" {
		return u.Host
	}
	return c.BaseURL
}

// The only paths the client may request (compared case-insensitively).
var allowedPaths = map[string]bool{
	"/loginnew.aspx":                       true,
	"/welcome.aspx":                        true,
	"/daily.aspx":                          true,
	"/reportviewer.aspx":                   true,
	"/reserved.reportviewerwebcontrol.axd": true,
	"/employeelist.aspx":                   true, // the employee list; only its Export Excel button (employees.go)
	"/machinelist.aspx":                    true, // the machine list; opened (GET) and read, no button pressed (machines.go)
}

// ErrNotAllowed is returned for a request outside allowedPaths.
var ErrNotAllowed = errors.New("realtime: page not on the read-only list")

// ErrSignIn is returned when the portal does not accept the username and password.
var ErrSignIn = errors.New("the portal did not accept the username and password (REALTIME_USER, REALTIME_PASS)")

// Client is one signed-in browsing session.
type Client struct {
	cfg  Config
	base *url.URL
	hc   *http.Client
}

// New makes a client; Report signs in when needed.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("REALTIME_URL is not an address like http://203.0.113.10:83")
	}
	jar, _ := cookiejar.New(nil)
	c := &Client{cfg: cfg, base: u}
	// Requests are made like the plain client the flow was worked out with: a new connection each time, no
	// compression, no Referer on redirects.
	c.hc = &http.Client{Jar: jar, Timeout: 5 * time.Minute, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DisableKeepAlives: true, DisableCompression: true},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if !c.allowed(req.URL) {
				return http.ErrUseLastResponse // Default.aspx when signed out, and anything else off the list
			}
			req.Header.Del("Referer")
			return nil
		}}
	return c, nil
}

func (c *Client) allowed(u *url.URL) bool {
	return u.Host == c.base.Host && (allowedPaths[strings.ToLower(u.Path)] || writePath(u.Path))
}

// do sends one request to an allowed path and returns the final response with its body read.
func (c *Client) do(ctx context.Context, method, path string, form url.Values, hdr map[string]string) (*http.Response, []byte, error) {
	u, err := c.base.Parse(path)
	if err != nil {
		return nil, nil, err
	}
	if !c.allowed(u) {
		return nil, nil, ErrNotAllowed
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 HRMS-connector")
	req.Header.Set("Accept-Encoding", "identity")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("the portal could not be reached: %w", stripURL(err))
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, nil, err
	}
	return res, b, nil
}

// stripURL drops the request URL from a transport error (it never holds credentials, but keeps messages short).
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// SignIn signs in through loginnew.aspx. Success is the redirect to the landing page (Welcome.aspx), which is
// opened as a browser would.
func (c *Client) SignIn(ctx context.Context) error {
	res, b, err := c.do(ctx, http.MethodGet, "/loginnew.aspx", nil, nil)
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("the sign-in page answered %d", res.StatusCode)
	}
	page := string(b)
	f := hiddenFields(page)
	f.Set("TextBox2", c.cfg.User)
	f.Set("TextBox3", c.cfg.Pass)
	f.Set("ddlLanguage", "0")
	if v, ok := firstOption(page, "Cbologinas"); ok {
		f.Set("Cbologinas", v)
	}
	f.Set("Button1", "Log In") // never Button2 ("Update software Database")
	res, _, err = c.do(ctx, http.MethodPost, "/loginnew.aspx", f, nil)
	if err != nil {
		return err
	}
	if res.StatusCode == http.StatusOK && strings.EqualFold(res.Request.URL.Path, "/welcome.aspx") {
		return nil
	}
	return ErrSignIn
}

// Report signs in and returns the All Raw Punch Report of one day as the exported Excel file.
func (c *Client) Report(ctx context.Context, day time.Time) ([]byte, error) {
	if err := c.SignIn(ctx); err != nil {
		return nil, err
	}
	res, b, err := c.do(ctx, http.MethodGet, "/Daily.aspx", nil, nil)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK || !strings.Contains(string(b), "cmdShowReport") {
		return nil, errors.New("the Daily report page did not open (signed out?)")
	}
	f := dailyForm(string(b), day)
	res, b, err = c.do(ctx, http.MethodPost, "/Daily.aspx", f, nil)
	if err != nil {
		return nil, err
	}
	viewer := string(b)
	if res.StatusCode != http.StatusOK || !strings.Contains(viewer, "ExportUrlBase") {
		return nil, errors.New("the report did not open in the report viewer")
	}
	// The viewer renders the report on a second (partial) postback; the export works only after it.
	target := asyncTarget(viewer)
	a := hiddenFields(viewer)
	sm := "ctl00$ScriptManager1"
	if m := reScriptMgr.FindStringSubmatch(viewer); m != nil {
		sm = m[1]
	}
	a.Set(sm, target)
	a.Set("__EVENTTARGET", target)
	a.Set("__EVENTARGUMENT", "")
	a.Set("__ASYNCPOST", "true")
	if res, _, err = c.do(ctx, http.MethodPost, "/ReportViewer.aspx", a, map[string]string{"X-MicrosoftAjax": "Delta=true", "X-Requested-With": "XMLHttpRequest"}); err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the report viewer answered %d", res.StatusCode)
	}
	m := reExportBase.FindStringSubmatch(viewer)
	if m == nil {
		return nil, errors.New("the report viewer has no export link")
	}
	exp := strings.ReplaceAll(m[1], `\u0026`, "&") + "EXCEL" // the page writes & as \u0026 inside a script
	res, b, err = c.do(ctx, http.MethodGet, exp, nil, nil)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK || !bytes.HasPrefix(b, oleMagic) {
		return nil, fmt.Errorf("the Excel export failed (%d, %s)", res.StatusCode, res.Header.Get("Content-Type"))
	}
	return b, nil
}

var (
	oleMagic     = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}
	reExportBase = regexp.MustCompile(`"ExportUrlBase":"([^"]+)"`)
	reScriptMgr  = regexp.MustCompile(`Sys\.WebForms\.PageRequestManager\._initialize\('([^']+)'`)
	reAsync      = regexp.MustCompile(`id="([A-Za-z0-9_]+)_Reserved_AsyncLoadTarget"`)
	reInput      = regexp.MustCompile(`(?is)<input\b[^>]*>`)
	reSelect     = regexp.MustCompile(`(?is)<select\b[^>]*\bname="([^"]*)"[^>]*>(.*?)</select>`)
	reOption     = regexp.MustCompile(`(?is)<option\b([^>]*)\bvalue="([^"]*)"`)
	reAttr       = func(name string) *regexp.Regexp { return regexp.MustCompile(`(?is)\b` + name + `="([^"]*)"`) }
	reName       = reAttr("name")
	reType       = reAttr("type")
	reValue      = reAttr("value")
	reChecked    = regexp.MustCompile(`(?i)\bchecked\b`)
	reSelected   = regexp.MustCompile(`(?i)\bselected\b`)
)

// asyncTarget is the name of the viewer's async load target (ctl00$MainContent$ReportViewer1$ctl10$Reserved_AsyncLoadTarget).
func asyncTarget(page string) string {
	if m := reAsync.FindStringSubmatch(page); m != nil {
		return strings.ReplaceAll(m[1], "_", "$") + "$Reserved_AsyncLoadTarget"
	}
	return "ctl00$MainContent$ReportViewer1$ctl10$Reserved_AsyncLoadTarget"
}

func attr(re *regexp.Regexp, tag string) (string, bool) {
	if m := re.FindStringSubmatch(tag); m != nil {
		return html.UnescapeString(m[1]), true
	}
	return "", false
}

// hiddenFields is every hidden input of the page (ASP.NET view state and friends).
func hiddenFields(page string) url.Values {
	f := url.Values{}
	for _, t := range reInput.FindAllString(page, -1) {
		if ty, _ := attr(reType, t); !strings.EqualFold(ty, "hidden") {
			continue
		}
		if n, ok := attr(reName, t); ok {
			v, _ := attr(reValue, t)
			f.Set(n, v)
		}
	}
	return f
}

func firstOption(page, name string) (string, bool) {
	for _, m := range reSelect.FindAllStringSubmatch(page, -1) {
		if html.UnescapeString(m[1]) != name {
			continue
		}
		if o := reOption.FindStringSubmatch(m[2]); o != nil {
			return html.UnescapeString(o[2]), true
		}
	}
	return "", false
}

// dailyForm is the Daily report form as a browser would send it with its defaults (every company, branch, shift,
// department, designation and employee), "All Raw Punch Report" chosen, the date set and Show Report pressed. No
// other button is included.
func dailyForm(page string, day time.Time) url.Values {
	if i := strings.Index(page, "<form"); i >= 0 {
		page = page[i:]
	}
	f := url.Values{}
	for _, t := range reInput.FindAllString(page, -1) {
		n, ok := attr(reName, t)
		if !ok {
			continue
		}
		ty, _ := attr(reType, t)
		ty = strings.ToLower(ty)
		v, hasV := attr(reValue, t)
		switch ty {
		case "submit", "button", "image", "file", "reset":
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
	for _, m := range reSelect.FindAllStringSubmatch(page, -1) {
		var first, sel string
		for _, o := range reOption.FindAllStringSubmatch(m[2], -1) {
			if first == "" {
				first = html.UnescapeString(o[2])
			}
			if sel == "" && reSelected.MatchString(o[1]) {
				sel = html.UnescapeString(o[2])
			}
		}
		if sel == "" {
			sel = first
		}
		if sel != "" {
			f.Set(html.UnescapeString(m[1]), sel)
		}
	}
	f.Set("ctl00$MainContent$Daily", "AllIn_OutReport")
	f.Set("ctl00$MainContent$txtdate", day.Format("02/01/2006"))
	f.Del("ctl00$MainContent$chknewwindow")
	f.Set("ctl00$MainContent$cmdShowReport", "ShowReport")
	return f
}
