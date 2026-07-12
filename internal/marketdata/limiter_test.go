package marketdata

import (
	"testing"
	"time"
)

// בלם הקצב הוא מה שמונע את "נגמרו הקרדיטים לדקה הזו".
// מוודאים שהוא באמת עוצר בדיוק במכסה — ולא נותן לבקשה השמינית לעבור.
func TestLimiterStopsAtQuota(t *testing.T) {
	var l limiter

	start := time.Now()
	for i := 0; i < rateMax; i++ {
		if err := l.take(time.Second); err != nil {
			t.Fatalf("בקשה %d נחסמה למרות שיש מכסה: %v", i+1, err)
		}
	}
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Fatalf("%d הבקשות הראשונות אמורות לעבור מיד, לקח %s", rateMax, el)
	}

	// המכסה מלאה — הבקשה הבאה חייבת להמתין, ואם אין לה זמן, לחזור עם ErrBusy
	if err := l.take(200 * time.Millisecond); err != ErrBusy {
		t.Fatalf("ציפינו ל-ErrBusy כשהמכסה מלאה, קיבלנו %v", err)
	}
}

// כשחלון הזמן חולף — המכסה מתפנה מעצמה.
func TestLimiterFreesUpAfterWindow(t *testing.T) {
	l := limiter{}
	old := time.Now().Add(-rateWindow - time.Second)
	for i := 0; i < rateMax; i++ {
		l.hits = append(l.hits, old) // בקשות ישנות שכבר יצאו מהחלון
	}
	if err := l.take(50 * time.Millisecond); err != nil {
		t.Fatalf("בקשות ישנות היו צריכות להתפנות מהחלון: %v", err)
	}
}
