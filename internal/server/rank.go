package server

import (
	"net/http"
	"sort"
	"sync"
	"time"

	"stockcompass/internal/indicators"
	"stockcompass/internal/marketdata"
)

// הדירוג — האסטרטגיה היחידה שבאמת עבדה כשמדדנו אותה.
//
// למה דווקא זה, ולמה זה שונה מהציון: הציון של האפליקציה נכנס ויוצא ממניה בודדת לפי
// אינדיקטורים יומיים, ובמדידה על 63 מניות ובשני חלונות זמן נפרדים הוא הפסיד לקנה־והחזק
// כמעט תמיד. הדירוג עושה משהו אחר לגמרי: הוא *משווה* מניות זו לזו לפי מומנטום 12-1
// (התשואה מלפני שנה ועד לפני חודש) ומחזיק את החזקות ביותר. זה ניצח את הרף בשני החלונות.
//
// מתג השוק (SPY מעל/מתחת לממוצע 200 יום) לא מוסיף תשואה — הוא קונה ביטוח:
// במדידה הוא הוריד את הנפילה המקסימלית בערך בשליש.
//
// שים לב: המומנטום מחושב מנתונים בני 21 יום ומעלה, ולכן הדירוג *לא* משתנה במהלך היום.
// רק המחירים חיים.

// universe — 100 החברות הגדולות בארה"ב (מדד S&P 100). זה היקום שבתוכו מדרגים.
var universe = []string{
	"AAPL", "ABBV", "ABT", "ACN", "ADBE", "AIG", "AMD", "AMGN", "AMT", "AMZN",
	"AVGO", "AXP", "BA", "BAC", "BK", "BKNG", "BLK", "BMY", "BRK-B", "C",
	"CAT", "CHTR", "CL", "CMCSA", "COF", "COP", "COST", "CRM", "CSCO", "CVS",
	"CVX", "DE", "DHR", "DIS", "DOW", "DUK", "EMR", "EXC", "F", "FDX",
	"GD", "GE", "GILD", "GM", "GOOGL", "GS", "HD", "HON", "IBM", "INTC",
	"INTU", "JNJ", "JPM", "KHC", "KO", "LIN", "LLY", "LMT", "LOW", "MA",
	"MCD", "MDLZ", "MDT", "MET", "META", "MMM", "MO", "MRK", "MS", "MSFT",
	"NEE", "NFLX", "NKE", "NVDA", "ORCL", "PEP", "PFE", "PG", "PM", "PYPL",
	"QCOM", "RTX", "SBUX", "SCHW", "SO", "SPG", "T", "TGT", "TMO", "TMUS",
	"TSLA", "TXN", "UNH", "UNP", "UPS", "USB", "V", "VZ", "WFC", "WMT", "XOM",
}

const (
	holdN       = 10 // כמה מניות האסטרטגיה מחזיקה
	rankShow    = 25 // כמה מוצגות במסך (10 בפנים + 15 "על הספסל")
	momLookback = 252
	momSkip     = 21 // מדלגים על החודש האחרון — זה מה שהופך את זה ל-12-1
)

type rankItem struct {
	Rank      int     `json:"rank"`
	Symbol    string  `json:"symbol"`
	Name      string  `json:"name"`
	Mom       float64 `json:"mom"`  // מומנטום 12-1 (%)
	Year      float64 `json:"year"` // התשואה האמיתית בשנה (%)
	Price     float64 `json:"price"`
	ChangePct float64 `json:"changePct"`
	Hold      bool    `json:"hold"` // בתוך ה-10 שהאסטרטגיה מחזיקה
}

type rankResponse struct {
	Ready     bool       `json:"ready"`
	Done      int        `json:"done"`
	Total     int        `json:"total"`
	AsOf      string     `json:"asOf"`
	MarketOn  bool       `json:"marketOn"` // SPY מעל הממוצע → בשוק
	SPY       float64    `json:"spy"`
	SPYMA     float64    `json:"spyMA"`
	HoldN     int        `json:"holdN"`
	NextRebal string     `json:"nextRebal"`
	Items     []rankItem `json:"items"`
}

// warmUniverse — מביא ברקע את ההיסטוריה של כל היקום. בטפטוף, כדי לא להציף את הספק.
func (s *Server) warmUniverse() {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, sym := range append([]string{"SPY"}, universe...) {
		s.mu.Lock()
		h, ok := s.hist[sym]
		s.mu.Unlock()
		if ok && h.day == today() {
			continue
		}
		wg.Add(1)
		go func(sym string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s.fetchHistory(sym, marketdata.WaitBG)
		}(sym)
	}
	wg.Wait()
}

// rankLoop — מחזיק את היקום חם, ומרענן את המחירים החיים בזמן מסחר.
func (s *Server) rankLoop() {
	s.warmUniverse()
	s.refreshUniverseQuotes()

	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		s.warmUniverse() // אחרי חצות תיפול היסטוריה חדשה — זה מה שמביא אותה
		if marketHours() {
			s.refreshUniverseQuotes()
		}
	}
}

// refreshUniverseQuotes — מחירי כל היקום בשתי בקשות בלבד.
func (s *Server) refreshUniverseQuotes() {
	q := s.md.BatchQuotes(append([]string{"SPY"}, universe...))
	if len(q) == 0 {
		return
	}
	s.mu.Lock()
	for sym, v := range q {
		s.uq[sym] = v
	}
	s.uqAt = time.Now()
	s.mu.Unlock()
}

// mom121 — מומנטום 12-1: התשואה מלפני שנה ועד לפני חודש.
func mom121(cs []indicators.Candle) (float64, bool) {
	n := len(cs)
	if n < momLookback+momSkip+1 {
		return 0, false
	}
	from := cs[n-1-momLookback].Close
	to := cs[n-1-momSkip].Close
	if from <= 0 {
		return 0, false
	}
	return to/from - 1, true
}

func yearReturn(cs []indicators.Candle) float64 {
	n := len(cs)
	if n < momLookback+1 || cs[n-1-momLookback].Close <= 0 {
		return 0
	}
	return cs[n-1].Close/cs[n-1-momLookback].Close - 1
}

// nextQuarter — מתי האיזון הבא (האסטרטגיה מאזנת פעם ברבעון).
func nextQuarter() string {
	now := time.Now()
	q := (int(now.Month())-1)/3 + 1
	y := now.Year()
	m := time.Month(q*3 + 1)
	if q == 4 {
		m, y = time.January, y+1
	}
	months := [...]string{"ינואר", "אפריל", "יולי", "אוקטובר"}
	idx := (int(m) - 1) / 3
	return months[idx] + " " + itoa(y)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func (s *Server) handleRank(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	spyHist, haveSPY := s.hist["SPY"]
	quotes := make(map[string]marketdata.BQuote, len(s.uq))
	for k, v := range s.uq {
		quotes[k] = v
	}
	done := 0
	hists := make(map[string][]indicators.Candle, len(universe))
	names := make(map[string]string, len(universe))
	for _, sym := range universe {
		if h, ok := s.hist[sym]; ok && len(h.candles) > 0 {
			hists[sym] = h.candles
			done++
		}
		if n, ok := s.names[sym]; ok {
			names[sym] = n
		}
	}
	s.mu.Unlock()

	// עדיין נטען — עונים מיד עם התקדמות, בלי להשהות אף אחד
	if !haveSPY || done < len(universe)*8/10 {
		writeJSON(w, http.StatusOK, rankResponse{Ready: false, Done: done, Total: len(universe)})
		return
	}

	// מתג השוק: SPY מעל הממוצע של 200 יום?
	spyPrice := spyHist.candles[len(spyHist.candles)-1].Close
	if q, ok := quotes["SPY"]; ok && q.Price > 0 {
		spyPrice = q.Price
	}
	sum, cnt := 0.0, 0
	for i := len(spyHist.candles) - 1; i >= 0 && cnt < 200; i-- {
		sum += spyHist.candles[i].Close
		cnt++
	}
	spyMA := 0.0
	if cnt == 200 {
		spyMA = sum / 200
	}

	items := make([]rankItem, 0, len(universe))
	for _, sym := range universe {
		cs := hists[sym]
		m, ok := mom121(cs)
		if !ok {
			continue
		}
		it := rankItem{
			Symbol: sym, Name: names[sym], Mom: m * 100, Year: yearReturn(cs) * 100,
			Price: cs[len(cs)-1].Close,
		}
		if it.Name == "" {
			it.Name = sym
		}
		if len(cs) > 1 && cs[len(cs)-2].Close > 0 {
			it.ChangePct = (it.Price/cs[len(cs)-2].Close - 1) * 100
		}
		if q, ok := quotes[sym]; ok && q.Price > 0 {
			it.Price, it.ChangePct = q.Price, q.ChangePct
		}
		items = append(items, it)
	}
	sort.Slice(items, func(a, b int) bool { return items[a].Mom > items[b].Mom })
	for i := range items {
		items[i].Rank = i + 1
		items[i].Hold = i < holdN
	}
	if len(items) > rankShow {
		items = items[:rankShow]
	}

	writeJSON(w, http.StatusOK, rankResponse{
		Ready: true, Done: done, Total: len(universe),
		AsOf:     spyHist.candles[len(spyHist.candles)-1].Date,
		MarketOn: spyMA > 0 && spyPrice > spyMA, SPY: spyPrice, SPYMA: spyMA,
		HoldN: holdN, NextRebal: nextQuarter(), Items: items,
	})
}
