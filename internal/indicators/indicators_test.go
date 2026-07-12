package indicators

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

type tdResp struct {
	Values []struct {
		Datetime string `json:"datetime"`
		Open     string `json:"open"`
		High     string `json:"high"`
		Low      string `json:"low"`
		Close    string `json:"close"`
		Volume   string `json:"volume"`
	} `json:"values"`
}

func loadAAPL(t *testing.T) []Candle {
	b, err := os.ReadFile("testdata/aapl.json")
	if err != nil { t.Fatal(err) }
	var r tdResp
	if err := json.Unmarshal(b, &r); err != nil { t.Fatal(err) }
	// newest-first → oldest-first
	cs := make([]Candle, 0, len(r.Values))
	for i := len(r.Values) - 1; i >= 0; i-- {
		v := r.Values[i]
		f := func(s string) float64 { x, _ := strconv.ParseFloat(s, 64); return x }
		cs = append(cs, Candle{v.Datetime, f(v.Open), f(v.High), f(v.Low), f(v.Close), f(v.Volume)})
	}
	return cs
}

func TestAnalyzeAAPL(t *testing.T) {
	res := Analyze(loadAAPL(t))
	if !res.OK { t.Fatalf("not ok: %s", res.Reason) }
	t.Logf("מחיר %.2f | ציון %d → %s | בעד %d ניטרלי %d נגד %d",
		res.Price, res.Score100, res.Recommendation, res.Agreement.Buy, res.Agreement.Neutral, res.Agreement.Sell)
	for _, i := range res.Indicators {
		t.Logf("  %-28s %-8s score=%.2f | %s", i.Name, i.Signal, i.Score, i.Detail)
	}
	if res.Score100 != 75 {
		t.Errorf("ציפינו ל-75 (כמו מנוע JS), קיבלנו %d", res.Score100)
	}
	if res.Recommendation != "קנייה" {
		t.Errorf("ציפינו 'קנייה', קיבלנו %q", res.Recommendation)
	}
}
