package api

import "testing"

// The ICICI upload's text rules (bankfile.go): name cleaning, mobile numbers, the narration and the file names.

func TestICICIName(t *testing.T) {
	for in, want := range map[string]string{
		"Ravi Kumar":                "Ravi Kumar",
		"  S.K.  Sharma ":           "S K Sharma",
		"Anne-Marie D'Souza":        "Anne Marie DSouza",
		"Rahul (Jr.) #2 Verma!":     "Rahul Jr Verma",
		"Priya\tNair\n":             "Priya Nair",
		"José Ñuñez":                "Jos uez",
		"12345":                     "",
		"Mohd. Irfan / Khan_Pathan": "Mohd Irfan Khan Pathan",
	} {
		if got := iciciName(in); got != want {
			t.Errorf("iciciName(%q) = %q, want %q", in, got, want)
		}
	}
	long := ""
	for i := 0; i < 120; i++ {
		long += "Abcde "
	}
	if got := iciciName(long); len(got) > 500 || got[len(got)-1] == ' ' {
		t.Fatalf("long name: %d characters, %q at the end", len(got), got[len(got)-1:])
	}
}

func TestICICIMobile(t *testing.T) {
	for in, want := range map[string]string{
		"9876543210":        "9876543210",
		"+91 98765 43210":   "9876543210",
		"+91-98765-43210":   "9876543210",
		"919876543210":      "9876543210",
		"09876543210":       "9876543210",
		"(+91) 98765 43210": "9876543210",
		"98765 4321":        "",
		"+1 415 555 01234":  "",
		"98765abcde":        "",
		"":                  "",
	} {
		if got := iciciMobile(in); got != want {
			t.Errorf("iciciMobile(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestICICINarrationAndNames(t *testing.T) {
	if got := iciciNarration("2026-10"); got != "Oct 26 Salary" {
		t.Fatalf("narration %q", got)
	}
	if got := iciciNarration("2027-01"); got != "Jan 27 Salary" || len(got) > 30 {
		t.Fatalf("narration %q", got)
	}
	if got := iciciFileName("2026-10"); got != "ICICI-bank-upload-Oct-2026.xlsx" {
		t.Fatalf("file %q", got)
	}
	if got := fullRegisterName("2026-10", true); got != "full-salary-register-Oct-2026-DRAFT.xlsx" {
		t.Fatalf("draft file %q", got)
	}
	if got := fullRegisterName("2026-10", false); got != "full-salary-register-Oct-2026.xlsx" {
		t.Fatalf("file %q", got)
	}
	for _, ok := range []string{"38605005062", "123456789", "123456789012345678"} {
		if !reDebitAcc.MatchString(ok) {
			t.Errorf("%s refused", ok)
		}
	}
	for _, no := range []string{"12345678", "1234567890123456789", "3860500506A", " 38605005062", ""} {
		if reDebitAcc.MatchString(no) {
			t.Errorf("%q accepted", no)
		}
	}
	for _, ok := range []string{"ICIC0001234", "HDFC0ABC123"} {
		if !reBankIFSC.MatchString(ok) {
			t.Errorf("IFSC %s refused", ok)
		}
	}
	for _, no := range []string{"ICIC1001234", "HDFC000123", "hdfc0001234", "HDF00001234"} {
		if reBankIFSC.MatchString(no) {
			t.Errorf("IFSC %s accepted", no)
		}
	}
}
