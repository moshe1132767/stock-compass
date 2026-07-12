package backtest

import (
	"math"
	"testing"

	"stockcompass/internal/indicators"
)

// synth — בונה סדרת נרות מלאכותית ממסלול מחירים.
func synth(prices []float64) []indicators.Candle {
	cs := make([]indicators.Candle, len(prices))
	for i, p := range prices {
		cs[i] = indicators.Candle{
			Date: "2020-01-01", Open: p, High: p * 1.01, Low: p * 0.99,
			Close: p, Volume: 1_000_000,
		}
	}
	return cs
}

func period(r Result, key string) Period {
	for _, p := range r.Periods {
		if p.Key == key {
			return p
		}
	}
	return Period{}
}

// מניה שרק עולה: ההמלצה אף פעם לא הופכת למכירה, ולכן "להקשיב" = "לקנות ולהחזיק".
// זה בדיוק המקרה שנראה למשתמש כמו באג — כאן מוודאים שהוא מכוון ונכון.
func TestSteadyRiseNeverSells(t *testing.T) {
	n := 1600
	prices := make([]float64, n)
	for i := range prices {
		prices[i] = 100 * math.Pow(1.0008, float64(i)) // עלייה יציבה
	}
	p := period(Run(synth(prices)), "1y")

	if !p.Available {
		t.Fatal("תקופת השנה אמורה להיות זמינה")
	}
	if p.Trades != 0 {
		t.Fatalf("במגמת עלייה רצופה לא אמורות להיות פעולות, היו %d", p.Trades)
	}
	if math.Abs(p.Strategy-p.BuyHold) > 0.01 {
		t.Fatalf("בלי פעולות התוצאה חייבת להיות זהה לקנה־והחזק: %.2f מול %.2f", p.Strategy, p.BuyHold)
	}
	if p.MinScore <= sellAt {
		t.Fatalf("הציון ירד ל-%d (סף המכירה %d) אבל לא נרשמה מכירה", p.MinScore, sellAt)
	}
	t.Logf("עלייה רצופה: 0 פעולות, הציון נע בין %d ל-%d ✔", p.MinScore, p.MaxScore)
}

// מניה שעולה ואז קורסת: כאן ההמלצה *חייבת* לצאת, ולהציל חלק מהנפילה.
// זה מוכיח שמנגנון המכירה באמת עובד ולא תקוע.
func TestCrashTriggersExit(t *testing.T) {
	n := 1600
	prices := make([]float64, n)
	for i := 0; i < 1100; i++ { // עלייה: 100 → 300
		prices[i] = 100 * math.Pow(1.001, float64(i))
	}
	peak := prices[1099]
	for i := 1100; i < n; i++ { // קריסה ארוכה: ~70% מטה
		prices[i] = peak * math.Pow(0.9976, float64(i-1100))
	}
	p := period(Run(synth(prices)), "5y")

	if !p.Available {
		t.Fatal("תקופת 5 השנים אמורה להיות זמינה")
	}
	if p.Trades == 0 {
		t.Fatalf("בקריסה של 70%% חייבת להיות לפחות פעולת מכירה אחת (הציון ירד עד %d)", p.MinScore)
	}
	if p.MinScore > sellAt {
		t.Fatalf("הציון לא ירד מתחת לסף המכירה (%d) למרות הקריסה — המנוע לא מזהה מגמת ירידה", sellAt)
	}
	if p.Strategy <= p.BuyHold {
		t.Fatalf("בקריסה ההמלצות אמורות להפסיד פחות מקנה־והחזק: %.1f%% מול %.1f%%", p.Strategy, p.BuyHold)
	}
	if p.MaxDD >= p.HoldDD {
		t.Fatalf("היציאה למזומן אמורה להקטין את הירידה: %.0f%% מול %.0f%%", p.MaxDD, p.HoldDD)
	}
	t.Logf("קריסה: %d פעולות, בשוק %.0f%%, המלצות %.1f%% מול החזקה %.1f%%, ירידה %.0f%% מול %.0f%% ✔",
		p.Trades, p.TimeIn, p.Strategy, p.BuyHold, p.MaxDD, p.HoldDD)
}
