package indicators

import (
	"math"
	"testing"
)

// המומנטום חייב להיות בדיוק התשואה של 12 החודשים האחרונים — אותו מספר שהמשתמש
// רואה בכל אתר פיננסי. פעם הוא חישב נוסחה אקדמית שמדלגת על החודש האחרון,
// והמספר על המסך לא הסתדר עם המציאות. זה בדיוק מה שהמבחן הזה מונע.
func TestMomentumIsTrueYearReturn(t *testing.T) {
	n := 300
	closes := make([]float64, n)
	for i := range closes {
		closes[i] = 50 + float64(i)*0.7 // מסלול עולה כלשהו
	}

	got, months, ok := momentum12m(closes)
	if !ok {
		t.Fatal("היה צריך להיות אפשר לחשב מומנטום על 300 נרות")
	}
	want := closes[n-1]/closes[n-1-252] - 1
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("המומנטום אינו התשואה השנתית האמיתית: %.6f במקום %.6f", got, want)
	}
	if math.Abs(months-12) > 0.01 {
		t.Fatalf("היה צריך לדווח על 12 חודשים, דיווח על %.1f", months)
	}
	t.Logf("מומנטום = %.2f%% = בדיוק התשואה מלפני 252 ימי מסחר ✔", got*100)
}

// מניה צעירה שאין לה שנה שלמה: מותר לחשב, אבל אסור להבטיח "שנה".
func TestMomentumYoungStockTellsTheTruth(t *testing.T) {
	n := 100
	cs := make([]Candle, n)
	for i := range cs {
		p := 20 + float64(i)*0.5
		cs[i] = Candle{Date: "2026-01-01", Open: p, High: p * 1.01, Low: p * 0.99, Close: p, Volume: 1e6}
	}
	closes := make([]float64, n)
	for i, c := range cs {
		closes[i] = c.Close
	}
	_, months, ok := momentum12m(closes)
	if !ok {
		t.Fatal("גם למניה צעירה אפשר לחשב מומנטום")
	}
	if months >= 11.5 {
		t.Fatalf("עם 100 נרות בלבד אסור לטעון ל-12 חודשים (דיווח %.1f)", months)
	}

	res := Analyze(cs)
	for _, ind := range res.Indicators {
		if ind.Key != "momentum" {
			continue
		}
		if ind.Name == "מומנטום 12 חודשים" {
			t.Fatal("הכיתוב מבטיח שנה שלמה למניה שאין לה שנה של נתונים")
		}
		t.Logf("מניה צעירה → %q · %s ✔", ind.Name, ind.Detail)
		return
	}
	t.Fatal("אינדיקטור המומנטום נעלם")
}
