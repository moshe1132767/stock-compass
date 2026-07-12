package marketdata

import (
	"testing"
	"time"
)

// בקשה של משתמש חייבת לחזור מיד — גם כשהמכסה של הספק מלאה לגמרי.
// זה הכלל שנשבר פעם אחת (בקשות נתקעו ~60 שניות) ואסור שיישבר שוב:
// עדיף להגיש נתון מהמטמון מאשר להשהות את המסך.
func TestUserNeverWaits(t *testing.T) {
	var l limiter
	for i := 0; i < rateMax; i++ { // ממלאים את כל המכסה
		if err := l.take(WaitUser, 0); err != nil {
			t.Fatalf("בקשה %d נחסמה למרות שיש מכסה: %v", i+1, err)
		}
	}

	start := time.Now()
	err := l.take(WaitUser, 0)
	el := time.Since(start)

	if err != ErrBusy {
		t.Fatalf("כשהמכסה מלאה בקשת משתמש אמורה לחזור עם ErrBusy, קיבלנו %v", err)
	}
	if el > 2*time.Second {
		t.Fatalf("בקשת משתמש המתינה %s — אסור. היא חייבת לחזור מיד ולתת למטמון לענות", el)
	}
	t.Logf("מכסה מלאה → המשתמש קיבל תשובה תוך %s ✔", el.Round(time.Millisecond))
}

// רענון רקע דווקא כן ממתין בסבלנות — אף אחד לא מסתכל עליו.
func TestBackgroundIsPatient(t *testing.T) {
	if WaitBG <= WaitUser {
		t.Fatal("רענון הרקע אמור להיות סבלני יותר מבקשת משתמש")
	}
	l := limiter{}
	// מכסה שהתמלאה לפני חצי דקה — הרקע ימתין להתפנות, המשתמש לא היה ממתין
	for i := 0; i < rateMax; i++ {
		l.hits = append(l.hits, time.Now().Add(-rateWindow+400*time.Millisecond))
	}
	start := time.Now()
	if err := l.take(WaitBG, 0); err != nil {
		t.Fatalf("רענון רקע היה אמור להמתין ולעבור: %v", err)
	}
	if el := time.Since(start); el < 300*time.Millisecond {
		t.Fatalf("הרקע לא באמת המתין (%s) — הבלם לא עובד", el)
	}
}

// הרקע לא גונב את המקומות האחרונים: תמיד נשאר אוויר לבקשה של משתמש.
func TestBackgroundLeavesRoomForUser(t *testing.T) {
	var l limiter
	for i := 0; i < rateMax-2; i++ { // הרקע ממלא עד לרזרבה
		if err := l.take(time.Millisecond, 2); err != nil {
			t.Fatalf("רענון רקע %d נחסם מוקדם מדי: %v", i+1, err)
		}
	}
	if err := l.take(time.Millisecond, 2); err != ErrBusy {
		t.Fatal("הרקע היה צריך לעצור ברזרבה ולא לגעת במקומות של המשתמשים")
	}
	// והמשתמש? עדיין עובר.
	if err := l.take(WaitUser, 0); err != nil {
		t.Fatalf("המשתמש נחסם למרות שהרזרבה נשמרה בשבילו: %v", err)
	}
}

// כשחלון הזמן חולף — המכסה מתפנה מעצמה.
func TestLimiterFreesUpAfterWindow(t *testing.T) {
	l := limiter{}
	old := time.Now().Add(-rateWindow - time.Second)
	for i := 0; i < rateMax; i++ {
		l.hits = append(l.hits, old)
	}
	if err := l.take(50*time.Millisecond, 0); err != nil {
		t.Fatalf("בקשות ישנות היו צריכות להתפנות מהחלון: %v", err)
	}
}
