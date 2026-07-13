package marketdata

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// האם Yahoo נותן מחירים להרבה מניות בבקשה אחת? זה מה שיקבע אם מסך הדירוג
// יציג מחירים חיים או רק את הסגירה האחרונה.
func TestYahooBatchQuote(t *testing.T) {
	if os.Getenv("LIVE") == "" {
		t.Skip("מבחן רשת — הרץ עם LIVE=1")
	}
	y := newYahoo()
	syms := []string{"AAPL", "MSFT", "NVDA", "AMZN", "GOOGL", "META", "TSLA", "JPM", "XOM", "SPY"}

	u := "https://query2.finance.yahoo.com/v7/finance/quote?symbols=" + strings.Join(syms, ",")
	if cr := y.prime(); cr != "" {
		u += "&crumb=" + cr
	}
	resp, err := y.do(u)
	if err != nil {
		t.Fatalf("החבילה נכשלה: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var r struct {
		QuoteResponse struct {
			Result []struct {
				Symbol                     string  `json:"symbol"`
				RegularMarketPrice         float64 `json:"regularMarketPrice"`
				RegularMarketChangePercent float64 `json:"regularMarketChangePercent"`
			} `json:"result"`
			Error any `json:"error"`
		} `json:"quoteResponse"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("תשובה לא תקינה: %v — %.120s", err, b)
	}
	if len(r.QuoteResponse.Result) == 0 {
		t.Fatalf("חבילה ריקה: %.200s", b)
	}
	for _, q := range r.QuoteResponse.Result {
		t.Logf("  %-6s %8.2f  %+.2f%%", q.Symbol, q.RegularMarketPrice, q.RegularMarketChangePercent)
	}
	t.Logf("✔ %d מניות בבקשה אחת", len(r.QuoteResponse.Result))
}
