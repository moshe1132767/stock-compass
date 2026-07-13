package marketdata

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestDumpCandles — שומר לקובץ את הנרות בדיוק כפי שהאפליקציה רואה אותם,
// כדי שאפשר יהיה להשוות את חישובי האינדיקטורים למימוש עצמאי.
// DUMP_SYMS=NVDA,AAPL DUMP_DIR=/tmp/x go test ./internal/marketdata -run TestDumpCandles
func TestDumpCandles(t *testing.T) {
	syms := os.Getenv("DUMP_SYMS")
	dir := os.Getenv("DUMP_DIR")
	if syms == "" || dir == "" {
		t.Skip("הרץ עם DUMP_SYMS ו-DUMP_DIR")
	}
	c := New("", "") // בלי מפתחות — רק Yahoo, בדיוק כמו בייצור
	for _, sym := range strings.Split(syms, ",") {
		cs, meta, err := c.History(sym, WaitUser)
		if err != nil {
			t.Fatalf("%s: %v", sym, err)
		}
		b, _ := json.Marshal(cs)
		if err := os.WriteFile(dir+"/"+sym+"_all.json", b, 0644); err != nil {
			t.Fatal(err)
		}
		// והחלון שנכנס בפועל למנוע האינדיקטורים
		w := cs
		if len(w) > 300 {
			w = w[len(w)-300:]
		}
		b, _ = json.Marshal(w)
		os.WriteFile(dir+"/"+sym+"_300.json", b, 0644)
		t.Logf("%s: %d נרות (%s → %s), שם=%s", sym, len(cs), cs[0].Date, cs[len(cs)-1].Date, meta.Name)
	}
}
