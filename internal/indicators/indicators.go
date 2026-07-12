// Package indicators מחשב אינדיקטורים טכניים ומאחד אותם לציון והמלצה.
// זהו פורט נאמן של מנוע ה-JS שנבדק (engine.js).
package indicators

import (
	"fmt"
	"math"
)

// Window — כמה נרות המנוע מקבל לחישוב. חייב להיות זהה בכל מקום (מסך, זרם חי, סימולציית עבר),
// אחרת אותו יום יקבל ציון שונה בכל מסלול.
const Window = 300

// Candle — נר יומי אחד (סדר הקלט: oldest-first).
type Candle struct {
	Date   string
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume float64
}

// Indicator — אינדיקטור בודד עם ניקוד ואות.
type Indicator struct {
	Key    string  `json:"key"`
	Name   string  `json:"name"`
	Weight float64 `json:"weight"`
	Score  float64 `json:"score"`  // [-1,1]
	Signal string  `json:"signal"` // buy / neutral / sell
	Detail string  `json:"detail"`
}

// Agreement — כמה אינדיקטורים בעד/נגד/ניטרלי.
type Agreement struct {
	Buy     int `json:"buy"`
	Neutral int `json:"neutral"`
	Sell    int `json:"sell"`
	Total   int `json:"total"`
}

// Result — תוצאת הניתוח המלאה (מבנה ה-JSON שהפרונט מצפה לו).
type Result struct {
	OK             bool        `json:"ok"`
	Reason         string      `json:"reason,omitempty"`
	Symbol         string      `json:"symbol,omitempty"`
	Name           string      `json:"name,omitempty"`
	Price          float64     `json:"price"`
	PrevClose      float64     `json:"prevClose"`
	Change         float64     `json:"change"`
	ChangePct      float64     `json:"changePct"`
	Score100       int         `json:"score100"`
	Composite      float64     `json:"composite"`
	Recommendation string      `json:"recommendation"`
	RecoKey        string      `json:"recoKey"`
	Agreement      Agreement   `json:"agreement"`
	Indicators     []Indicator `json:"indicators"`
}

// ---------- עזרי חישוב ----------

func smaAt(v []float64, period, end int) (float64, bool) {
	if end-period+1 < 0 {
		return 0, false
	}
	sum := 0.0
	for i := end - period + 1; i <= end; i++ {
		sum += v[i]
	}
	return sum / float64(period), true
}

func sma(v []float64, period int) (float64, bool) {
	return smaAt(v, period, len(v)-1)
}

func stdevAt(v []float64, period, end int) (float64, bool) {
	mean, ok := smaAt(v, period, end)
	if !ok {
		return 0, false
	}
	sq := 0.0
	for i := end - period + 1; i <= end; i++ {
		d := v[i] - mean
		sq += d * d
	}
	return math.Sqrt(sq / float64(period)), true
}

// emaSeries — EMA מלא באורך הקלט (ערכים ראשוניים = NaN עד ההזרעה).
func emaSeries(v []float64, period int) []float64 {
	k := 2.0 / (float64(period) + 1.0)
	out := make([]float64, len(v))
	for i := range out {
		out[i] = math.NaN()
	}
	if len(v) < period {
		return out
	}
	prev, _ := smaAt(v, period, period-1)
	out[period-1] = prev
	for i := period; i < len(v); i++ {
		prev = v[i]*k + prev*(1-k)
		out[i] = prev
	}
	return out
}

func clamp(x, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, x)) }

// bandScore — ניקוד מנוגד עם אזור מת: +1 בקצה נמוך, 0 באמצע, -1 בקצה גבוה.
func bandScore(x, lowExt, lowNeu, highNeu, highExt float64) float64 {
	if x <= lowExt {
		return 1
	}
	if x < lowNeu {
		return (lowNeu - x) / (lowNeu - lowExt)
	}
	if x <= highNeu {
		return 0
	}
	if x < highExt {
		return -(x - highNeu) / (highExt - highNeu)
	}
	return -1
}

func labelFromScore(s float64) string {
	if s >= 0.33 {
		return "buy"
	}
	if s <= -0.33 {
		return "sell"
	}
	return "neutral"
}

// ---------- אינדיקטורים ----------

func rsi(closes []float64, period int) (float64, bool) {
	if len(closes) < period+1 {
		return 0, false
	}
	gain, loss := 0.0, 0.0
	for i := 1; i <= period; i++ {
		d := closes[i] - closes[i-1]
		if d >= 0 {
			gain += d
		} else {
			loss -= d
		}
	}
	avgGain := gain / float64(period)
	avgLoss := loss / float64(period)
	for i := period + 1; i < len(closes); i++ {
		d := closes[i] - closes[i-1]
		g, l := 0.0, 0.0
		if d > 0 {
			g = d
		} else if d < 0 {
			l = -d
		}
		avgGain = (avgGain*float64(period-1) + g) / float64(period)
		avgLoss = (avgLoss*float64(period-1) + l) / float64(period)
	}
	if avgLoss == 0 {
		return 100, true
	}
	rs := avgGain / avgLoss
	return 100 - 100/(1+rs), true
}

type macdResult struct {
	macd, signal, hist, histPrev float64
	hasPrev                      bool
}

func macd(closes []float64, fast, slow, signal int) (macdResult, bool) {
	if len(closes) < slow+signal {
		return macdResult{}, false
	}
	emaFast := emaSeries(closes, fast)
	emaSlow := emaSeries(closes, slow)
	macdLine := make([]float64, len(closes))
	macdVals := make([]float64, 0, len(closes))
	for i := range closes {
		if !math.IsNaN(emaFast[i]) && !math.IsNaN(emaSlow[i]) {
			macdLine[i] = emaFast[i] - emaSlow[i]
			macdVals = append(macdVals, macdLine[i])
		} else {
			macdLine[i] = math.NaN()
		}
	}
	sigSeries := emaSeries(macdVals, signal)
	signalLine := sigSeries[len(sigSeries)-1]
	macdNow := macdLine[len(macdLine)-1]
	macdPrev := macdLine[len(macdLine)-2]
	sigPrev := sigSeries[len(sigSeries)-2]
	res := macdResult{macd: macdNow, signal: signalLine, hist: macdNow - signalLine}
	if !math.IsNaN(macdPrev) && !math.IsNaN(sigPrev) {
		res.histPrev = macdPrev - sigPrev
		res.hasPrev = true
	}
	return res, true
}

func stochastic(highs, lows, closes []float64, kPeriod, dPeriod int) (k, d float64, ok bool) {
	if len(closes) < kPeriod+dPeriod {
		return 0, 0, false
	}
	kArr := make([]float64, 0, len(closes))
	for i := kPeriod - 1; i < len(closes); i++ {
		hh, ll := math.Inf(-1), math.Inf(1)
		for j := i - kPeriod + 1; j <= i; j++ {
			if highs[j] > hh {
				hh = highs[j]
			}
			if lows[j] < ll {
				ll = lows[j]
			}
		}
		if hh == ll {
			kArr = append(kArr, 50)
		} else {
			kArr = append(kArr, 100*(closes[i]-ll)/(hh-ll))
		}
	}
	kVal := kArr[len(kArr)-1]
	dVal, _ := smaAt(kArr, dPeriod, len(kArr)-1)
	return kVal, dVal, true
}

type bollResult struct{ mid, upper, lower, pctB float64 }

func bollinger(closes []float64, period int, mult float64) (bollResult, bool) {
	if len(closes) < period {
		return bollResult{}, false
	}
	mid, _ := sma(closes, period)
	sd, _ := stdevAt(closes, period, len(closes)-1)
	upper := mid + mult*sd
	lower := mid - mult*sd
	price := closes[len(closes)-1]
	pctB := 0.5
	if upper != lower {
		pctB = (price - lower) / (upper - lower)
	}
	return bollResult{mid, upper, lower, pctB}, true
}

func obvTrend(closes, volumes []float64) (rising bool, ok bool) {
	if len(closes) < 22 {
		return false, false
	}
	obv := make([]float64, len(closes))
	obv[0] = 0
	for i := 1; i < len(closes); i++ {
		switch {
		case closes[i] > closes[i-1]:
			obv[i] = obv[i-1] + volumes[i]
		case closes[i] < closes[i-1]:
			obv[i] = obv[i-1] - volumes[i]
		default:
			obv[i] = obv[i-1]
		}
	}
	obvNow := obv[len(obv)-1]
	obvMa, _ := smaAt(obv, 20, len(obv)-1)
	return obvNow > obvMa, true
}

func momentum12m(closes []float64) (ret float64, approx bool, ok bool) {
	n := len(closes)
	if n < 252 {
		if n < 40 {
			return 0, false, false
		}
		return closes[n-1]/closes[0] - 1, true, true
	}
	end := closes[n-1-21]
	start := closes[n-1-252]
	return end/start - 1, false, true
}

// ---------- החישוב הראשי ----------

func Analyze(candles []Candle) Result {
	if len(candles) < 40 {
		return Result{OK: false, Reason: "אין מספיק היסטוריה (צריך לפחות ~40 ימי מסחר)."}
	}
	n := len(candles)
	closes := make([]float64, n)
	highs := make([]float64, n)
	lows := make([]float64, n)
	vols := make([]float64, n)
	for i, c := range candles {
		closes[i] = c.Close
		highs[i] = c.High
		lows[i] = c.Low
		vols[i] = c.Volume
	}
	price := closes[n-1]

	var inds []Indicator

	sma200, ok200 := sma(closes, 200)
	sma150, _ := sma(closes, 150)
	if ok200 {
		dist := price/sma200 - 1
		score := clamp(dist/0.10, -1, 1)
		pos := "מתחת"
		if dist >= 0 {
			pos = "מעל"
		}
		inds = append(inds, Indicator{"sma200", "מחיר מול ממוצע 200 יום", 2.0, score, labelFromScore(score),
			fmt.Sprintf("%s הממוצע ב-%.1f%%", pos, math.Abs(dist)*100)})
		_ = sma150
	}

	sma50, ok50 := sma(closes, 50)
	if ok50 && ok200 {
		gap := sma50/sma200 - 1
		score := clamp(gap/0.05, -1, 1)
		detail := "הקצר מתחת לארוך — מגמת ירידה"
		if sma50 >= sma200 {
			detail = "הקצר מעל הארוך — מגמת עלייה"
		}
		inds = append(inds, Indicator{"goldencross", "צלב זהב (ממוצע 50 מול 200)", 2.0, score, labelFromScore(score), detail})
	}

	if ret, approx, ok := momentum12m(closes); ok {
		score := clamp(ret/0.30, -1, 1)
		name := "מומנטום 12 חודשים"
		if approx {
			name += " (מקורב)"
		}
		inds = append(inds, Indicator{"momentum", name, 2.0, score, labelFromScore(score),
			fmt.Sprintf("תשואה של %.1f%% בשנה האחרונה", ret*100)})
	}

	if m, ok := macd(closes, 12, 26, 9); ok {
		score := 0.6
		if m.hist <= 0 {
			score = -0.6
		}
		if m.hasPrev && math.Signbit(m.hist) != math.Signbit(m.histPrev) {
			if m.hist > 0 {
				score = 1
			} else {
				score = -1
			}
		}
		detail := "מומנטום שלילי (קו מתחת לאות)"
		if m.hist > 0 {
			detail = "מומנטום חיובי (קו מעל האות)"
		}
		inds = append(inds, Indicator{"macd", "MACD", 1.5, score, labelFromScore(score), detail})
	}

	if r, ok := rsi(closes, 14); ok {
		score := bandScore(r, 30, 40, 60, 70)
		var detail string
		switch {
		case r < 30:
			detail = fmt.Sprintf("מכירת יתר (%.0f) — הזדמנות קנייה", r)
		case r > 70:
			detail = fmt.Sprintf("קניית יתר (%.0f) — סיכון לתיקון", r)
		case r > 60:
			detail = fmt.Sprintf("נוטה לקניית יתר (%.0f)", r)
		case r < 40:
			detail = fmt.Sprintf("נוטה למכירת יתר (%.0f)", r)
		default:
			detail = fmt.Sprintf("ניטרלי (%.0f)", r)
		}
		inds = append(inds, Indicator{"rsi", "RSI (עוצמה יחסית)", 1.0, score, labelFromScore(score), detail})
	}

	if k, _, ok := stochastic(highs, lows, closes, 14, 3); ok {
		score := bandScore(k, 20, 30, 70, 80)
		var detail string
		switch {
		case k < 20:
			detail = fmt.Sprintf("מכירת יתר (%.0f) — הזדמנות", k)
		case k > 80:
			detail = fmt.Sprintf("קניית יתר (%.0f)", k)
		default:
			detail = fmt.Sprintf("ניטרלי (%.0f)", k)
		}
		inds = append(inds, Indicator{"stoch", "סטוכסטי", 1.0, score, labelFromScore(score), detail})
	}

	if bb, ok := bollinger(closes, 20, 2); ok {
		score := bandScore(bb.pctB, 0, 0.2, 0.8, 1)
		var detail string
		switch {
		case bb.pctB < 0.05:
			detail = "ברצועה התחתונה — מתוח כלפי מטה"
		case bb.pctB > 0.95:
			detail = "ברצועה העליונה — מתוח כלפי מעלה"
		default:
			detail = "בתוך הרצועה הרגילה"
		}
		inds = append(inds, Indicator{"bollinger", "רצועות בולינגר", 1.0, score, labelFromScore(score), detail})
	}

	if rising, ok := obvTrend(closes, vols); ok {
		score := -0.6
		detail := "כסף יוצא מהמניה"
		if rising {
			score = 0.6
			detail = "כסף נכנס למניה"
		}
		inds = append(inds, Indicator{"obv", "נפח וזרימת כסף (OBV)", 1.0, score, labelFromScore(score), detail})
	}

	// שקלול
	wsum, wtot := 0.0, 0.0
	buys, sells := 0, 0
	for _, ind := range inds {
		wsum += ind.Score * ind.Weight
		wtot += ind.Weight
		if ind.Signal == "buy" {
			buys++
		} else if ind.Signal == "sell" {
			sells++
		}
	}
	composite := 0.0
	if wtot > 0 {
		composite = wsum / wtot
	}
	score100 := int(math.Round((composite + 1) * 50))

	reco, recoKey := recommendation(score100)

	return Result{
		OK:             true,
		Price:          price,
		Score100:       score100,
		Composite:      composite,
		Recommendation: reco,
		RecoKey:        recoKey,
		Agreement:      Agreement{Buy: buys, Sell: sells, Neutral: len(inds) - buys - sells, Total: len(inds)},
		Indicators:     inds,
	}
}

func recommendation(s int) (string, string) {
	switch {
	case s >= 70:
		return "קנייה", "buy"
	case s >= 55:
		return "נטייה לקנייה", "weakbuy"
	case s > 45:
		return "החזקה / המתנה", "hold"
	case s > 30:
		return "נטייה למכירה", "weaksell"
	default:
		return "מכירה", "sell"
	}
}
