// Package marketdata מושך נתוני מניות ממקורות חיצוניים (Twelve Data + Finnhub).
package marketdata

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"stockcompass/internal/indicators"
)

type Client struct {
	tdKey string
	fhKey string
	http  *http.Client
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

type tdResponse struct {
	Meta struct {
		Symbol string `json:"symbol"`
	} `json:"meta"`
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
}

// History מושך היסטוריית נרות יומיים (oldest-first) ואת שם המניה.
func (c *Client) History(symbol string) (candles []indicators.Candle, name string, err error) {
	u := fmt.Sprintf("https://api.twelvedata.com/time_series?symbol=%s&interval=1day&outputsize=300&apikey=%s",
		url.QueryEscape(symbol), url.QueryEscape(c.tdKey))
	resp, err := c.http.Get(u)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	var r tdResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, "", err
	}
	if r.Status == "error" || len(r.Values) == 0 {
		msg := r.Message
		if msg == "" {
			msg = "לא נמצאו נתונים עבור " + symbol
		}
		return nil, "", fmt.Errorf("%s", msg)
	}

	parse := func(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }
	// Twelve Data מחזיר newest-first → הופכים ל-oldest-first
	candles = make([]indicators.Candle, 0, len(r.Values))
	for i := len(r.Values) - 1; i >= 0; i-- {
		v := r.Values[i]
		candles = append(candles, indicators.Candle{
			Date: v.Datetime, Open: parse(v.Open), High: parse(v.High),
			Low: parse(v.Low), Close: parse(v.Close), Volume: parse(v.Volume),
		})
	}
	name = r.Meta.Symbol
	if name == "" {
		name = symbol
	}
	return candles, name, nil
}

// LivePrice מושך מחיר חי מ-Finnhub (רשות). ok=false אם אין מפתח או תקלה.
func (c *Client) LivePrice(symbol string) (price, prevClose float64, ok bool) {
	if c.fhKey == "" {
		return 0, 0, false
	}
	u := fmt.Sprintf("https://finnhub.io/api/v1/quote?symbol=%s&token=%s",
		url.QueryEscape(symbol), url.QueryEscape(c.fhKey))
	resp, err := c.http.Get(u)
	if err != nil {
		return 0, 0, false
	}
	defer resp.Body.Close()
	var q struct {
		C  float64 `json:"c"`  // current
		PC float64 `json:"pc"` // previous close
	}
	if err := json.NewDecoder(resp.Body).Decode(&q); err != nil {
		return 0, 0, false
	}
	if q.C <= 0 {
		return 0, 0, false
	}
	return q.C, q.PC, true
}
