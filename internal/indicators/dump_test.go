package indicators

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// TestDump — מדפיס את הערכים הגולמיים של כל אינדיקטור עבור נרות שמוזנים מקובץ,
// כדי להשוות אותם למימוש עצמאי (פייתון) ולוודא שאין סטייה מההגדרות המקובלות.
// רץ רק כשמבקשים: DUMP=/path/candles.json go test ./internal/indicators -run TestDump
func TestDump(t *testing.T) {
	path := os.Getenv("DUMP")
	if path == "" {
		t.Skip("הרץ עם DUMP=<קובץ נרות>")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cs []Candle
	if err := json.Unmarshal(b, &cs); err != nil {
		t.Fatal(err)
	}

	closes := make([]float64, len(cs))
	highs := make([]float64, len(cs))
	lows := make([]float64, len(cs))
	vols := make([]float64, len(cs))
	for i, c := range cs {
		closes[i], highs[i], lows[i], vols[i] = c.Close, c.High, c.Low, c.Volume
	}

	out := map[string]any{}
	out["n"] = len(cs)
	out["last_date"] = cs[len(cs)-1].Date
	out["price"] = closes[len(closes)-1]

	s50, _ := sma(closes, 50)
	s200, _ := sma(closes, 200)
	out["sma50"], out["sma200"] = s50, s200

	r, _ := rsi(closes, 14)
	out["rsi14"] = r

	m, _ := macd(closes, 12, 26, 9)
	out["macd"], out["macd_signal"], out["macd_hist"] = m.macd, m.signal, m.hist

	k, d, _ := stochastic(highs, lows, closes, 14, 3)
	out["stoch_k"], out["stoch_d"] = k, d

	bb, _ := bollinger(closes, 20, 2)
	out["bb_mid"], out["bb_upper"], out["bb_lower"], out["bb_pctB"] = bb.mid, bb.upper, bb.lower, bb.pctB

	rising, _ := obvTrend(closes, vols)
	out["obv_rising"] = rising

	mom, approx, _ := momentum12m(closes)
	out["momentum"], out["momentum_approx"] = mom, approx

	res := Analyze(cs)
	out["score100"], out["composite"], out["reco"] = res.Score100, res.Composite, res.RecoKey

	j, _ := json.MarshalIndent(out, "", " ")
	fmt.Println(string(j))
}
