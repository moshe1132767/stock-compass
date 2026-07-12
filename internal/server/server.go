// Package server — שרת ה-HTTP: מגיש את אתר הווב, את ה-API, ואת הזרם החי (SSE).
package server

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"stockcompass/internal/backtest"
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
	hist   map[string]cachedHistory // היסטוריה יומית לכל סימול (עד ~20 שנה)
	deep   map[string]cachedHistory // כל ההיסטוריה שקיימת — לטווח "מקסימום"
	bt     map[string]cachedBT      // סימולציית עבר
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
type cachedBT struct {
	day string
	res backtest.Result
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
	quoteTTL      = 15 * time.Second       // מגן על מגבלת הבקשות של Twelve Data
	broadcastTick = 500 * time.Millisecond // עד 2 עדכונים חיים בשנייה — חלק, לא מציף
	maxDeepPages  = 4                      // עד כמה עמודים אחורה נדפדף בטווח "מקסימום"
	chartPoints   = 400                    // דילול נקודות הגרף — יותר מזה לא נראה על מסך טלפון
)

func New(cfg config.Config) *Server {
	s := &Server{
		mux:     http.NewServeMux(),
		cfg:     cfg,
		md:      marketdata.New(cfg.TwelveDataKey, cfg.FinnhubKey),
		feed:    livefeed.New(cfg.FinnhubKey),
		hist:    make(map[string]cachedHistory),
		deep:    make(map[string]cachedHistory),
		bt:      make(map[string]cachedBT),
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
	s.mux.HandleFunc("/api/backtest", s.handleBacktest)
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

	full, meta, err := s.historyRaw(symbol)
	if err != nil {
		writeJSON(w, http.StatusOK, indicators.Result{OK: false, Symbol: symbol, Reason: err.Error()})
		return
	}
	candles := tailCopy(full, indicators.Window) // עותק — כאן משנים את מחיר הסגירה האחרון
	s.feed.Subscribe(symbol)                     // מרגע שהסתכלת על מניה — היא נכנסת לזרם החי

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

// derivedDays — טווחים שנגזרים מההיסטוריה היומית שכבר במטמון (0 = הכול). אפס בקשות API נוספות.
var derivedDays = map[string]int{"1m": 23, "1y": 252, "5y": 1260, "max": 0}

// downsample — מדלל נקודות בלי לאבד את הראשונה והאחרונה.
func downsample(pts []marketdata.Point, max int) []marketdata.Point {
	if len(pts) <= max || max < 2 {
		return pts
	}
	step := float64(len(pts)-1) / float64(max-1)
	out := make([]marketdata.Point, 0, max)
	for i := 0; i < max; i++ {
		j := int(math.Round(float64(i) * step))
		if j > len(pts)-1 {
			j = len(pts) - 1
		}
		out = append(out, pts[j])
	}
	return out
}

// handleSeries — נקודות לגרף. חודש/שנה/5 שנים/מקסימום נגזרים מההיסטוריה במטמון (בלי בקשה נוספת).
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

	if days, derived := derivedDays[rng]; derived {
		var candles []indicators.Candle
		var err error
		if rng == "max" {
			candles, err = s.deepHistory(symbol) // כל מה שהספק נותן, גם אחורה בזמן
		} else {
			candles, _, err = s.historyRaw(symbol)
		}
		if err != nil {
			writeJSON(w, http.StatusOK, seriesResponse{OK: false, Symbol: symbol, Range: rng, Reason: err.Error()})
			return
		}
		if days > 0 && days < len(candles) {
			candles = candles[len(candles)-days:]
		}
		pts := make([]marketdata.Point, 0, len(candles))
		for _, c := range candles {
			pts = append(pts, marketdata.Point{T: c.Date, C: c.Close})
		}
		writeJSON(w, http.StatusOK, seriesResponse{OK: true, Symbol: symbol, Range: rng,
			Points: downsample(pts, chartPoints)})
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

// historyRaw — ההיסטוריה היומית מהמטמון (או מהספק). הפרוסה המוחזרת משותפת — לקריאה בלבד!
// מי שצריך לשנות נרות ישתמש ב-tailCopy.
func (s *Server) historyRaw(symbol string) ([]indicators.Candle, marketdata.Meta, error) {
	today := time.Now().Format("2006-01-02")
	s.mu.Lock()
	if c, ok := s.hist[symbol]; ok && c.day == today {
		s.mu.Unlock()
		return c.candles, c.meta, nil
	}
	s.mu.Unlock()

	candles, meta, err := s.md.History(symbol)
	if err != nil {
		return nil, marketdata.Meta{}, err
	}
	s.mu.Lock()
	s.hist[symbol] = cachedHistory{day: today, meta: meta, candles: candles}
	s.mu.Unlock()
	return candles, meta, nil
}

// deepHistory — כל ההיסטוריה שקיימת למניה, גם מעבר לעומק של בקשה אחת (טווח "מקסימום").
// מדפדף אחורה בעמודים; אם הספק לא נותן יותר — פשוט מסתפק במה שיש.
func (s *Server) deepHistory(symbol string) ([]indicators.Candle, error) {
	today := time.Now().Format("2006-01-02")
	s.mu.Lock()
	if c, ok := s.deep[symbol]; ok && c.day == today {
		s.mu.Unlock()
		return c.candles, nil
	}
	s.mu.Unlock()

	base, _, err := s.historyRaw(symbol)
	if err != nil {
		return nil, err
	}
	all := append([]indicators.Candle(nil), base...) // עותק — לא נוגעים במטמון

	// אם העמוד הראשון לא היה מלא — כבר יש לנו את כל ההיסטוריה
	for page := 0; page < maxDeepPages && len(all) >= marketdata.MaxCandles; page++ {
		oldest := all[0].Date
		older, err := s.md.HistoryBefore(symbol, dayOf(oldest))
		if err != nil {
			break
		}
		var fresh []indicators.Candle
		for _, c := range older {
			if c.Date < oldest { // תאריכי YYYY-MM-DD — השוואת מחרוזות תקינה
				fresh = append(fresh, c)
			}
		}
		if len(fresh) == 0 {
			break
		}
		all = append(fresh, all...)
		if len(older) < marketdata.MaxCandles { // העמוד לא היה מלא — הגענו לתחילת ההיסטוריה
			break
		}
	}

	s.mu.Lock()
	s.deep[symbol] = cachedHistory{day: today, candles: all}
	s.mu.Unlock()
	return all, nil
}

func dayOf(datetime string) string {
	if len(datetime) >= 10 {
		return datetime[:10]
	}
	return datetime
}

// ---------- סימולציית עבר: "מה היה קורה אם היית מקשיב" ----------

type backtestResponse struct {
	OK     bool   `json:"ok"`
	Symbol string `json:"symbol"`
	Reason string `json:"reason,omitempty"`
	backtest.Result
}

func (s *Server) handleBacktest(w http.ResponseWriter, r *http.Request) {
	symbol := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("symbol")))
	if symbol == "" || !s.md.HasKey() {
		writeJSON(w, http.StatusOK, backtestResponse{OK: false, Symbol: symbol, Reason: "חסר סימול."})
		return
	}

	today := time.Now().Format("2006-01-02")
	s.mu.Lock()
	if c, ok := s.bt[symbol]; ok && c.day == today {
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, backtestResponse{OK: true, Symbol: symbol, Result: c.res})
		return
	}
	s.mu.Unlock()

	candles, _, err := s.historyRaw(symbol)
	if err != nil {
		writeJSON(w, http.StatusOK, backtestResponse{OK: false, Symbol: symbol, Reason: err.Error()})
		return
	}

	res := backtest.Run(candles) // חישוב מקומי בלבד — אפס בקשות API
	s.mu.Lock()
	s.bt[symbol] = cachedBT{day: today, res: res}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, backtestResponse{OK: true, Symbol: symbol, Result: res})
}

// cachedCandles — עותק של חלון הנרות האחרון, מהמטמון בלבד (בלי לפנות ל-API) — ללולאת השידור.
func (s *Server) cachedCandles(symbol string) ([]indicators.Candle, bool) {
	today := time.Now().Format("2006-01-02")
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.hist[symbol]
	if !ok || c.day != today {
		return nil, false
	}
	return tailCopy(c.candles, indicators.Window), true
}

// tailCopy — עותק בר-שינוי של N הנרות האחרונים. המטמון עצמו לעולם לא נוגעים בו.
func tailCopy(in []indicators.Candle, n int) []indicators.Candle {
	if n > len(in) {
		n = len(in)
	}
	out := make([]indicators.Candle, n)
	copy(out, in[len(in)-n:])
	return out
}
