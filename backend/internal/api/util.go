package api

import (
	"encoding/json"
	"strconv"
	"time"
)

func jsonOrNil(v any) any {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return b
}

func today() string { return time.Now().Format("2006-01-02") }

func isDate(s string) bool { _, err := time.Parse("2006-01-02", s); return err == nil }

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func joinComma(l []string) string {
	out := ""
	for i, s := range l {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }
