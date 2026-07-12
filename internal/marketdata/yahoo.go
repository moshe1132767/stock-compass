package marketdata

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"stockcompass/internal/indicators"
)

// Yahoo Finance — מקור הנרות הראשי.
// למה דווקא הוא: אין מפתח, אין מכסת קרדיטים לדקה, ויש היסטוריה מלאה מיום ההנפקה
// וגם נרות תוך-יומיים — כלומר בדיוק מה שחנק אותנו אצל Twelve Data (8 קרדיטים לדקה).
// אם Yahoo חוסם אותנו מסיבה כלשהי — יש מפסק שמעביר מיד ל-Twelve Data, בלי להשהות אף בקשה.

const yahooUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

// coolDown — אחרי כישלון, לא מנסים שוב במשך הזמן הזה.
// זה מה שמונע מספק חסום להוסיף השהיה לכל בקשה של המשתמש.
const coolDown = 5 * time.Minute

type yahoo struct {
	mu      sync.Mutex
	pmu     sync.Mutex // הרמת העוגייה — אחד בכל פעם
	http    *http.Client
	crumb   string
	primed  time.Time
	brokeAt time.Time // מתי נכשל לאחרונה
	served  time.Time // מתי הצליח לאחרונה
	ny      *time.Location
}

func newYahoo() *yahoo {
	jar, _ := cookiejar.New(nil)
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		ny = time.UTC
	}
	return &yahoo{
		http: &http.Client{Timeout: 12 * time.Second, Jar: jar},
		ny:   ny,
	}
}

// up — האם כדאי לנסות את Yahoo עכשיו (מפסק אחרי כישלון).
func (y *yahoo) up() bool {
	y.mu.Lock()
	defer y.mu.Unlock()
	return time.Since(y.brokeAt) > coolDown
}

func (y *yahoo) broke() {
	y.mu.Lock()
	y.brokeAt = time.Now()
	y.crumb, y.primed = "", time.Time{}
	y.mu.Unlock()
}

func (y *yahoo) worked() {
	y.mu.Lock()
	y.brokeAt, y.served = time.Time{}, time.Now()
	y.mu.Unlock()
}

// source — מי באמת מספק את הנרות כרגע. לחיווי מצב בלבד.
func (y *yahoo) source() string {
	y.mu.Lock()
	defer y.mu.Unlock()
	switch {
	case !y.served.IsZero() && y.brokeAt.IsZero():
		return "yahoo"
	case !y.brokeAt.IsZero():
		return "twelvedata"
	}
	return "—"
}

// prime — Yahoo דורש עוגייה + "crumb" לפני שהוא עונה. מרימים אותם פעם בשעה.
// pmu מבטיח שרק גורם אחד מרים אותם: אחרת מטח בקשות היה גורר מטח הרמות — ויאהו היה חוסם.
func (y *yahoo) prime() string {
	y.pmu.Lock()
	defer y.pmu.Unlock()

	y.mu.Lock()
	if y.crumb != "" && time.Since(y.primed) < time.Hour {
		c := y.crumb
		y.mu.Unlock()
		return c
	}
	y.mu.Unlock()

	// שלב 1: עוגיות
	for _, u := range []string{"https://fc.yahoo.com/", "https://finance.yahoo.com/"} {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("User-Agent", yahooUA)
		if resp, err := y.http.Do(req); err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
			resp.Body.Close()
		}
	}
	// שלב 2: crumb
	req, _ := http.NewRequest("GET", "https://query2.finance.yahoo.com/v1/test/getcrumb", nil)
	req.Header.Set("User-Agent", yahooUA)
	resp, err := y.http.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
	cr := strings.TrimSpace(string(b))
	if resp.StatusCode != http.StatusOK || cr == "" || strings.ContainsAny(cr, " <{") {
		return "" // חסום / דף שגיאה — לא crumb
	}
	y.mu.Lock()
	y.crumb, y.primed = cr, time.Now()
	y.mu.Unlock()
	return cr
}

// daily — נרות יומיים מ-since ועד היום (since=0 → הכל, מיום ההנפקה).
func (y *yahoo) daily(symbol string, since time.Time) ([]indicators.Candle, Meta, error) {
	from := int64(0)
	if !since.IsZero() {
		from = since.Unix()
	}
	q := fmt.Sprintf("period1=%d&period2=%d&interval=1d&includePrePost=false",
		from, time.Now().Add(48*time.Hour).Unix())
	return y.chart(symbol, q, true)
}

// do — בקשה ל-Yahoo עם ניסיון חוזר אחד: במטח של בקשות הוא לפעמים חונק בקשה בודדת (429),
// והניסיון השני כמעט תמיד מצליח. חצי שנייה — עדיין הרבה מתחת לסף שהמשתמש מרגיש.
func (y *yahoo) do(u string) (*http.Response, error) {
	var last error
	for try := 0; try < 2; try++ {
		if try > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("User-Agent", yahooUA)
		req.Header.Set("Accept", "application/json")
		resp, err := y.http.Do(req)
		if err != nil {
			last = err
			continue
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		resp.Body.Close()
		last = fmt.Errorf("yahoo החזיר %d", resp.StatusCode)
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return nil, last // 404 וכדומה — אין טעם לנסות שוב
		}
	}
	return nil, last
}

type yChart struct {
	Chart struct {
		Result []struct {
			Meta struct {
				Symbol             string  `json:"symbol"`
				ExchangeName       string  `json:"exchangeName"`
				FullExchangeName   string  `json:"fullExchangeName"`
				InstrumentType     string  `json:"instrumentType"`
				RegularMarketPrice float64 `json:"regularMarketPrice"`
				ShortName          string  `json:"shortName"`
				LongName           string  `json:"longName"`
			} `json:"meta"`
			Timestamp  []int64 `json:"timestamp"`
			Indicators struct {
				Quote []struct {
					Open   []*float64 `json:"open"`
					High   []*float64 `json:"high"`
					Low    []*float64 `json:"low"`
					Close  []*float64 `json:"close"`
					Volume []*float64 `json:"volume"`
				} `json:"quote"`
			} `json:"indicators"`
		} `json:"result"`
		Error *struct {
			Description string `json:"description"`
		} `json:"error"`
	} `json:"chart"`
}

// chart — קריאה אחת ל-Yahoo. daily=true מחזיר תאריכים בלבד (כמו Twelve Data),
// אחרת תאריך+שעה בשעון ניו-יורק — בדיוק הפורמט שהאתר כבר יודע להציג.
//
// שים לב ל-query: עבור ההיסטוריה היומית חייבים period1/period2 ולא range=max.
// עם range=max יאהו מחזיר בשקט נתונים רבעוניים — מה שהיה הורס את כל האינדיקטורים.
func (y *yahoo) chart(symbol, query string, daily bool) ([]indicators.Candle, Meta, error) {
	if !y.up() {
		return nil, Meta{}, fmt.Errorf("yahoo במנוחה אחרי כישלון")
	}
	u := fmt.Sprintf("https://query2.finance.yahoo.com/v8/finance/chart/%s?%s",
		url.PathEscape(symbol), query)
	if cr := y.prime(); cr != "" {
		u += "&crumb=" + url.QueryEscape(cr)
	}
	resp, err := y.do(u)
	if err != nil {
		y.broke()
		return nil, Meta{}, err
	}
	defer resp.Body.Close()

	var r yChart
	if err := json.NewDecoder(io.LimitReader(resp.Body, 24<<20)).Decode(&r); err != nil {
		y.broke()
		return nil, Meta{}, err
	}
	if len(r.Chart.Result) == 0 || len(r.Chart.Result[0].Indicators.Quote) == 0 {
		if r.Chart.Error != nil { // סימול לא קיים — זו לא תקלה של הספק
			return nil, Meta{}, fmt.Errorf("לא נמצאו נתונים עבור %s", symbol)
		}
		y.broke()
		return nil, Meta{}, fmt.Errorf("yahoo החזיר תשובה ריקה")
	}
	res := r.Chart.Result[0]
	q := res.Indicators.Quote[0]

	layout := "2006-01-02"
	if !daily {
		layout = "2006-01-02 15:04:05"
	}
	candles := make([]indicators.Candle, 0, len(res.Timestamp))
	for i, ts := range res.Timestamp {
		if i >= len(q.Close) || q.Close[i] == nil || *q.Close[i] <= 0 {
			continue // נר חסר (חג/הפסקה) — מדלגים
		}
		at := func(a []*float64) float64 {
			if i < len(a) && a[i] != nil {
				return *a[i]
			}
			return *q.Close[i]
		}
		vol := 0.0
		if i < len(q.Volume) && q.Volume[i] != nil {
			vol = *q.Volume[i]
		}
		candles = append(candles, indicators.Candle{
			Date:   time.Unix(ts, 0).In(y.ny).Format(layout),
			Open:   at(q.Open),
			High:   at(q.High),
			Low:    at(q.Low),
			Close:  *q.Close[i],
			Volume: vol,
		})
	}
	if len(candles) == 0 {
		return nil, Meta{}, fmt.Errorf("אין נרות עבור %s", symbol)
	}
	y.worked()

	m := res.Meta
	name := m.LongName
	if name == "" {
		name = m.ShortName
	}
	ex := m.FullExchangeName
	if ex == "" {
		ex = m.ExchangeName
	}
	return candles, Meta{
		Symbol: symbol, Type: m.InstrumentType, Exchange: ex,
		Name: name, Full: daily,
	}, nil
}
