// Package server — שרת ה-HTTP: מגיש את אתר הווב, את ה-API, ואת הזרם החי (SSE).
//
// כלל הברזל של הקובץ הזה: בקשה של משתמש לעולם לא ממתינה לספק חיצוני.
// מה שיש במטמון מוגש מיד (גם אם הוא קצת ישן), והרענון קורה ברקע.
// רק בפעם הראשונה שרואים סימול חדש מביאים נתונים תוך כדי הבקשה — קריאה אחת, בלי תור.
package server

import (
	"compress/gzip"
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

	mu       sync.Mutex
	hist     map[string]cachedHistory     // היסטוריה יומית מלאה לכל סימול
	deep     map[string]cachedHistory     // היסטוריה עמוקה (רק במסלול הגיבוי של Twelve Data)
	bt       map[string]cachedBT          // סימולציית עבר
	quotes   map[string]cachedQuote       // ציטוט Finnhub
	names    map[string]string            // שם חברה — לא משתנה, נשמר לתמיד
	series   map[string]cachedSeries      // סדרות גרף תוך-יומיות
	search   map[string]cachedSearch      // תוצאות חיפוש
	seen     map[string]time.Time         // סימולים שהמשתמש נגע בהם — אותם מחזיקים חמים
	inflight map[string]*flight           // בקשה אחת בכל רגע לכל מפתח
	uq       map[string]marketdata.BQuote // מחירי היקום המדורג (חבילה אחת לכולם)
	uqAt     time.Time

	cmu     sync.Mutex
	clients map[*sseClient]bool
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
	q  marketdata.FHQuote
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

// flight — בקשה שכבר בדרך; מי שמבקש את אותו דבר ממתין לה במקום לשלוח בקשה כפולה.
type flight struct{ done chan struct{} }

const (
	broadcastTick = 500 * time.Millisecond // עד 2 עדכונים חיים בשנייה
	chartPoints   = 400                    // דילול נקודות הגרף
	warmTick      = 20 * time.Second       // דופק הרענון ברקע
	seenTTL       = 3 * 24 * time.Hour     // כמה זמן ממשיכים לחמם סימול שלא נצפה
	firstFetchCap = 6 * time.Second        // תקרה מוחלטת להבאה ראשונה של סימול חדש
	maxDeepPages  = 2
)

// marketHours — האם הבורסה בארה"ב פתוחה עכשיו (לפי השעון).
func marketHours() bool {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		return true
	}
	t := time.Now().In(ny)
	if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	return m >= 9*60+30 && m < 16*60
}

// ttl — מטמון קצר כשהשוק פתוח, ארוך כשהוא סגור (אז שום דבר לא זז ממילא).
func ttl(open, closed time.Duration) time.Duration {
	if marketHours() {
		return open
	}
	return closed
}

func quoteTTL() time.Duration { return ttl(15*time.Second, 6*time.Hour) }

// seriesTTL — כל כמה זמן שווה לרענן גרף תוך-יומי.
func seriesTTL(rng string) time.Duration {
	switch rng {
	case "1h":
		return ttl(60*time.Second, 6*time.Hour)
	case "5d":
		return ttl(10*time.Minute, 6*time.Hour)
	default: // 1d
		return ttl(2*time.Minute, 6*time.Hour)
	}
}

func today() string { return time.Now().Format("2006-01-02") }

func New(cfg config.Config) *Server {
	s := &Server{
		mux:      http.NewServeMux(),
		cfg:      cfg,
		md:       marketdata.New(cfg.TwelveDataKey, cfg.FinnhubKey),
		feed:     livefeed.New(cfg.FinnhubKey),
		hist:     make(map[string]cachedHistory),
		deep:     make(map[string]cachedHistory),
		bt:       make(map[string]cachedBT),
		quotes:   make(map[string]cachedQuote),
		names:    make(map[string]string),
		series:   make(map[string]cachedSeries),
		search:   make(map[string]cachedSearch),
		seen:     make(map[string]time.Time),
		inflight: make(map[string]*flight),
		uq:       make(map[string]marketdata.BQuote),
		clients:  make(map[*sseClient]bool),
	}
	s.routes()
	s.feed.Start()
	go s.broadcastLoop()
	go s.warmLoop()
	go s.rankLoop()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/healthz", s.handleHealth)
	s.mux.HandleFunc("/api/analyze", s.handleAnalyze)
	s.mux.HandleFunc("/api/search", s.handleSearch)
	s.mux.HandleFunc("/api/series", s.handleSeries)
	s.mux.HandleFunc("/api/backtest", s.handleBacktest)
	s.mux.HandleFunc("/api/rank", s.handleRank)
	s.mux.HandleFunc("/api/stream", s.handleStream)
	s.mux.Handle("/", http.FileServer(http.Dir(s.cfg.WebDir)))
}

func (s *Server) Start() error {
	addr := ":" + s.cfg.Port
	log.Printf("trade מאזין על %s (web=%s, נרות=yahoo, גיבוי=%v, זרם חי=%v)",
		addr, s.cfg.WebDir, s.md.HasKey(), s.feed.Enabled())
	return http.ListenAndServe(addr, s.compress(s.logRequests(s.mux)))
}

// compress — דוחס את התשובות. הדף עצמו יורד פי ארבעה יותר מהר, וזה מורגש בעיקר בסלולר.
// הזרם החי יוצא מן הכלל: דחיסה הייתה תוקעת אותו בבאפר במקום לדחוף כל עסקה מיד.
func (s *Server) compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/stream" || !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		next.ServeHTTP(gzipWriter{ResponseWriter: w, gz: gz}, r)
	})
}

type gzipWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (g gzipWriter) Write(b []byte) (int, error) { return g.gz.Write(b) }

// WriteHeader — האורך המקורי כבר לא נכון אחרי הדחיסה, אז מסירים אותו.
func (g gzipWriter) WriteHeader(code int) {
	g.ResponseWriter.Header().Del("Content-Length")
	g.ResponseWriter.WriteHeader(code)
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
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"candles": s.md.CandleSource(), // מי מספק את הנרות בפועל
		"live":    s.feed.Connected(),
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// ---------- singleflight ----------

// once — מריץ את fn פעם אחת בלבד לכל מפתח. אם כבר רצה בקשה כזו, ממתינים לתוצאה שלה
// (עד cap) במקום לשלוח בקשה כפולה לספק.
func (s *Server) once(key string, cap time.Duration, fn func()) {
	s.mu.Lock()
	if f, ok := s.inflight[key]; ok {
		s.mu.Unlock()
		select {
		case <-f.done:
		case <-time.After(cap):
		}
		return
	}
	f := &flight{done: make(chan struct{})}
	s.inflight[key] = f
	s.mu.Unlock()

	fn()

	s.mu.Lock()
	delete(s.inflight, key)
	s.mu.Unlock()
	close(f.done)
}

// touch — מסמן שהמשתמש מתעניין בסימול; מכאן והלאה מחזיקים אותו חם ברקע.
func (s *Server) touch(symbol string) {
	s.mu.Lock()
	s.seen[symbol] = time.Now()
	s.mu.Unlock()
}

// ---------- ניתוח ----------

type analyzeResponse struct {
	indicators.Result
	Kind       string `json:"kind"`
	Exchange   string `json:"exchange,omitempty"`
	MarketOpen bool   `json:"marketOpen"`
	Live       bool   `json:"live"`
}

func (s *Server) handleAnalyze(w http.ResponseWriter, r *http.Request) {
	symbol := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("symbol")))
	if symbol == "" {
		writeJSON(w, http.StatusBadRequest, indicators.Result{OK: false, Reason: "חסר סימול מניה."})
		return
	}

	full, meta, err := s.history(symbol)
	if err != nil {
		writeJSON(w, http.StatusOK, indicators.Result{OK: false, Symbol: symbol, Reason: err.Error()})
		return
	}
	candles := tailCopy(full, indicators.Window) // עותק — כאן משלבים את המחיר החי
	s.feed.Subscribe(symbol)                     // מרגע שהסתכלת על מניה — היא נכנסת לזרם החי

	marketOpen := marketHours()
	price, prevQuote := 0.0, 0.0

	// המחיר החי מגיע מ-Finnhub (מכסה נדיבה). זמן העסקה האחרונה מגלה גם חגים.
	if q, ok := s.getQuote(symbol); ok {
		price, prevQuote = q.Price, q.PrevClose
		if !q.LastTrade.IsZero() {
			marketOpen = marketHours() && time.Since(q.LastTrade) < 30*time.Minute
		}
	}
	// המחיר מהזרם החי הוא הטרי ביותר — גובר על הכול
	if lp, ok := s.feed.Price(symbol); ok && lp > 0 {
		price = lp
	}
	candles = applyLive(candles, price, marketOpen)

	prevClose := candles[len(candles)-1].Close
	if len(candles) > 1 {
		prevClose = candles[len(candles)-2].Close // הסגירה של יום המסחר הקודם
	}
	if prevQuote > 0 {
		prevClose = prevQuote
	}

	res := indicators.Analyze(candles)
	res.Symbol = symbol
	res.Name = s.companyName(symbol, meta.Name)
	res.PrevClose = prevClose
	res.Change = res.Price - prevClose
	if prevClose != 0 {
		res.ChangePct = res.Change / prevClose * 100
	}
	writeJSON(w, http.StatusOK, analyzeResponse{
		Result: res, Kind: meta.Kind(), Exchange: meta.Exchange,
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
			s.touch(p) // הרשימה של המשתמש — בדיוק מה שצריך להישאר חם
		}
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

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

// livePayload — עדכון חי. הציון מחושב מחדש מהנרות שבמטמון — אפס בקשות API.
func (s *Server) livePayload(sym string, price float64) liveUpdate {
	u := liveUpdate{Symbol: sym, Price: price}

	candles, ok := s.cachedCandles(sym)
	if !ok || len(candles) < 2 {
		return u
	}
	candles = applyLive(candles, price, marketHours())

	prev := candles[len(candles)-2].Close
	if q, ok := s.cachedQuote(sym); ok && q.PrevClose > 0 {
		prev = q.PrevClose
	}

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
	if len(q) < 1 || (!s.md.HasFinnhub() && !s.md.HasKey()) {
		writeJSON(w, http.StatusOK, []marketdata.SearchItem{})
		return
	}
	key := strings.ToLower(q)

	s.mu.Lock()
	if c, ok := s.search[key]; ok && time.Since(c.at) < 10*time.Minute {
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

type seriesResponse struct {
	OK     bool               `json:"ok"`
	Symbol string             `json:"symbol"`
	Range  string             `json:"range"`
	Reason string             `json:"reason,omitempty"`
	Points []marketdata.Point `json:"points"`
}

// derivedDays — טווחים שנגזרים מההיסטוריה היומית שכבר במטמון (0 = הכול). אפס בקשות נוספות.
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

func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	symbol := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("symbol")))
	rng := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("range")))
	if rng == "" {
		rng = "1d"
	}
	if symbol == "" {
		writeJSON(w, http.StatusOK, seriesResponse{OK: false, Range: rng, Reason: "חסר סימול."})
		return
	}

	// חודש/שנה/5 שנים/מקסימום — נגזרים מההיסטוריה היומית. בלי שום בקשה חיצונית.
	if days, derived := derivedDays[rng]; derived {
		var candles []indicators.Candle
		var err error
		if rng == "max" {
			candles, err = s.deepHistory(symbol)
		} else {
			candles, _, err = s.history(symbol)
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

	pts, err := s.intraday(symbol, rng)
	if err != nil {
		writeJSON(w, http.StatusOK, seriesResponse{OK: false, Symbol: symbol, Range: rng, Reason: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, seriesResponse{OK: true, Symbol: symbol, Range: rng, Points: pts})
}

// intraday — גרף תוך-יומי מהמטמון. ישן מוגש מיד ומתרענן ברקע; רק בפעם הראשונה מביאים בבקשה עצמה.
func (s *Server) intraday(symbol, rng string) ([]marketdata.Point, error) {
	s.touch(symbol)
	key := symbol + "|" + rng

	s.mu.Lock()
	c, ok := s.series[key]
	s.mu.Unlock()
	if ok {
		if time.Since(c.at) >= c.ttl {
			go s.fetchSeries(symbol, rng, marketdata.WaitBG)
		}
		return c.pts, nil
	}

	err := s.fetchSeries(symbol, rng, marketdata.WaitUser)
	s.mu.Lock()
	c, ok = s.series[key]
	s.mu.Unlock()
	if ok {
		return c.pts, nil
	}
	if err == nil {
		err = fmt.Errorf("אין נתוני גרף עבור %s", symbol)
	}
	return nil, err
}

func (s *Server) fetchSeries(symbol, rng string, wait time.Duration) error {
	var err error
	s.once("s|"+symbol+"|"+rng, firstFetchCap, func() {
		pts, e := s.md.Intraday(symbol, rng, wait)
		if e != nil || len(pts) == 0 {
			err = e
			return
		}
		s.mu.Lock()
		s.series[symbol+"|"+rng] = cachedSeries{at: time.Now(), ttl: seriesTTL(rng), pts: pts}
		s.mu.Unlock()
	})
	return err
}

// ---------- היסטוריה: תמיד מהמטמון, רענון ברקע ----------

// history — ההיסטוריה היומית. הפרוסה משותפת — לקריאה בלבד (מי שמשנה ישתמש ב-tailCopy).
func (s *Server) history(symbol string) ([]indicators.Candle, marketdata.Meta, error) {
	s.touch(symbol)

	s.mu.Lock()
	c, ok := s.hist[symbol]
	s.mu.Unlock()

	if ok {
		if c.day != today() {
			go s.fetchHistory(symbol, marketdata.WaitBG) // התיישן — מרעננים ברקע, בלי להשהות אף אחד
		}
		return c.candles, c.meta, nil
	}

	// סימול חדש שאין עליו כלום — חייבים להביא עכשיו. קריאה אחת, בלי תור.
	err := s.fetchHistory(symbol, marketdata.WaitUser)
	s.mu.Lock()
	c, ok = s.hist[symbol]
	s.mu.Unlock()
	if ok {
		return c.candles, c.meta, nil
	}
	if err == nil {
		err = fmt.Errorf("לא נמצאו נתונים עבור %s", symbol)
	}
	return nil, marketdata.Meta{}, err
}

func (s *Server) fetchHistory(symbol string, wait time.Duration) error {
	var err error
	s.once("h|"+symbol, firstFetchCap, func() {
		candles, meta, e := s.md.History(symbol, wait)
		if e != nil || len(candles) == 0 {
			err = e
			return
		}
		s.mu.Lock()
		s.hist[symbol] = cachedHistory{day: today(), meta: meta, candles: candles}
		if meta.Name != "" {
			s.names[symbol] = meta.Name // Yahoo מחזיר את השם בחינם — חוסך בקשה
		}
		delete(s.bt, symbol) // נתונים חדשים — הסימולציה תחושב מחדש בפעם הבאה
		s.mu.Unlock()
	})
	return err
}

// deepHistory — טווח "מקסימום". אצל Yahoo ההיסטוריה ממילא מלאה מיום ההנפקה;
// רק במסלול הגיבוי של Twelve Data צריך לדפדף אחורה — וזה קורה ברקע בלבד.
func (s *Server) deepHistory(symbol string) ([]indicators.Candle, error) {
	base, meta, err := s.history(symbol)
	if err != nil {
		return nil, err
	}
	_ = meta

	s.mu.Lock()
	c, ok := s.deep[symbol]
	s.mu.Unlock()
	if ok && c.day == today() {
		return c.candles, nil // ההיסטוריה המלאה כבר במטמון
	}

	// עוד לא הבאנו את הכל — מביאים ברקע ומגישים בינתיים את 8 השנים שיש. אף אחד לא מחכה.
	go s.fetchDeep(symbol)
	if ok {
		return c.candles, nil
	}
	return base, nil
}

// fetchDeep — כל ההיסטוריה מיום ההנפקה. מנה כבדה, ולכן תמיד ברקע.
func (s *Server) fetchDeep(symbol string) {
	s.once("d|"+symbol, firstFetchCap, func() {
		all, _, err := s.md.HistoryAll(symbol, marketdata.WaitBG)
		if err != nil || len(all) == 0 {
			return
		}
		// במסלול הגיבוי של Twelve Data מגיעים 5000 נרות לכל היותר — מדפדפים אחורה
		for page := 0; page < maxDeepPages && len(all) >= marketdata.MaxCandles; page++ {
			oldest := all[0].Date
			older, err := s.md.HistoryBefore(symbol, dayOf(oldest), marketdata.WaitBG)
			if err != nil {
				break
			}
			var fresh []indicators.Candle
			for _, c := range older {
				if c.Date < oldest { // YYYY-MM-DD — השוואת מחרוזות תקינה
					fresh = append(fresh, c)
				}
			}
			if len(fresh) == 0 {
				break
			}
			all = append(fresh, all...)
			if len(older) < marketdata.MaxCandles {
				break
			}
		}
		s.mu.Lock()
		s.deep[symbol] = cachedHistory{day: today(), candles: all}
		s.mu.Unlock()
	})
}

func dayOf(datetime string) string {
	if len(datetime) >= 10 {
		return datetime[:10]
	}
	return datetime
}

// ---------- חימום ברקע ----------

// warmLoop — הדופק שמחזיק את הנתונים חמים, כדי שהמשתמש תמיד יקבל תשובה מיידית.
// כל מה שכאן ממתין בסבלנות בתור של הספק — אף אחד לא מסתכל על זה.
func (s *Server) warmLoop() {
	t := time.NewTicker(warmTick)
	defer t.Stop()
	for range t.C {
		for _, sym := range s.warmList() {
			s.mu.Lock()
			h, hasHist := s.hist[sym]
			s.mu.Unlock()
			if !hasHist || h.day != today() {
				s.fetchHistory(sym, marketdata.WaitBG)
			}

			s.mu.Lock()
			d, hasDeep := s.deep[sym]
			s.mu.Unlock()
			if !hasDeep || d.day != today() {
				s.fetchDeep(sym) // שגרף "מקס" יהיה מוכן עוד לפני שילחצו עליו
			}

			for _, rng := range []string{"1h", "1d", "5d"} {
				s.mu.Lock()
				c, has := s.series[sym+"|"+rng]
				s.mu.Unlock()
				switch {
				case has && time.Since(c.at) >= c.ttl:
					s.fetchSeries(sym, rng, marketdata.WaitBG) // התיישן — מרעננים
				case !has && rng == "1d" && s.md.YahooUp():
					s.fetchSeries(sym, rng, marketdata.WaitBG) // הגרף שנפתח ראשון — שיהיה מוכן מראש
				}
			}
		}
	}
}

// warmList — הסימולים שהמשתמש נגע בהם לאחרונה.
func (s *Server) warmList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.seen))
	for sym, at := range s.seen {
		if time.Since(at) > seenTTL {
			delete(s.seen, sym)
			continue
		}
		out = append(out, sym)
	}
	return out
}

// ---------- סימולציית עבר ----------

type backtestResponse struct {
	OK     bool   `json:"ok"`
	Symbol string `json:"symbol"`
	Reason string `json:"reason,omitempty"`
	backtest.Result
}

func (s *Server) handleBacktest(w http.ResponseWriter, r *http.Request) {
	symbol := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("symbol")))
	if symbol == "" {
		writeJSON(w, http.StatusOK, backtestResponse{OK: false, Reason: "חסר סימול."})
		return
	}

	s.mu.Lock()
	if c, ok := s.bt[symbol]; ok && c.day == today() {
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, backtestResponse{OK: true, Symbol: symbol, Result: c.res})
		return
	}
	s.mu.Unlock()

	candles, _, err := s.history(symbol)
	if err != nil {
		writeJSON(w, http.StatusOK, backtestResponse{OK: false, Symbol: symbol, Reason: err.Error()})
		return
	}

	res := backtest.Run(candles) // חישוב מקומי בלבד — אפס בקשות API
	s.mu.Lock()
	s.bt[symbol] = cachedBT{day: today(), res: res}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, backtestResponse{OK: true, Symbol: symbol, Result: res})
}

// ---------- מטמונים קטנים ----------

// getQuote — ציטוט Finnhub. אם הוא לא זמין — פשוט אין ציטוט; לא מפילים את הבקשה.
func (s *Server) getQuote(symbol string) (marketdata.FHQuote, bool) {
	s.mu.Lock()
	c, ok := s.quotes[symbol]
	s.mu.Unlock()
	if ok && time.Since(c.at) < quoteTTL() {
		return c.q, true
	}

	q, err := s.md.FinnhubQuote(symbol)
	if err != nil {
		return c.q, ok // ציטוט ישן עדיף על כלום
	}
	s.mu.Lock()
	s.quotes[symbol] = cachedQuote{at: time.Now(), q: q}
	s.mu.Unlock()
	return q, true
}

func (s *Server) cachedQuote(symbol string) (marketdata.FHQuote, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.quotes[symbol]
	return c.q, ok
}

// companyName — השם מהמטמון; אם Yahoo כבר החזיר אותו — משתמשים בו ולא שואלים אף אחד.
func (s *Server) companyName(symbol, fromMeta string) string {
	s.mu.Lock()
	n, ok := s.names[symbol]
	s.mu.Unlock()
	if ok && n != "" {
		return n
	}
	if fromMeta != "" {
		s.mu.Lock()
		s.names[symbol] = fromMeta
		s.mu.Unlock()
		return fromMeta
	}

	n, err := s.md.CompanyName(symbol)
	if err != nil || n == "" {
		return symbol // לא שווה להיכשל בגלל שם
	}
	s.mu.Lock()
	s.names[symbol] = n
	s.mu.Unlock()
	return n
}

// cachedCandles — עותק של חלון הנרות האחרון, מהמטמון בלבד — ללולאת השידור החי.
func (s *Server) cachedCandles(symbol string) ([]indicators.Candle, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.hist[symbol]
	if !ok || len(c.candles) == 0 {
		return nil, false
	}
	return tailCopy(c.candles, indicators.Window), true
}

// nyToday — התאריך של היום בבורסה (ניו-יורק), לא לפי השעון שלנו.
func nyToday() string {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.Now().Format("2006-01-02")
	}
	return time.Now().In(ny).Format("2006-01-02")
}

// applyLive — משלב את המחיר החי בתוך סדרת הנרות.
//
// אם הנר האחרון הוא של היום — מעדכנים את הסגירה שלו (וגם את השיא/שפל, אם המחיר חרג).
// אם השוק פתוח אבל עדיין אין נר להיום (קורה בדקות הראשונות של המסחר, לפני שהספק
// פותח אותו) — מוסיפים נר חדש. הגרסה הקודמת פשוט דרסה את הסגירה של אתמול,
// וזה זייף לרגע את כל האינדיקטורים: יום מסחר שלם היה נעלם מההיסטוריה.
func applyLive(candles []indicators.Candle, price float64, open bool) []indicators.Candle {
	if price <= 0 || len(candles) == 0 {
		return candles
	}
	last := &candles[len(candles)-1]
	if !open || last.Date == nyToday() {
		last.Close = price
		if price > last.High {
			last.High = price
		}
		if price < last.Low {
			last.Low = price
		}
		return candles
	}
	return append(candles, indicators.Candle{
		Date: nyToday(), Open: price, High: price, Low: price, Close: price,
	})
}

// tailCopy — עותק בר-שינוי של N הנרות האחרונים. במטמון עצמו לא נוגעים.
func tailCopy(in []indicators.Candle, n int) []indicators.Candle {
	if n > len(in) {
		n = len(in)
	}
	out := make([]indicators.Candle, n)
	copy(out, in[len(in)-n:])
	return out
}
