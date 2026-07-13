// Package marketdata מושך נתוני מניות ממקורות חיצוניים.
//
// חלוקת התפקידים, אחרי שהמכסה של Twelve Data חנקה את האפליקציה:
//   - נרות (היסטוריה + גרפים): Yahoo — בלי מפתח ובלי מכסה. Twelve Data רק כרשת ביטחון.
//   - מחיר חי, שמות חברות, חיפוש: Finnhub — 60 בקשות לדקה, נדיב.
//   - Twelve Data: 8 קרדיטים לדקה בלבד. נוגעים בו רק אם Yahoo נפל.
//
// הכלל שמנחה את כל הקובץ: בקשה של משתמש לעולם לא ממתינה בתור.
// מי שממתין זה רק רענון רקע — שאף אחד לא רואה.
package marketdata

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"stockcompass/internal/indicators"
)

const (
	rateMax    = 7 // המכסה של Twelve Data היא 8 לדקה — עוצרים ב-7 כדי להשאיר אוויר
	rateWindow = time.Minute
)

// כמה זמן מותר לבקשה להמתין בתור אצל הספק:
//
//	WaitUser — משתמש מחכה מול המסך. כמעט ולא ממתינים; עדיף להגיש נתון מהמטמון.
//	WaitBG   — רענון רקע. יכול להמתין בשקט, אף אחד לא מסתכל.
const (
	WaitUser = 1200 * time.Millisecond
	WaitBG   = 90 * time.Second
)

// ErrBusy — הספק חסום כרגע. אף פעם לא מגיע למשתמש כשיש נתון במטמון.
var ErrBusy = errors.New("ספק הנתונים עמוס כרגע")

// limiter — בלם קצב: לא שולחים לספק יותר מ-max בקשות בחלון זמן.
// reserve שומר מקומות פנויים לבקשות של משתמשים — רענוני רקע לא יגנבו להם את התור.
type limiter struct {
	mu   sync.Mutex
	max  int // 0 = ברירת המחדל של Twelve Data
	hits []time.Time
}

func (l *limiter) take(maxWait time.Duration, reserve int) error {
	quota := l.max
	if quota == 0 {
		quota = rateMax
	}
	if quota -= reserve; quota < 1 {
		quota = 1
	}
	deadline := time.Now().Add(maxWait)
	for {
		l.mu.Lock()
		now := time.Now()
		keep := l.hits[:0]
		for _, t := range l.hits {
			if now.Sub(t) < rateWindow {
				keep = append(keep, t)
			}
		}
		l.hits = keep
		if len(l.hits) < quota {
			l.hits = append(l.hits, now)
			l.mu.Unlock()
			return nil
		}
		sleep := rateWindow - now.Sub(l.hits[0]) + 100*time.Millisecond
		l.mu.Unlock()
		if time.Now().Add(sleep).After(deadline) {
			return ErrBusy // המשתמש לא ימתין — הקורא יגיש מהמטמון
		}
		time.Sleep(sleep)
	}
}

type Client struct {
	tdKey string
	fhKey string
	http  *http.Client
	y     *yahoo
	lim   limiter // Twelve Data — 8 לדקה, הצוואר הצר
	fhLim limiter // Finnhub — 60 לדקה
}

func New(twelveDataKey, finnhubKey string) *Client {
	c := &Client{
		tdKey: twelveDataKey,
		fhKey: finnhubKey,
		http:  &http.Client{Timeout: 15 * time.Second},
		y:     newYahoo(),
		fhLim: limiter{max: 55},
	}
	go c.y.prime() // מרימים את העוגייה כבר עכשיו — שהמשתמש הראשון לא ימתין לה
	return c
}

func (c *Client) HasKey() bool     { return c.tdKey != "" }
func (c *Client) HasFinnhub() bool { return c.fhKey != "" }

// YahooUp — האם מקור הנרות הראשי עובד (לתצוגת מצב ולוגים).
func (c *Client) YahooUp() bool { return c.y.up() }

// CandleSource — מי מספק את הנרות בפועל: yahoo (הרצוי) או twelvedata (הגיבוי).
func (c *Client) CandleSource() string { return c.y.source() }

func pf(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }

// bgReserve — רענון רקע (המתנה ארוכה) משאיר שני מקומות פנויים למשתמשים.
func bgReserve(wait time.Duration) int {
	if wait >= WaitBG {
		return 2
	}
	return 0
}

// get — פנייה ל-Twelve Data דרך הבלם הצר.
func (c *Client) get(u string, v any, wait time.Duration) error {
	if err := c.lim.take(wait, bgReserve(wait)); err != nil {
		return err
	}
	resp, err := c.http.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

// getFH — פנייה ל-Finnhub דרך הבלם הנדיב.
func (c *Client) getFH(u string, v any) error {
	if err := c.fhLim.take(WaitUser, 0); err != nil {
		return err
	}
	resp, err := c.http.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return ErrBusy
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func apiErr(code int, msg, fallback string) error {
	if code == 429 || strings.Contains(strings.ToLower(msg), "api credits") {
		return ErrBusy
	}
	if msg == "" {
		msg = fallback
	}
	return fmt.Errorf("%s", msg)
}

// Meta — מידע על הנייר.
type Meta struct {
	Symbol   string `json:"symbol"`
	Type     string `json:"type"`
	Exchange string `json:"exchange"`
	Currency string `json:"currency"`
	Name     string `json:"-"` // Yahoo מחזיר את שם החברה באותה קריאה — בחינם
	Full     bool   `json:"-"` // כל ההיסטוריה מיום ההנפקה כבר כאן
}

// Kind — מסווג "index" (מדד/סל) מול "stock".
func (m Meta) Kind() string {
	t := strings.ToLower(m.Type)
	if strings.Contains(t, "index") || strings.Contains(t, "etf") || strings.Contains(t, "fund") {
		return "index"
	}
	return "stock"
}

type tdSeriesResp struct {
	Meta   Meta `json:"meta"`
	Values []struct {
		Datetime string `json:"datetime"`
		Open     string `json:"open"`
		High     string `json:"high"`
		Low      string `json:"low"`
		Close    string `json:"close"`
		Volume   string `json:"volume"`
	} `json:"values"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Code    int    `json:"code"`
}

// MaxCandles — כמות הנרות המרבית ש-Twelve Data מחזיר בבקשה אחת.
const MaxCandles = 5000

// histYears — כמה שנות היסטוריה מספיקות למסך הרגיל:
// האינדיקטורים צריכים 300 ימי מסחר, והסימולציה הארוכה ביותר 5 שנים. 8 שנים מכסות הכל בנוחות,
// והמנה קטנה פי עשרה מהיסטוריה מלאה — כלומר תשובה מיידית במקום הורדה של מגה-בייטים.
const histYears = 8

// History — היסטוריה יומית לשימוש היומיומי (8 שנים אחרונות, oldest-first).
func (c *Client) History(symbol string, wait time.Duration) ([]indicators.Candle, Meta, error) {
	return c.history(symbol, time.Now().AddDate(-histYears, 0, 0), wait)
}

// HistoryAll — כל ההיסטוריה שקיימת, מיום ההנפקה (לגרף "מקסימום"). מנה כבדה — נמשכת ברקע.
func (c *Client) HistoryAll(symbol string, wait time.Duration) ([]indicators.Candle, Meta, error) {
	return c.history(symbol, time.Time{}, wait)
}

func (c *Client) history(symbol string, since time.Time, wait time.Duration) ([]indicators.Candle, Meta, error) {
	cs, m, err := c.y.daily(symbol, since)
	if err == nil && len(cs) > 30 {
		return cs, m, nil
	}
	if err != nil && c.tdKey == "" {
		return nil, Meta{}, err
	}
	if err != nil {
		log.Printf("yahoo/היסטוריה %s נכשל (%v) — עובר ל-Twelve Data", symbol, err)
	}
	return c.tdHistory(symbol, "", wait)
}

// HistoryBefore — עמוד היסטוריה נוסף אחורה בזמן (רק במסלול הגיבוי של Twelve Data).
func (c *Client) HistoryBefore(symbol, endDate string, wait time.Duration) ([]indicators.Candle, error) {
	cs, _, err := c.tdHistory(symbol, endDate, wait)
	return cs, err
}

func (c *Client) tdHistory(symbol, endDate string, wait time.Duration) ([]indicators.Candle, Meta, error) {
	if c.tdKey == "" {
		return nil, Meta{}, fmt.Errorf("אין נתונים עבור %s", symbol)
	}
	u := fmt.Sprintf("https://api.twelvedata.com/time_series?symbol=%s&interval=1day&outputsize=%d&apikey=%s",
		url.QueryEscape(symbol), MaxCandles, url.QueryEscape(c.tdKey))
	if endDate != "" {
		u += "&end_date=" + url.QueryEscape(endDate)
	}
	var r tdSeriesResp
	if err := c.get(u, &r, wait); err != nil {
		return nil, Meta{}, err
	}
	if r.Status == "error" || len(r.Values) == 0 {
		return nil, Meta{}, apiErr(r.Code, r.Message, "לא נמצאו נתונים עבור "+symbol)
	}
	candles := make([]indicators.Candle, 0, len(r.Values))
	for i := len(r.Values) - 1; i >= 0; i-- { // Twelve Data מחזיר newest-first
		v := r.Values[i]
		candles = append(candles, indicators.Candle{
			Date: v.Datetime, Open: pf(v.Open), High: pf(v.High),
			Low: pf(v.Low), Close: pf(v.Close), Volume: pf(v.Volume),
		})
	}
	return candles, r.Meta, nil
}

// Point — נקודה בגרף המחיר.
type Point struct {
	T string  `json:"t"`
	C float64 `json:"c"`
}

// intradaySpec — איך כל טווח נראה אצל כל ספק.
var intradaySpec = map[string]struct {
	yRange, yInterval string
	tdInterval        string
	tdSize            int
	keepLast          int // כמה נקודות אחרונות להשאיר (0 = הכל)
}{
	"1h": {"1d", "1m", "1min", 60, 60},
	"1d": {"1d", "5m", "5min", 78, 0},
	"5d": {"5d", "15m", "1h", 40, 0},
}

// Intraday — נקודות לגרף תוך-יומי. Yahoo קודם, Twelve Data כגיבוי.
func (c *Client) Intraday(symbol, rng string, wait time.Duration) ([]Point, error) {
	spec, ok := intradaySpec[rng]
	if !ok {
		spec = intradaySpec["1d"]
	}

	cs, _, err := c.y.chart(symbol, "range="+spec.yRange+"&interval="+spec.yInterval, false)
	if err == nil && len(cs) > 1 {
		if spec.keepLast > 0 && len(cs) > spec.keepLast {
			cs = cs[len(cs)-spec.keepLast:]
		}
		pts := make([]Point, 0, len(cs))
		for _, k := range cs {
			pts = append(pts, Point{T: k.Date, C: k.Close})
		}
		return pts, nil
	}
	if c.tdKey == "" {
		if err == nil {
			err = fmt.Errorf("אין נתוני גרף עבור %s", symbol)
		}
		return nil, err
	}
	return c.tdSeries(symbol, spec.tdInterval, spec.tdSize, wait)
}

func (c *Client) tdSeries(symbol, interval string, outputsize int, wait time.Duration) ([]Point, error) {
	u := fmt.Sprintf("https://api.twelvedata.com/time_series?symbol=%s&interval=%s&outputsize=%d&apikey=%s",
		url.QueryEscape(symbol), url.QueryEscape(interval), outputsize, url.QueryEscape(c.tdKey))
	var r tdSeriesResp
	if err := c.get(u, &r, wait); err != nil {
		return nil, err
	}
	if r.Status == "error" || len(r.Values) == 0 {
		return nil, apiErr(r.Code, r.Message, "אין נתוני גרף עבור "+symbol)
	}
	pts := make([]Point, 0, len(r.Values))
	for i := len(r.Values) - 1; i >= 0; i-- {
		pts = append(pts, Point{T: r.Values[i].Datetime, C: pf(r.Values[i].Close)})
	}
	return pts, nil
}

// Quote — ציטוט Twelve Data (גיבוי בלבד; המחיר החי מגיע מ-Finnhub).
type Quote struct {
	Name       string
	Price      float64
	PrevClose  float64
	MarketOpen bool
	Exchange   string
}

func (c *Client) Quote(symbol string) (Quote, error) {
	if c.tdKey == "" {
		return Quote{}, fmt.Errorf("אין ציטוט עבור %s", symbol)
	}
	u := fmt.Sprintf("https://api.twelvedata.com/quote?symbol=%s&apikey=%s",
		url.QueryEscape(symbol), url.QueryEscape(c.tdKey))
	var q struct {
		Name          string `json:"name"`
		Exchange      string `json:"exchange"`
		Close         string `json:"close"`
		PreviousClose string `json:"previous_close"`
		IsMarketOpen  bool   `json:"is_market_open"`
		Status        string `json:"status"`
		Message       string `json:"message"`
		Code          int    `json:"code"`
	}
	if err := c.get(u, &q, WaitUser); err != nil {
		return Quote{}, err
	}
	if q.Status == "error" || q.Close == "" {
		return Quote{}, apiErr(q.Code, q.Message, "אין ציטוט עבור "+symbol)
	}
	return Quote{
		Name: q.Name, Price: pf(q.Close), PrevClose: pf(q.PreviousClose),
		MarketOpen: q.IsMarketOpen, Exchange: q.Exchange,
	}, nil
}

// SearchItem — תוצאת חיפוש סימול.
type SearchItem struct {
	Symbol   string `json:"symbol"`
	Name     string `json:"name"`
	Exchange string `json:"exchange"`
	Kind     string `json:"kind"`
}

// Search — חיפוש סימולים בארה"ב. Finnhub קודם (מכסה נדיבה), Twelve Data כגיבוי.
func (c *Client) Search(q string) ([]SearchItem, error) {
	if c.fhKey != "" {
		if items, err := c.searchFH(q); err == nil {
			return items, nil
		}
	}
	return c.searchTD(q)
}

func (c *Client) searchFH(q string) ([]SearchItem, error) {
	u := fmt.Sprintf("https://finnhub.io/api/v1/search?q=%s&exchange=US&token=%s",
		url.QueryEscape(q), url.QueryEscape(c.fhKey))
	var r struct {
		Result []struct {
			Symbol      string `json:"symbol"`
			Description string `json:"description"`
			Type        string `json:"type"`
		} `json:"result"`
	}
	if err := c.getFH(u, &r); err != nil {
		return nil, err
	}
	up := strings.ToUpper(strings.TrimSpace(q))
	out := make([]SearchItem, 0, 8)
	seen := map[string]bool{}
	for _, d := range r.Result {
		if d.Symbol == "" || seen[d.Symbol] {
			continue
		}
		seen[d.Symbol] = true
		t := strings.ToUpper(d.Type)
		kind := "stock"
		if strings.Contains(t, "ETP") || strings.Contains(t, "ETF") ||
			strings.Contains(t, "INDEX") || strings.Contains(t, "FUND") {
			kind = "index"
		}
		it := SearchItem{Symbol: d.Symbol, Name: d.Description, Kind: kind}
		if strings.ToUpper(d.Symbol) == up {
			out = append([]SearchItem{it}, out...)
		} else {
			out = append(out, it)
		}
	}
	if len(out) > 8 {
		out = out[:8]
	}
	return out, nil
}

func (c *Client) searchTD(q string) ([]SearchItem, error) {
	if c.tdKey == "" {
		return []SearchItem{}, nil
	}
	u := fmt.Sprintf("https://api.twelvedata.com/symbol_search?symbol=%s&outputsize=30&apikey=%s",
		url.QueryEscape(q), url.QueryEscape(c.tdKey))
	var r struct {
		Data []struct {
			Symbol         string `json:"symbol"`
			InstrumentName string `json:"instrument_name"`
			Exchange       string `json:"exchange"`
			InstrumentType string `json:"instrument_type"`
			Country        string `json:"country"`
		} `json:"data"`
	}
	if err := c.get(u, &r, WaitUser); err != nil {
		return nil, err
	}
	up := strings.ToUpper(strings.TrimSpace(q))
	out := make([]SearchItem, 0, 8)
	seen := map[string]bool{}
	for _, d := range r.Data {
		ex := strings.ToUpper(d.Exchange)
		if d.Country != "United States" || seen[d.Symbol] ||
			strings.Contains(ex, "OTC") || strings.Contains(ex, "PINK") {
			continue
		}
		seen[d.Symbol] = true
		it := SearchItem{
			Symbol: d.Symbol, Name: d.InstrumentName, Exchange: d.Exchange,
			Kind: Meta{Type: d.InstrumentType}.Kind(),
		}
		if strings.ToUpper(d.Symbol) == up {
			out = append([]SearchItem{it}, out...)
		} else {
			out = append(out, it)
		}
	}
	if len(out) > 8 {
		out = out[:8]
	}
	return out, nil
}

// FHQuote — ציטוט Finnhub: מחיר, סגירה קודמת, וזמן העסקה האחרונה
// (זה מה שמגלה אם השוק באמת נסחר עכשיו — גם בחגים שהשעון לא מכיר).
type FHQuote struct {
	Price     float64
	PrevClose float64
	LastTrade time.Time
}

func (c *Client) FinnhubQuote(symbol string) (FHQuote, error) {
	if c.fhKey == "" {
		return FHQuote{}, fmt.Errorf("אין מפתח Finnhub")
	}
	u := fmt.Sprintf("https://finnhub.io/api/v1/quote?symbol=%s&token=%s",
		url.QueryEscape(symbol), url.QueryEscape(c.fhKey))
	var q struct {
		C  float64 `json:"c"`
		PC float64 `json:"pc"`
		T  int64   `json:"t"`
	}
	if err := c.getFH(u, &q); err != nil {
		return FHQuote{}, err
	}
	if q.C <= 0 {
		return FHQuote{}, fmt.Errorf("אין ציטוט עבור %s", symbol)
	}
	fq := FHQuote{Price: q.C, PrevClose: q.PC}
	if q.T > 0 {
		fq.LastTrade = time.Unix(q.T, 0)
	}
	return fq, nil
}

// BQuote — מחיר בחבילה (לדירוג של יקום שלם).
type BQuote struct {
	Price     float64
	ChangePct float64
}

// BatchQuotes — מחירים חיים לעשרות מניות בבקשה אחת. זה מה שמאפשר להציג יקום שלם
// בזמן אמת בלי לשרוף מכסה: 100 מניות = שתי בקשות, במקום 100.
func (c *Client) BatchQuotes(syms []string) map[string]BQuote {
	out := make(map[string]BQuote, len(syms))
	const chunk = 50
	for i := 0; i < len(syms); i += chunk {
		end := i + chunk
		if end > len(syms) {
			end = len(syms)
		}
		u := "https://query2.finance.yahoo.com/v7/finance/quote?symbols=" +
			url.QueryEscape(strings.Join(syms[i:end], ","))
		if cr := c.y.crumbNow(); cr != "" {
			u += "&crumb=" + url.QueryEscape(cr)
		}
		resp, err := c.y.do(u)
		if err != nil {
			continue // נסתפק במה שיש; המחירים יתעדכנו בסבב הבא
		}
		var r struct {
			QuoteResponse struct {
				Result []struct {
					Symbol                     string  `json:"symbol"`
					RegularMarketPrice         float64 `json:"regularMarketPrice"`
					RegularMarketChangePercent float64 `json:"regularMarketChangePercent"`
				} `json:"result"`
			} `json:"quoteResponse"`
		}
		err = json.NewDecoder(resp.Body).Decode(&r)
		resp.Body.Close()
		if err != nil {
			continue
		}
		for _, q := range r.QuoteResponse.Result {
			if q.RegularMarketPrice > 0 {
				out[q.Symbol] = BQuote{Price: q.RegularMarketPrice, ChangePct: q.RegularMarketChangePercent}
			}
		}
	}
	return out
}

// CompanyName — שם החברה. Finnhub קודם; ואם אין לו פרופיל (קרנות/מדדים) — Twelve Data, פעם אחת.
// ברוב המקרים לא נגיע לכאן בכלל: Yahoo כבר החזיר את השם יחד עם ההיסטוריה.
func (c *Client) CompanyName(symbol string) (string, error) {
	if c.fhKey != "" {
		u := fmt.Sprintf("https://finnhub.io/api/v1/stock/profile2?symbol=%s&token=%s",
			url.QueryEscape(symbol), url.QueryEscape(c.fhKey))
		var p struct {
			Name string `json:"name"`
		}
		if err := c.getFH(u, &p); err == nil && p.Name != "" {
			return p.Name, nil
		}
	}
	if c.tdKey != "" {
		if q, err := c.Quote(symbol); err == nil && q.Name != "" {
			return q.Name, nil
		}
	}
	return "", fmt.Errorf("לא נמצא שם עבור %s", symbol)
}
