package api

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"

	"github.com/saltstayz/hrms/internal/calc"
)

// PPL-091, PPL-155: a date cell reads as its date whatever its display format (US default, dd-mmm-yyyy, d-mmm-yy,
// dd/mm/yyyy, a custom one), a serial number in a General cell too, and text dates are day first.
func TestFtUploadDates(t *testing.T) {
	x := excelize.NewFile()
	sh := x.GetSheetName(0)
	d := time.Date(2024, 6, 12, 0, 0, 0, 0, time.UTC)
	cust := func(f string) int { s, _ := x.NewStyle(&excelize.Style{CustomNumFmt: &f}); return s }
	builtin14, _ := x.NewStyle(&excelize.Style{NumFmt: 14})
	styles := []int{builtin14, cust("dd-mmm-yyyy"), cust("d-mmm-yy"), cust("dd/mm/yyyy"), cust("mm-dd-yy"), cust("yyyy-mm-dd")}
	_ = x.SetCellValue(sh, "A1", "Date")
	for i, st := range styles {
		cell, _ := excelize.CoordinatesToCellName(1, i+2)
		_ = x.SetCellValue(sh, cell, d)
		_ = x.SetCellStyle(sh, cell, cell, st)
	}
	_ = x.SetCellValue(sh, "A8", 45455)         // serial in a General cell
	_ = x.SetCellValue(sh, "A9", "12-06-24")    // text, two-digit year
	_ = x.SetCellValue(sh, "A10", "12/06/2024") // text
	_ = x.SetCellValue(sh, "B2", 26000)         // a plain number stays
	var xb bytes.Buffer
	_ = x.Write(&xb)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	f, _ := w.CreateFormFile("file", "d.xlsx")
	_, _ = f.Write(xb.Bytes())
	_ = w.Close()
	r := httptest.NewRequest("POST", "/", &body)
	r.Header.Set("Content-Type", w.FormDataContentType())
	rows, err := readUpload(r)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(rows); i++ {
		if got := importDate(rows[i][0]); got != "2024-06-12" {
			t.Errorf("row %d: %q read as %q", i+1, rows[i][0], got)
		}
		if got := excelDate(rows[i][0]); got != "2024-06-12" {
			t.Errorf("row %d: excelDate %q read as %q", i+1, rows[i][0], got)
		}
	}
	if rows[1][1] != "26000" {
		t.Errorf("number cell: %q", rows[1][1])
	}
}

// SET-111: a part-year employee's PF for the year is worked out month by month on the real gross (the PF ceiling
// applies each month), not on an averaged gross.
func TestFtPartYearPF(t *testing.T) {
	lwd := "2026-09-30"
	e := &emp{Type: "Permanent", DOJ: "2020-01-01", LWD: &lwd}
	st := calc.DefaultStructure
	f := taxFigure(e, 56300, &st, "Gurugram", &taxDecl{Regime: "Old", Items: map[string]float64{}}, map[string]float64{}, "2026-27")
	if f == nil || f.Months != 6 {
		t.Fatalf("%+v", f)
	}
	m := calc.Calc(calc.Input{Type: calc.Permanent, Gross: 56300, Structure: &st})
	if want := calc.Round2(m.EePF * 6); f.PFYear != want {
		t.Errorf("PF for the year %v, want 6 × %v = %v", f.PFYear, m.EePF, want)
	}
}

// TIME-233: a double tap within 2 minutes is one punch.
func TestFtDoubleTap(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 4, h, m, 0, 0, time.UTC) }
	in, out, _ := pick([]punchPt{{t: at(6, 0)}, {t: at(6, 1)}})
	if in == nil || out != nil {
		t.Errorf("06:00/06:01: in %v out %v", in, out)
	}
	in, out, _ = pick([]punchPt{{t: at(6, 0)}, {t: at(6, 1)}, {t: at(14, 0)}})
	if in == nil || out == nil || !out.Equal(at(14, 0)) {
		t.Errorf("06:00/06:01/14:00: in %v out %v", in, out)
	}
}

// AUTH-041, AUTH-042 (owner, 27 Nov 2026): failures and OTPs are counted per Employee ID and address, so one
// person's mistakes don't stop others on the same office Wi-Fi; the address as a whole has a higher ceiling.
func TestFtAuthLimiterProduction(t *testing.T) {
	l := newAuthLimiter(false)
	for i := 0; i < 10; i++ {
		if l.blocked("1.2.3.4", "EMP-1") != nil {
			t.Fatalf("blocked after %d failures", i)
		}
		l.failed("1.2.3.4", "EMP-1")
	}
	if l.blocked("1.2.3.4", "EMP-1") == nil {
		t.Error("the 11th try for the ID from the address is not refused")
	}
	if l.blocked("1.2.3.4", "EMP-2") != nil || l.blocked("5.6.7.8", "EMP-1") != nil {
		t.Error("another person on the same Wi-Fi, or the same ID elsewhere, is refused")
	}
	for i := 0; i < 10; i++ {
		if l.otpSend("9.9.9.9", "EMP-1") != nil {
			t.Fatalf("OTP %d refused", i+1)
		}
	}
	if l.otpSend("9.9.9.9", "EMP-1") == nil || l.otpSend("9.9.9.9", "EMP-2") != nil {
		t.Error("11th OTP for one ID allowed, or another ID on the same address refused")
	}
	// The address as a whole: many IDs tried from one machine stop at authIPFailMax.
	for i := 0; i < authIPFailMax; i++ {
		l.failed("7.7.7.7", fmt.Sprintf("EMP-X%d", i))
	}
	if l.blocked("7.7.7.7", "EMP-NEW") == nil {
		t.Error("an address past its ceiling may still try new IDs")
	}
}
