package server

import (
	"testing"

	"stockcompass/internal/indicators"
)

// כשהשוק נפתח ולספק עדיין אין נר להיום — אסור לדרוס את הסגירה של אתמול במחיר של היום.
// יום מסחר שלם היה נמחק מההיסטוריה, וכל האינדיקטורים היו מזייפים לכמה דקות.
func TestLivePriceNeverEatsYesterday(t *testing.T) {
	cs := []indicators.Candle{
		{Date: "2026-07-09", Open: 95, High: 101, Low: 94, Close: 100, Volume: 1e6},
		{Date: "2026-07-10", Open: 100, High: 112, Low: 99, Close: 110, Volume: 1e6}, // אתמול
	}
	out := applyLive(cs, 120, true) // השוק פתוח, המחיר החי 120, ואין נר להיום

	if len(out) != 3 {
		t.Fatalf("היה צריך להיווסף נר חדש להיום, יש %d נרות", len(out))
	}
	if out[1].Close != 110 {
		t.Fatalf("הסגירה של אתמול נדרסה: %.2f במקום 110", out[1].Close)
	}
	if out[2].Close != 120 || out[2].Date != nyToday() {
		t.Fatalf("הנר של היום שגוי: %+v", out[2])
	}
}

// אם כבר יש נר להיום — מעדכנים אותו, כולל השיא והשפל אם המחיר חרג מהם.
func TestLivePriceUpdatesTodaysCandle(t *testing.T) {
	cs := []indicators.Candle{
		{Date: "2026-07-10", Open: 100, High: 105, Low: 99, Close: 104, Volume: 1e6},
		{Date: nyToday(), Open: 104, High: 106, Low: 103, Close: 105, Volume: 5e5},
	}
	out := applyLive(cs, 108, true)

	if len(out) != 2 {
		t.Fatalf("לא היה צריך להוסיף נר — כבר יש נר להיום (יש %d)", len(out))
	}
	if out[1].Close != 108 {
		t.Fatalf("הסגירה לא עודכנה למחיר החי: %.2f", out[1].Close)
	}
	if out[1].High != 108 {
		t.Fatalf("המחיר עבר את השיא של היום — השיא היה צריך להתעדכן ל-108, נשאר %.2f", out[1].High)
	}
}

// שוק סגור: אף פעם לא ממציאים נר חדש.
func TestClosedMarketNeverAddsCandle(t *testing.T) {
	cs := []indicators.Candle{
		{Date: "2026-07-09", Close: 100, High: 101, Low: 99},
		{Date: "2026-07-10", Close: 110, High: 111, Low: 109},
	}
	if out := applyLive(cs, 110, false); len(out) != 2 {
		t.Fatalf("כשהשוק סגור אסור להוסיף נר, נוצרו %d נרות", len(out))
	}
}
