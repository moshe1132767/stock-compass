// Package server — שרת ה-HTTP: מגיש את אתר הווב, את ה-API, ואת הזרם החי (SSE).
package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"stockcompass/internal/config"
	"stockcompass/internal/indicators"
	"stockcompass/internal/livefeed"
	"stockcompass/internal/marketdata"
)

type Server struct {
	mux  *http.ServeMux
	cfg  config.Config
	md   *marketdata.Client
	feed *livefeed.Feed

	mu     sync.Mutex
	hist   map[string]cachedHistory // היסטוריה יומית לכל סימול
	quotes map[string]cachedQuote   // ציטוט חי, TTL קצר
	series map[string]cachedSeries  // סדרות גרף תוך-יומיות
	search map[string]cachedSearch  // תוצאות חיפוש

	cmu     sync.Mutex
	clients map[*sseClient]bool // לקוחות מחוברים לזרם החי
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
type sseClient struct {
	ch   chan []byte
	syms map[string]bool
}

const (
	quoteTTL      = 15 * time.Second        // מגן על מגבלת הבקשות של Twelve Data
	broadcastTick = 500 * time.Millisecond  // עד 2 עדכונים חיים בשנייה — חלק, לא מציף
)

func New(cfg config.Config) *Server {
	s := &Server{
		mux:     http.NewServeMux(),
		cfg:     cfg,
		md:      marketdata.New(cfg.TwelveDataKey, cfg.FinnhubKey),
		feed:    livefeed.New(cfg.FinnhubKey),
		hist:    make(map[string]cachedHistory),
		quotes:  make(map[string]cachedQuote),
		series:  make(map[string]cachedSeries),
		search:  make(map[string]cachedSearch),
		clients: make(map[*sseClient]bool),
	}
	s.routes()
	s.feed.Start()
	go s.broadcastLoop()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/healthz", s.handleHealth)
	s.mux.HandleFunc("/api/analyze", s.handleAnalyze)
	s.mux.HandleFunc("/api/search", s.handleSearch)
	s.mux.HandleFunc("/api/series", s.handleSeries)
	s.mux.HandleFunc("/api/stream", s.handleStream)
	s.mux.Handle("/", http.FileServer(http.Dir(s.cfg.WebDir)))
}

func (s *Server) Start() error {
	addr := ":" + s.cfg.Port
	log.Printf("trade מאזין על %s (web=%s, twelvedata=%v, זרם חי=%v)",
		addr, s.cfg.WebDir, s.md.HasKey(), s.feed.Enabled())
	return http.ListenAndServe(addr, s.logRequests(s.mux))
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/stream" {
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

// ---------- ניתוח ----------

type analyzeResponse struct {
	indicators.Result
	Kind       string `json:"kind"` // index / stock
	Exchange   string `json:"exchange,omitempty"`
	MarketOpen bool   `json:"marketOpen"`
	Live       bool   `json:"live"` // האם הזרם החי מחובר
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
	s.feed.Subscribe(symbol) // מרגע שהסתכלת על מניה — היא נכנסת לזרם החי

	prevClose := candles[len(candles)-1].Close
	if len(candles) > 1 {
		prevClose = candles[len(candles)-2].Close
	}
	name, marketOpen, exchange := symbol, false, meta.Exchange

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
	}
	// המחיר מהזרם החי הוא הטרי ביותר — גובר על הציטוט
	if lp, ok := s.feed.Price(symbol); ok && lp > 0 {
		candles[len(candles)-1].Close = lp
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
		Result: res, Kind: meta.Kind(), Exchange: exchange,
		MarketOpen: marketOpen, Live: s.feed.Connected(),
	})
}

// ---------- הזרם החי (SSE) ----------

type liveUpdate struct {
	Symbol         string                `json:"symbol"`
	Price          float64               `json:"price"`
	PrevClose      float64               `json:"prevClose,omitempty"`
	Change         float64               `json:"change"`
	ChangePct      float64               `json:"changePct"`
	Score100       int                   `json:"score100,omitempty"`
	Recommendation string                `json:"recommendation,omitempty"`
	RecoKey        string                `json:"recoKey,omitempty"`
	Agreement      *indicators.Agreement `json:"agreement,omitempty"`
}

// handleStream — ערוץ SSE: דוחף לדפדפן כל שינוי מחיר ברגע שהוא קורה.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	if !s.feed.Enabled() {
		http.Error(w, "live feed disabled", http.StatusServiceUnavailable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	syms := make(map[string]bool)
	for _, p := range strings.Split(r.URL.Query().Get("symbols"), ",") {
		if p = strings.ToUpper(strings.TrimSpace(p)); p != "" {
			syms[p] = true
			s.feed.Subscribe(p)
		}
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // בלי באפרינג בפרוקסי

	cl := &sseClient{ch: make(chan []byte, 32), syms: syms}
	s.cmu.Lock()
	s.clients[cl] = true
	s.cmu.Unlock()
	defer func() {
		s.cmu.Lock()
		delete(s.clients, cl)
		s.cmu.Unlock()
	}()

	fmt.Fprint(w, "retry: 3000\n: connected\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-cl.ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// broadcastLoop — כל חצי שנייה: לוקח את המחירים שהשתנו ודוחף אותם ללקוחות.
func (s *Server) broadcastLoop() {
	t := time.NewTicker(broadcastTick)
	defer t.Stop()
	for range t.C {
		dirty := s.feed.TakeDirty()
		if len(dirty) == 0 {
			continue
		}
		for sym, price := range dirty {
			b, err := json.Marshal(s.livePayload(sym, price))
			if err != nil {
				continue
			}
			s.cmu.Lock()
			for cl := range s.clients {
				if !cl.syms[sym] {
					continue
				}
				select {
				case cl.ch <- b:
				default: // לקוח איטי — מדלגים במקום לחסום
				}
			}
			s.cmu.Unlock()
		}
	}
}

// livePayload — עדכון חי. אם יש נרות במטמון, מחשב מחדש גם את הציון — חישוב מקומי בלבד, בלי בקשות API.
func (s *Server) livePayload(sym string, price float64) liveUpdate {
	u := liveUpdate{Symbol: sym, Price: price}

	candles, ok := s.cachedCandles(sym)
	if !ok || len(candles) < 2 {
		return u // אין נרות במטמון — שולחים מחיר בלבד
	}
	prev := candles[len(candles)-2].Close
	if q, ok := s.cachedQuote(sym); ok && q.PrevClose > 0 {
		prev = q.PrevClose
	}
	candles[len(candles)-1].Close = price

	res := indicators.Analyze(candles)
	ag := res.Agreement
	u.PrevClose = prev
	u.Change = price - prev
	if prev != 0 {
		u.ChangePct = u.Change / prev * 100
	}
	u.Score100 = res.Score100
	u.Recommendation = res.Recommendation
	u.RecoKey = res.RecoKey
	u.Agreement = &ag
	return u
}

// ---------- חיפוש ----------

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

// ---------- גרף ----------

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

// handleSeries — נקודות לגרף. 1m/1y נגזרים מההיסטוריה במטמון (בלי בקשה נוספת).
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

// ---------- מטמונים ----------

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

func (s *Server) cachedQuote(symbol string) (marketdata.Quote, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.quotes[symbol]
	return c.q, ok
}

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

// cachedCandles — קריאה מהמטמון בלבד (בלי לפנות ל-API) — לשימוש בלולאת השידור.
func (s *Server) cachedCandles(symbol string) ([]indicators.Candle, bool) {
	today := time.Now().Format("2006-01-02")
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.hist[symbol]
	if !ok || c.day != today {
		return nil, false
	}
	return cloneCandles(c.candles), true
}

func cloneCandles(in []indicators.Candle) []indicators.Candle {
	out := make([]indicators.Candle, len(in))
	copy(out, in)
	return out
}
