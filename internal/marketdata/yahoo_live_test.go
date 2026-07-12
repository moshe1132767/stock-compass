package marketdata

import (
	"os"
	"testing"
	"time"
)

// מבחן רשת אמיתי מול Yahoo. רץ רק כשמבקשים במפורש (LIVE=1 go test ./internal/marketdata/),
// כדי שהבנייה האוטומטית לא תהיה תלויה בספק חיצוני.
func TestYahooLive(t *testing.T) {
	if os.Getenv("LIVE") == "" {
		t.Skip("מבחן רשת — הרץ עם LIVE=1")
	}
	y := newYahoo()

	cs, m, err := y.daily("AAPL", time.Time{})
	if err != nil {
		t.Fatalf("היסטוריה יומית נכשלה: %v", err)
	}
	t.Logf("היסטוריה: %d נרות, %s → %s, שם=%q בורסה=%q סוג=%q",
		len(cs), cs[0].Date, cs[len(cs)-1].Date, m.Name, m.Exchange, m.Type)
	if len(cs) < 2000 {
		t.Fatalf("ציפינו להיסטוריה עמוקה, קיבלנו %d נרות", len(cs))
	}
	last := cs[len(cs)-1]
	if last.Close <= 0 || last.High < last.Low || last.Volume <= 0 {
		t.Fatalf("נר אחרון לא הגיוני: %+v", last)
	}

	for _, rng := range []string{"1h", "1d", "1w"} {
		pts, err := (&Client{y: y}).Intraday("NVDA", rng, WaitUser)
		if err != nil {
			t.Fatalf("גרף %s נכשל: %v", rng, err)
		}
		t.Logf("גרף %-3s: %d נקודות, %s → %s", rng, len(pts), pts[0].T, pts[len(pts)-1].T)
	}
}
