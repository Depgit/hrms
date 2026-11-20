package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/xuri/excelize/v2"
)

// sheetFn builds a download: file name, column headings and rows.
type sheetFn func(r *http.Request, c *Ctx) (name string, cols []string, rows [][]any, err error)

// sheet serves an Excel file built by fn, for callers whose role may download (spec 6.3).
func (s *Server) sheet(fn sheetFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := s.authenticate(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		if !c.CanDownload {
			writeErr(w, forbidden("Your role cannot download files"))
			return
		}
		name, cols, rows, err := fn(r, c)
		if err != nil {
			writeErr(w, err)
			return
		}
		format := "xlsx"
		if r.URL.Query().Get("format") == "csv" {
			format = "csv"
		}
		s.auditDownload(r, c, name, format, len(rows))
		if format == "csv" { // the same file as CSV ("Download CSV")
			writeCSV(w, name, cols, rows)
			return
		}
		writeSheet(w, name, cols, rows)
	}
}

// auditDownload records who downloaded which file (bank advice, register, payroll sheet, statutory files and every
// other export), with the role, the request and the number of rows (ACC-19). A failure to record does not stop the
// download.
func (s *Server) auditDownload(r *http.Request, c *Ctx, name, format string, rows int) {
	_ = audit(r.Context(), s.st.DB, c, "Downloaded "+name, "download", name, nil,
		map[string]any{"path": r.URL.Path, "query": r.URL.RawQuery, "format": format, "rows": rows})
}

func writeSheet(w http.ResponseWriter, name string, cols []string, rows [][]any) {
	f := excelize.NewFile()
	defer f.Close()
	const sh = "Data"
	_ = f.SetSheetName("Sheet1", sh)
	head, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}, Fill: excelize.Fill{Type: "pattern", Color: []string{"E1E5E8"}, Pattern: 1}})
	for i, h := range cols {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		_ = f.SetCellValue(sh, cell, h)
		col, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetColWidth(sh, col, col, float64(max(12, min(40, len(h)+4))))
	}
	if len(cols) > 0 {
		end, _ := excelize.ColumnNumberToName(len(cols))
		_ = f.SetCellStyle(sh, "A1", end+"1", head)
		_ = f.SetPanes(sh, &excelize.Panes{Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft"})
	}
	for i, row := range rows {
		_ = f.SetSheetRow(sh, fmt.Sprintf("A%d", i+2), &row)
	}
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"; filename*=UTF-8''`+url.PathEscape(name))
	_ = f.Write(w)
}

// textFile serves a plain text download (ECR and other government formats).
func (s *Server) textFile(fn func(r *http.Request, c *Ctx) (name, body string, err error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := s.authenticate(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		if !c.CanDownload {
			writeErr(w, forbidden("Your role cannot download files"))
			return
		}
		name, body, err := fn(r, c)
		if err != nil {
			writeErr(w, err)
			return
		}
		s.auditDownload(r, c, name, "txt", strings.Count(body, "\n"))
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		_, _ = w.Write([]byte(body))
	}
}
