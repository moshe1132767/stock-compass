// Package marketdata מושך נתוני מניות ממקורות חיצוניים (Twelve Data + Finnhub).
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

// מכסת הספק בתוכנית החינמית: 8 קרדיטים לדקה. אנחנו עוצרים ב-7 כדי להשאיר אוויר.
const (
	rateMax    = 7
	rateWindow = time.Minute
	rateWait   = 25 * time.Second // כמה זמן בקשה מוכנה לחכות בתור לפני שהיא מוותרת
)

// ErrBusy — נגמרה המכסה לדקה הזו ולא נפנה מקום בזמן סביר.
var ErrBusy = errors.New("יותר מדי בקשות לספק הנתונים כרגע — נסה שוב בעוד רגע")

// limiter — בלם קצב: לא נשלח לספק יותר מ-max בקשות בכל חלון זמן.
// זה מה שמונע את השגיאה "נגמרו הקרדיטים לדקה הזו" — במקום להיכשל, מחכים בתור.
type limiter struct {
	mu   sync.Mutex
	hits []time.Time
}

func (l *limiter) take(maxWait time.Duration) error {
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
		if len(l.hits) < rateMax {
			l.hits = append(l.hits, now)
			l.mu.Unlock()
			return nil
		}
		sleep := rateWindow - now.Sub(l.hits[0]) + 100*time.Millisecond
		l.mu.Unlock()
		if time.Now().Add(sleep).After(deadline) {
			return ErrBusy
		}
		log.Printf("בלם קצב: המכסה מלאה — ממתין %s", sleep.Round(time.Second))
		time.Sleep(sleep)
	}
}

type Client struct {
	tdKey string
	fhKey string
	http  *http.Client
	lim   limiter
}

func New(twelveDataKey, finnhubKey string) *Client {
	return &Client{
		tdKey: twelveDataKey,
		fhKey: finnhubKey,
		http:  &http.Client{Timeout: 20 * time.Second},
	}
}

// HasKey — האם הוגדר מפתח Twelve Data (חובה לפעולה).
func (c *Client) HasKey() bool { return c.tdKey != "" }

func pf(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }

// get — כל פנייה לספק עוברת דרך בלם הקצב.
func (c *Client) get(u string, v any) error {
	if err := c.lim.take(rateWait); err != nil {
		return err
	}
	resp, err := c.http.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

// apiErr — הופך שגיאת מכסה של הספק להודעה ברורה בעברית.
func apiErr(code int, msg, fallback string) error {
	if code == 429 || strings.Contains(strings.ToLower(msg), "api credits") {
		return ErrBusy
	}
	if msg == "" {
		msg = fallback
	}
	return fmt.Errorf("%s", msg)
}

// Meta — מידע על הנייר (סוג, בורסה) כפי שמגיע מ-time_series.
type Meta struct {
	Symbol   string `json:"symbol"`
	Type     string `json:"type"`
	Exchange string `json:"exchange"`
	Currency string `json:"currency"`
}

// Kind — מסווג "index" (מדד/סל) מול "stock" (מניה), לצורך הלשוניות.
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

// MaxCandles — כמות הנרות המרבית שהספק מחזיר בבקשה אחת (~20 שנות מסחר). נבדק מול ה-API.
const MaxCandles = 5000

// History — נרות יומיים (oldest-first) + מידע על הנייר. מושך את מלוא העומק הזמין.
func (c *Client) History(symbol string) ([]indicators.Candle, Meta, error) {
	return c.history(symbol, "")
}

// HistoryBefore — עמוד היסטוריה נוסף שמסתיים בתאריך נתון (לשליפת עבר רחוק יותר, לטווח "מקסימום").
func (c *Client) HistoryBefore(symbol, endDate string) ([]indicators.Candle, error) {
	cs, _, err := c.history(symbol, endDate)
	return cs, err
}

func (c *Client) history(symbol, endDate string) ([]indicators.Candle, Meta, error) {
	u := fmt.Sprintf("https://api.twelvedata.com/time_series?symbol=%s&interval=1day&outputsize=%d&apikey=%s",
		url.QueryEscape(symbol), MaxCandles, url.QueryEscape(c.tdKey))
	if endDate != "" {
		u += "&end_date=" + url.QueryEscape(endDate)
	}
	var r tdSeriesResp
	if err := c.get(u, &r); err != nil {
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

// Series — סדרת מחירים לגרף (oldest-first), לפי אינטרוול וכמות נקודות.
func (c *Client) Series(symbol, interval string, outputsize int) ([]Point, error) {
	u := fmt.Sprintf("https://api.twelvedata.com/time_series?symbol=%s&interval=%s&outputsize=%d&apikey=%s",
		url.QueryEscape(symbol), url.QueryEscape(interval), outputsize, url.QueryEscape(c.tdKey))
	var r tdSeriesResp
	if err := c.get(u, &r); err != nil {
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

// Quote — מחיר חי, סגירה קודמת, שם החברה, והאם השוק פתוח כרגע.
type Quote struct {
	Name       string
	Price      float64
	PrevClose  float64
	MarketOpen bool
	Exchange   string
}

func (c *Client) Quote(symbol string) (Quote, error) {
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
	if err := c.get(u, &q); err != nil {
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

// SearchItem — תוצאת חיפוש סימול (להשלמה אוטומטית).
type SearchItem struct {
	Symbol   string `json:"symbol"`
	Name     string `json:"name"`
	Exchange string `json:"exchange"`
	Kind     string `json:"kind"` // index / stock
}

// Search — חיפוש סימולים, מסונן לארה"ב בלבד.
func (c *Client) Search(q string) ([]SearchItem, error) {
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
	if err := c.get(u, &r); err != nil {
		return nil, err
	}
	up := strings.ToUpper(strings.TrimSpace(q))
	out := make([]SearchItem, 0, 8)
	seen := map[string]bool{}
	for _, d := range r.Data {
		ex := strings.ToUpper(d.Exchange)
		// ארה"ב בלבד, בורסות ראשיות (בלי OTC/פינק־שיטס)
		if d.Country != "United States" || seen[d.Symbol] ||
			strings.Contains(ex, "OTC") || strings.Contains(ex, "PINK") {
			continue
		}
		seen[d.Symbol] = true
		it := SearchItem{
			Symbol: d.Symbol, Name: d.InstrumentName, Exchange: d.Exchange,
			Kind: Meta{Type: d.InstrumentType}.Kind(),
		}
		if strings.ToUpper(d.Symbol) == up { // התאמה מדויקת — ראשונה ברשימה
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

// LivePrice — מחיר חי מ-Finnhub (רשות). ok=false אם אין מפתח או תקלה.
func (c *Client) LivePrice(symbol string) (price, prevClose float64, ok bool) {
	if c.fhKey == "" {
		return 0, 0, false
	}
	u := fmt.Sprintf("https://finnhub.io/api/v1/quote?symbol=%s&token=%s",
		url.QueryEscape(symbol), url.QueryEscape(c.fhKey))
	var q struct {
		C  float64 `json:"c"`
		PC float64 `json:"pc"`
	}
	if err := c.get(u, &q); err != nil || q.C <= 0 {
		return 0, 0, false
	}
	return q.C, q.PC, true
}
