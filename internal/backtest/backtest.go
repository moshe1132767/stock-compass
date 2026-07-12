// Package backtest מריץ את מנוע האינדיקטורים על כל יום בעבר ומחשב
// מה היה קורה למי שהיה פועל לפי ההמלצות, מול מי שפשוט קנה והחזיק.
//
// הכלל: בכל יום מריצים את אותם 8 אינדיקטורים על אותו חלון נרות שהאפליקציה
// מציגה היום (indicators.Window), ומקבלים את אותו ציון 0–100 בדיוק.
//   ציון >= 55  ("נטייה לקנייה" ומעלה) → מחזיקים את המניה למחרת.
//   ציון <= 45  ("נטייה למכירה" ומטה)  → יוצאים למזומן.
//   באמצע ("החזקה/המתנה")              → נשארים כמו שהיינו.
// הפעולה מתבצעת בסגירת יום המסחר שאחרי האות — בלי הצצה לעתיד.
package backtest

import (
	"math"

	"stockcompass/internal/indicators"
)

const (
	buyAt    = 55    // ממנו ומעלה — נכנסים
	sellAt   = 45    // ממנו ומטה  — יוצאים
	StartCap = 10000 // סכום פתיחה לדוגמה ($)
)

// Period — תוצאת תקופה אחת.
type Period struct {
	Key       string  `json:"key"`
	Label     string  `json:"label"`
	Days      int     `json:"days"`
	Available bool    `json:"available"`
	From      string  `json:"from,omitempty"`
	To        string  `json:"to,omitempty"`
	Strategy  float64 `json:"strategy"` // תשואת ההמלצות ב-%
	BuyHold   float64 `json:"buyHold"`  // תשואת קנה־והחזק ב-%
	StratVal  float64 `json:"stratVal"` // מה היו הופכים 10,000$
	HoldVal   float64 `json:"holdVal"`
	Trades    int     `json:"trades"`  // כמה פעולות קנייה/מכירה
	TimeIn    float64 `json:"timeIn"`  // אחוז הימים שבהם היינו בתוך השוק
	MaxDD     float64 `json:"maxDD"`   // הירידה הכי גדולה בדרך (%)
	HoldDD    float64 `json:"holdDD"`  // אותו דבר לקנה־והחזק
}

// Result — כל התקופות יחד.
type Result struct {
	Start   float64  `json:"start"`
	Periods []Period `json:"periods"`
}

var defs = []struct {
	Key, Label string
	Days       int
}{
	{"1w", "שבוע", 5},
	{"1m", "חודש", 21},
	{"6m", "חצי שנה", 126},
	{"1y", "שנה", 252},
	{"5y", "5 שנים", 1260},
}

// Run — מריץ את הסימולציה על היסטוריית נרות יומית (oldest-first).
func Run(candles []indicators.Candle) Result {
	n := len(candles)
	res := Result{Start: StartCap, Periods: make([]Period, 0, len(defs))}

	longest := 0
	for _, d := range defs {
		if d.Days > longest {
			longest = d.Days
		}
	}

	// lo — היום הראשון שעבורו נחשב ציון. צריך חלון נרות מלא לפניו,
	// ועוד יום אחד לפני תחילת התקופה הארוכה (כדי לדעת את הפוזיציה ביום הראשון שלה).
	lo := n - 2 - longest
	if lo < indicators.Window-1 {
		lo = indicators.Window - 1
	}
	if n < indicators.Window+2 { // אין אפילו חלון אחד — אין מה לחשב
		for _, d := range defs {
			res.Periods = append(res.Periods, Period{Key: d.Key, Label: d.Label, Days: d.Days})
		}
		return res
	}

	// הציון שהאפליקציה הייתה מציגה בסוף כל יום
	score := make([]int, n)
	for i := lo; i < n; i++ {
		score[i] = indicators.Analyze(candles[i-indicators.Window+1 : i+1]).Score100
	}

	// pos[i] — האם החזקנו את המניה ביום i (הוחלט לפי הציון בסגירת i-1)
	pos := make([]int, n+1)
	cur := 0
	for i := lo; i < n; i++ {
		switch {
		case score[i] >= buyAt:
			cur = 1
		case score[i] <= sellAt:
			cur = 0
		}
		pos[i+1] = cur
	}

	for _, d := range defs {
		p := Period{Key: d.Key, Label: d.Label, Days: d.Days}
		s := n - d.Days // היום הראשון בתקופה
		if s-1 < lo+1 { // אין מספיק היסטוריה כדי לדעת מה היה הציון אז
			res.Periods = append(res.Periods, p)
			continue
		}

		eq, hold := float64(StartCap), float64(StartCap) // שניהם מתחילים בסגירת היום שלפני התקופה
		peakEq, peakHold := eq, hold
		var ddEq, ddHold float64
		trades, inDays := 0, 0

		for i := s; i < n; i++ {
			r := candles[i].Close/candles[i-1].Close - 1
			if pos[i] == 1 {
				eq *= 1 + r
				inDays++
			}
			hold *= 1 + r
			if pos[i] != pos[i-1] {
				trades++
			}
			peakEq = math.Max(peakEq, eq)
			peakHold = math.Max(peakHold, hold)
			ddEq = math.Max(ddEq, (peakEq-eq)/peakEq)
			ddHold = math.Max(ddHold, (peakHold-hold)/peakHold)
		}

		p.Available = true
		p.From = candles[s].Date
		p.To = candles[n-1].Date
		p.Strategy = (eq/StartCap - 1) * 100
		p.BuyHold = (hold/StartCap - 1) * 100
		p.StratVal = eq
		p.HoldVal = hold
		p.Trades = trades
		p.TimeIn = float64(inDays) / float64(d.Days) * 100
		p.MaxDD = ddEq * 100
		p.HoldDD = ddHold * 100
		res.Periods = append(res.Periods, p)
	}
	return res
}
