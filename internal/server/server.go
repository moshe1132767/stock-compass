// Package server — שרת ה-HTTP: מגיש את אתר הווב ואת ה-API לניתוח מניות.
package server

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"stockcompass/internal/config"
	"stockcompass/internal/indicators"
	"stockcompass/internal/marketdata"
)

type Server struct {
	mux *http.ServeMux
	cfg config.Config
	md  *marketdata.Client

	mu     sync.Mutex
	hist   map[string]cachedHistory // היסטוריה יומית לכל סימול
	quotes map[string]cachedQuote   // ציטוט חי, TTL קצר
	series map[string]cachedSeries  // סדרות גרף תוך-יומיות
	search map[string]cachedSearch  // תוצאות חיפוש
}

type cachedHistory struct {
	day     string // YYYY-MM-DD
	meta    marketdata.Meta
	candles []indicators.Candle
}
type cachedQuote struct {
	at time.Time
	q  marketdata.Quote
}
type cachedSeries struct {
	at  time.Time
	ttl time.Duration
	pts []marketdata.Point
}
type cachedSearch struct {
	at    time.Time
	items []marketdata.SearchItem
}

const quoteTTL = 15 * time.Second // מגן על מגבלת הבקשות (רענון אוטומטי כל 30ש')

func New(cfg config.Config) *Server {
	s := &Server{
		mux:    http.NewServeMux(),
		cfg:    cfg,
		md:     marketdata.New(cfg.TwelveDataKey, cfg.FinnhubKey),
		hist:   make(map[string]cachedHistory),
		quotes: make(map[string]cachedQuote),
		series: make(map[string]cachedSeries),
		search: make(map[string]cachedSearch),
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/healthz", s.handleHealth)
	s.mux.HandleFunc("/api/analyze", s.handleAnalyze)
	s.mux.HandleFunc("/api/search", s.handleSearch)
	s.mux.HandleFunc("/api/series", s.handleSeries)
	s.mux.Handle("/", http.FileServer(http.Dir(s.cfg.WebDir)))
}

func (s *Server) Start() error {
	addr := ":" + s.cfg.Port
	log.Printf("trade מאזין על %s (web=%s, twelvedata=%v, finnhub=%v)",
		addr, s.cfg.WebDir, s.md.HasKey(), s.cfg.FinnhubKey != "")
	return http.ListenAndServe(addr, s.logRequests(s.mux))
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			log.Printf("%s %s?%s (%s)", r.Method, r.URL.Path, r.URL.RawQuery, time.Since(start).Round(time.Millisecond))
		}
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// analyzeResponse — תוצאת האינדיקטורים + מידע שוק (סוג הנייר, האם השוק פתוח).
type analyzeResponse struct {
	indicators.Result
	Kind       string `json:"kind"` // index / stock
	Exchange   string `json:"exchange,omitempty"`
	MarketOpen bool   `json:"marketOpen"`
}

func (s *Server) handleAnalyze(w http.ResponseWriter, r *http.Request) {
	symbol := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("symbol")))
	if symbol == "" {
		writeJSON(w, http.StatusBadRequest, indicators.Result{OK: false, Reason: "חסר סימול מניה."})
		return
	}
	if !s.md.HasKey() {
		writeJSON(w, http.StatusServiceUnavailable, indicators.Result{OK: false,
			Reason: "השרת לא הוגדר עם מפתח נתונים (TWELVEDATA_API_KEY)."})
		return
	}

	candles, meta, err := s.getHistory(symbol)
	if err != nil {
		writeJSON(w, http.StatusOK, indicators.Result{OK: false, Symbol: symbol, Reason: err.Error()})
		return
	}

	// ברירת מחדל מהנרות היומיים
	prevClose := candles[len(candles)-1].Close
	if len(candles) > 1 {
		prevClose = candles[len(candles)-2].Close
	}
	name, marketOpen, exchange := symbol, false, meta.Exchange

	// ציטוט חי (מחיר עדכני + שם החברה + האם השוק פתוח) — נכשל בשקט אם יש מגבלת קצב
	if q, qerr := s.getQuote(symbol); qerr == nil {
		if q.Price > 0 {
			candles[len(candles)-1].Close = q.Price
		}
		if q.PrevClose > 0 {
			prevClose = q.PrevClose
		}
		if q.Name != "" {
			name = q.Name
		}
		if q.Exchange != "" {
			exchange = q.Exchange
		}
		marketOpen = q.MarketOpen
	} else if price, pc, ok := s.md.LivePrice(symbol); ok { // גיבוי: Finnhub (רשות)
		candles[len(candles)-1].Close = price
		if pc > 0 {
			prevClose = pc
		}
	}

	res := indicators.Analyze(candles)
	res.Symbol = symbol
	res.Name = name
	res.PrevClose = prevClose
	res.Change = res.Price - prevClose
	if prevClose != 0 {
		res.ChangePct = res.Change / prevClose * 100
	}
	writeJSON(w, http.StatusOK, analyzeResponse{
		Result: res, Kind: meta.Kind(), Exchange: exchange, MarketOpen: marketOpen,
	})
}

// handleSearch — השלמה אוטומטית של סימולים.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) < 1 || !s.md.HasKey() {
		writeJSON(w, http.StatusOK, []marketdata.SearchItem{})
		return
	}
	key := strings.ToLower(q)

	s.mu.Lock()
	if c, ok := s.search[key]; ok && time.Since(c.at) < 5*time.Minute {
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, c.items)
		return
	}
	s.mu.Unlock()

	items, err := s.md.Search(q)
	if err != nil {
		writeJSON(w, http.StatusOK, []marketdata.SearchItem{})
		return
	}
	s.mu.Lock()
	s.search[key] = cachedSearch{at: time.Now(), items: items}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, items)
}

// rangeSpec — טווח הגרף → אינטרוול, מספר נקודות, ו-TTL למטמון.
func rangeSpec(rng string) (interval string, size int, ttl time.Duration) {
	switch rng {
	case "1h":
		return "1min", 60, 60 * time.Second
	case "1w":
		return "1h", 40, 2 * time.Minute
	case "1m":
		return "1day", 23, 6 * time.Hour
	case "1y":
		return "1day", 252, 6 * time.Hour
	default: // 1d
		return "5min", 78, 60 * time.Second
	}
}

type seriesResponse struct {
	OK     bool               `json:"ok"`
	Symbol string             `json:"symbol"`
	Range  string             `json:"range"`
	Reason string             `json:"reason,omitempty"`
	Points []marketdata.Point `json:"points"`
}

// handleSeries — נקודות לגרף המחיר. 1m/1y נגזרים מההיסטוריה במטמון (בלי בקשה נוספת).
func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	symbol := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("symbol")))
	rng := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("range")))
	if rng == "" {
		rng = "1d"
	}
	if symbol == "" || !s.md.HasKey() {
		writeJSON(w, http.StatusOK, seriesResponse{OK: false, Symbol: symbol, Range: rng, Reason: "חסר סימול."})
		return
	}

	// 1m / 1y — מתוך הנרות היומיים שכבר במטמון: אפס בקשות נוספות
	if rng == "1m" || rng == "1y" {
		candles, _, err := s.getHistory(symbol)
		if err != nil {
			writeJSON(w, http.StatusOK, seriesResponse{OK: false, Symbol: symbol, Range: rng, Reason: err.Error()})
			return
		}
		n := 23
		if rng == "1y" {
			n = 252
		}
		if n > len(candles) {
			n = len(candles)
		}
		pts := make([]marketdata.Point, 0, n)
		for _, c := range candles[len(candles)-n:] {
			pts = append(pts, marketdata.Point{T: c.Date, C: c.Close})
		}
		writeJSON(w, http.StatusOK, seriesResponse{OK: true, Symbol: symbol, Range: rng, Points: pts})
		return
	}

	interval, size, ttl := rangeSpec(rng)
	key := symbol + "|" + rng

	s.mu.Lock()
	if c, ok := s.series[key]; ok && time.Since(c.at) < c.ttl {
		pts := c.pts
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, seriesResponse{OK: true, Symbol: symbol, Range: rng, Points: pts})
		return
	}
	s.mu.Unlock()

	pts, err := s.md.Series(symbol, interval, size)
	if err != nil {
		writeJSON(w, http.StatusOK, seriesResponse{OK: false, Symbol: symbol, Range: rng, Reason: err.Error()})
		return
	}
	s.mu.Lock()
	s.series[key] = cachedSeries{at: time.Now(), ttl: ttl, pts: pts}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, seriesResponse{OK: true, Symbol: symbol, Range: rng, Points: pts})
}

// getQuote — ציטוט חי עם מטמון קצר (מגן על מגבלת הקצב של ספק הנתונים).
func (s *Server) getQuote(symbol string) (marketdata.Quote, error) {
	s.mu.Lock()
	if c, ok := s.quotes[symbol]; ok && time.Since(c.at) < quoteTTL {
		s.mu.Unlock()
		return c.q, nil
	}
	s.mu.Unlock()

	q, err := s.md.Quote(symbol)
	if err != nil {
		return marketdata.Quote{}, err
	}
	s.mu.Lock()
	s.quotes[symbol] = cachedQuote{at: time.Now(), q: q}
	s.mu.Unlock()
	return q, nil
}

// getHistory — נרות יומיים עם מטמון יומי (משתנים פעם ביום).
func (s *Server) getHistory(symbol string) ([]indicators.Candle, marketdata.Meta, error) {
	today := time.Now().Format("2006-01-02")
	s.mu.Lock()
	if c, ok := s.hist[symbol]; ok && c.day == today {
		s.mu.Unlock()
		return cloneCandles(c.candles), c.meta, nil
	}
	s.mu.Unlock()

	candles, meta, err := s.md.History(symbol)
	if err != nil {
		return nil, marketdata.Meta{}, err
	}
	s.mu.Lock()
	s.hist[symbol] = cachedHistory{day: today, meta: meta, candles: candles}
	s.mu.Unlock()
	return cloneCandles(candles), meta, nil
}

// cloneCandles — עותק כדי שעדכון המחיר החי לא ישנה את המטמון.
func cloneCandles(in []indicators.Candle) []indicators.Candle {
	out := make([]indicators.Candle, len(in))
	copy(out, in)
	return out
}
