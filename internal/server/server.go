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

	mu    sync.Mutex
	cache map[string]cachedHistory // מטמון היסטוריה יומי לכל סימול
}

type cachedHistory struct {
	day      string // YYYY-MM-DD
	name     string
	candles  []indicators.Candle
}

func New(cfg config.Config) *Server {
	s := &Server{
		mux:   http.NewServeMux(),
		cfg:   cfg,
		md:    marketdata.New(cfg.TwelveDataKey, cfg.FinnhubKey),
		cache: make(map[string]cachedHistory),
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/healthz", s.handleHealth)
	s.mux.HandleFunc("/api/analyze", s.handleAnalyze)
	// כל השאר — אתר הווב הסטטי (index.html וכו')
	fs := http.FileServer(http.Dir(s.cfg.WebDir))
	s.mux.Handle("/", fs)
}

func (s *Server) Start() error {
	addr := ":" + s.cfg.Port
	log.Printf("מצפן המניות מאזין על %s (web=%s, twelvedata=%v, finnhub=%v)",
		addr, s.cfg.WebDir, s.md.HasKey(), s.cfg.FinnhubKey != "")
	return http.ListenAndServe(addr, s.logRequests(s.mux))
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path == "/api/analyze" {
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

	candles, name, err := s.getHistory(symbol)
	if err != nil {
		writeJSON(w, http.StatusOK, indicators.Result{OK: false, Symbol: symbol, Reason: err.Error()})
		return
	}

	// מחיר חי אופציונלי מ-Finnhub — מעדכן את הנר האחרון
	prevClose := candles[len(candles)-1].Close
	if len(candles) > 1 {
		prevClose = candles[len(candles)-2].Close
	}
	if price, pc, ok := s.md.LivePrice(symbol); ok {
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
	writeJSON(w, http.StatusOK, res)
}

// getHistory — עם מטמון יומי (נרות יומיים משתנים פעם ביום).
func (s *Server) getHistory(symbol string) ([]indicators.Candle, string, error) {
	today := time.Now().Format("2006-01-02")
	s.mu.Lock()
	if c, ok := s.cache[symbol]; ok && c.day == today {
		s.mu.Unlock()
		return cloneCandles(c.candles), c.name, nil
	}
	s.mu.Unlock()

	candles, name, err := s.md.History(symbol)
	if err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	s.cache[symbol] = cachedHistory{day: today, name: name, candles: candles}
	s.mu.Unlock()
	return cloneCandles(candles), name, nil
}

// cloneCandles — עותק כדי שעדכון המחיר החי לא ישנה את המטמון.
func cloneCandles(in []indicators.Candle) []indicators.Candle {
	out := make([]indicators.Candle, len(in))
	copy(out, in)
	return out
}
