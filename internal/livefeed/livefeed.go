// Package livefeed מחזיק חיבור זרם חי (WebSocket) ל-Finnhub ומקבל כל עסקה בזמן אמת.
// המסלול החינמי של Finnhub מאפשר עד 50 סימולים בזרם — לשימוש אישי.
package livefeed

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const maxSymbols = 50 // מגבלת המסלול החינמי

type Feed struct {
	key string

	mu        sync.Mutex
	prices    map[string]float64 // מחיר אחרון לכל סימול
	dirty     map[string]bool    // סימולים שהשתנו מאז השידור האחרון
	subs      map[string]bool    // סימולים שאנחנו רשומים אליהם
	conn      *websocket.Conn
	connected bool
	lastTrade time.Time
}

func New(finnhubKey string) *Feed {
	return &Feed{
		key:    finnhubKey,
		prices: make(map[string]float64),
		dirty:  make(map[string]bool),
		subs:   make(map[string]bool),
	}
}

// Enabled — האם הוגדר מפתח Finnhub (בלעדיו אין זרם חי).
func (f *Feed) Enabled() bool { return f.key != "" }

// Connected — האם הצינור פתוח כרגע.
func (f *Feed) Connected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

// Start — מפעיל את לולאת החיבור ברקע.
func (f *Feed) Start() {
	if !f.Enabled() {
		log.Printf("livefeed: אין מפתח Finnhub — הזרם החי כבוי")
		return
	}
	go f.loop()
}

func (f *Feed) loop() {
	backoff := time.Second
	for {
		c, _, err := websocket.DefaultDialer.Dial("wss://ws.finnhub.io?token="+f.key, nil)
		if err != nil {
			log.Printf("livefeed: חיבור נכשל (%v) — ניסיון חוזר בעוד %s", err, backoff)
			time.Sleep(backoff)
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second

		f.mu.Lock()
		f.conn, f.connected = c, true
		subs := make([]string, 0, len(f.subs))
		for s := range f.subs {
			subs = append(subs, s)
		}
		f.mu.Unlock()

		log.Printf("livefeed: מחובר לזרם החי (%d סימולים)", len(subs))
		for _, s := range subs {
			f.sendSub(c, s) // רישום מחדש אחרי ניתוק
		}

		f.readLoop(c)

		f.mu.Lock()
		f.conn, f.connected = nil, false
		f.mu.Unlock()
		log.Printf("livefeed: הצינור נותק — מתחבר מחדש")
		time.Sleep(time.Second)
	}
}

func (f *Feed) sendSub(c *websocket.Conn, sym string) {
	_ = c.WriteJSON(map[string]string{"type": "subscribe", "symbol": sym})
}

func (f *Feed) readLoop(c *websocket.Conn) {
	defer c.Close()
	for {
		_, raw, err := c.ReadMessage()
		if err != nil {
			return
		}
		var m struct {
			Type string `json:"type"`
			Data []struct {
				S string  `json:"s"`
				P float64 `json:"p"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &m) != nil || m.Type != "trade" {
			continue // ping / error / הודעה לא מוכרת
		}
		f.mu.Lock()
		for _, d := range m.Data {
			if d.P > 0 && f.prices[d.S] != d.P {
				f.prices[d.S] = d.P
				f.dirty[d.S] = true
			}
		}
		f.lastTrade = time.Now()
		f.mu.Unlock()
	}
}

// Subscribe — נרשם לסימול (חסין לכפילויות). מוגבל ל-50 סימולים.
func (f *Feed) Subscribe(sym string) {
	if !f.Enabled() || sym == "" {
		return
	}
	f.mu.Lock()
	if f.subs[sym] || len(f.subs) >= maxSymbols {
		f.mu.Unlock()
		return
	}
	f.subs[sym] = true
	c := f.conn
	f.mu.Unlock()
	if c != nil {
		f.sendSub(c, sym)
	}
}

// TakeDirty — מחזיר את הסימולים שהמחיר שלהם השתנה מאז הקריאה הקודמת, ומאפס.
func (f *Feed) TakeDirty() map[string]float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.dirty) == 0 {
		return nil
	}
	out := make(map[string]float64, len(f.dirty))
	for s := range f.dirty {
		out[s] = f.prices[s]
	}
	f.dirty = make(map[string]bool)
	return out
}

// Price — המחיר האחרון שהתקבל בזרם.
func (f *Feed) Price(sym string) (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.prices[sym]
	return p, ok
}
