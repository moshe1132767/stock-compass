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

// momentum12m — התשואה בפועל ב-12 החודשים האחרונים (252 ימי מסחר). זה המספר שמוצג
// למשתמש, כי זה מה שהוא רואה בכל אתר פיננסי וזה מה שמאפשר לו לוודא שהאפליקציה לא משקרת.
//
// months מחזיר כמה חודשים הנתון באמת מכסה (למניות צעירות שאין להן שנה שלמה).
func momentum12m(closes []float64) (ret float64, months float64, ok bool) {
	n := len(closes)
	if n < 40 {
		return 0, 0, false
	}
	back := 252
	if back > n-1 {
		back = n - 1
	}
	return closes[n-1]/closes[n-1-back] - 1, float64(back) / 21.0, true
}

// momentum12m1 — מומנטום "12 פחות 1": התשואה מלפני שנה ועד לפני חודש.
//
// זה הסיגנל שמניע את הציון, ולא התשואה המלאה. הסיבה אינה אסתטית: דילוג על החודש
// האחרון הוא בדיוק מה שמדד ג'גדיש-טיטמן (1993) מודד, והוא נשאר מובהק 30 שנה מחוץ
// למדגם וביותר מ-40 מדינות. החודש האחרון נוטה להיפוך קצר-טווח, ולכן הוא מוסיף רעש
// ולא אות. המספר המוצג למשתמש נשאר התשואה האמיתית — אבל מה שמכריע הוא זה.
func momentum12m1(closes []float64) (ret float64, ok bool) {
	n := len(closes)
	if n < 274 { // 252 + 21 + 1
		return 0, false
	}
	return closes[n-1-21]/closes[n-1-252] - 1, true
}

// ---------- הרכב הציון ----------

// Weights — כמה משקל יש לכל אינדיקטור בציון הסופי. משקל 0 = האינדיקטור עדיין מוצג
// למשתמש, אבל לא משפיע על ההמלצה.
//
// למה זה בכלל ניתן להחלפה: כדי שאפשר יהיה *למדוד* איזה הרכב באמת מנצח, על עשרות
// מניות ולאורך שנים, במקום להחליט לפי תחושה. ראה research_test.go.
type Weights struct {
	SMA200      float64
	GoldenCross float64
	Momentum    float64
	MACD        float64
	RSI         float64
	Stoch       float64
	Bollinger   float64
	OBV         float64
}

// Default — ההרכב שהאפליקציה משתמשת בו בפועל.
var Default = Weights{
	SMA200: 2, GoldenCross: 2, Momentum: 2, MACD: 1.5,
	RSI: 1, Stoch: 1, Bollinger: 1, OBV: 1,
}

// ---------- החישוב הראשי ----------

func Analyze(candles []Candle) Result { return AnalyzeWith(candles, Default) }

func AnalyzeWith(candles []Candle, w Weights) Result {
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
	if ok200 {
		dist := price/sma200 - 1
		score := clamp(dist/0.10, -1, 1)
		pos := "מתחת"
		if dist >= 0 {
			pos = "מעל"
		}
		inds = append(inds, Indicator{"sma200", "מחיר מול ממוצע 200 יום", w.SMA200, score, labelFromScore(score),
			fmt.Sprintf("%s הממוצע ב-%.1f%%", pos, math.Abs(dist)*100)})
	}

	sma50, ok50 := sma(closes, 50)
	if ok50 && ok200 {
		gap := sma50/sma200 - 1
		score := clamp(gap/0.05, -1, 1)
		detail := "הקצר מתחת לארוך — מגמת ירידה"
		if sma50 >= sma200 {
			detail = "הקצר מעל הארוך — מגמת עלייה"
		}
		inds = append(inds, Indicator{"goldencross", "צלב זהב (ממוצע 50 מול 200)", w.GoldenCross, score, labelFromScore(score), detail})
	}

	if ret, months, ok := momentum12m(closes); ok {
		// הציון נקבע לפי 12-1 (הסיגנל שיש לו ראיות), אבל המספר שמוצג הוא התשואה
		// האמיתית — כדי שהמשתמש יוכל לאמת אותו מול כל אתר.
		sig := ret
		name := "מומנטום 12 חודשים"
		detail := fmt.Sprintf("תשואה של %.1f%% בשנה האחרונה", ret*100)
		if s121, ok := momentum12m1(closes); ok {
			sig = s121
			detail = fmt.Sprintf("תשואה של %.1f%% בשנה האחרונה (הציון נקבע לפי %.1f%% — בלי החודש האחרון, שנוטה להיפוך)",
				ret*100, s121*100)
		} else if months < 11.5 { // מניה צעירה — לא מבטיחים שנה שלא קיימת
			name = fmt.Sprintf("מומנטום %.0f חודשים", months)
			detail = fmt.Sprintf("תשואה של %.1f%% מאז תחילת המסחר (%.0f חודשים בלבד)", ret*100, months)
		}
		score := clamp(sig/0.30, -1, 1)
		inds = append(inds, Indicator{"momentum", name, w.Momentum, score, labelFromScore(score), detail})
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
		inds = append(inds, Indicator{"macd", "MACD", w.MACD, score, labelFromScore(score), detail})
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
		inds = append(inds, Indicator{"rsi", "RSI (עוצמה יחסית)", w.RSI, score, labelFromScore(score), detail})
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
		inds = append(inds, Indicator{"stoch", "סטוכסטי", w.Stoch, score, labelFromScore(score), detail})
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
		inds = append(inds, Indicator{"bollinger", "רצועות בולינגר", w.Bollinger, score, labelFromScore(score), detail})
	}

	if rising, ok := obvTrend(closes, vols); ok {
		score := -0.6
		detail := "כסף יוצא מהמניה"
		if rising {
			score = 0.6
			detail = "כסף נכנס למניה"
		}
		inds = append(inds, Indicator{"obv", "נפח וזרימת כסף (OBV)", w.OBV, score, labelFromScore(score), detail})
	}

	// שקלול. אינדיקטור שמשקלו 0 עדיין מוצג למשתמש, אבל אינו מכריע ואינו נספר בהסכמה.
	wsum, wtot := 0.0, 0.0
	buys, sells, counted := 0, 0, 0
	for _, ind := range inds {
		if ind.Weight <= 0 {
			continue
		}
		wsum += ind.Score * ind.Weight
		wtot += ind.Weight
		counted++
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
		Agreement:      Agreement{Buy: buys, Sell: sells, Neutral: counted - buys - sells, Total: counted},
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
