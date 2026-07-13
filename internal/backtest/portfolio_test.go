package backtest

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"stockcompass/internal/indicators"
)

// מגרש מחקר שני: האסטרטגיות שהמחקר של המשתמש מצביע עליהן.
//
// ההבדל המהותי מהציון של האפליקציה: כאן לא "נכנסים ויוצאים" ממניה בודדת לפי ציון יומי,
// אלא *מדרגים* יקום שלם של מניות לפי מומנטום 12-1 ומחזיקים את החזקות ביותר —
// עם מתג של השוק כולו (SPY מעל/מתחת לממוצע 200) שקובע אם להיות בשוק בכלל.
//
// PORTFOLIO=1 DATA=<תיקייה> go test ./internal/backtest -run TestPortfolio -v -timeout 20m
func TestPortfolio(t *testing.T) {
	if os.Getenv("PORTFOLIO") == "" {
		t.Skip("מגרש מחקר — הרץ עם PORTFOLIO=1 ו-DATA=<תיקייה>")
	}
	dir := os.Getenv("DATA")
	files, _ := filepath.Glob(dir + "/*_all.json")
	if len(files) == 0 {
		t.Fatal("לא נמצאו קבצי נרות")
	}

	// טוענים את כל המניות
	series := map[string]map[string]float64{} // סימול → תאריך → מחיר
	for _, f := range files {
		sym := strings.TrimSuffix(filepath.Base(f), "_all.json")
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var cs []indicators.Candle
		if json.Unmarshal(b, &cs) != nil {
			continue
		}
		m := make(map[string]float64, len(cs))
		for _, c := range cs {
			m[c.Date] = c.Close
		}
		series[sym] = m
	}
	spy := series["SPY"]
	if spy == nil {
		t.Fatal("צריך את SPY בתור לוח השנה ונקודת הייחוס")
	}

	// לוח השנה = ימי המסחר של SPY
	cal := make([]string, 0, len(spy))
	for d := range spy {
		cal = append(cal, d)
	}
	sort.Strings(cal)

	// מיישרים כל מניה ללוח השנה (השלמה קדימה: מחיר אחרון ידוע)
	syms := make([]string, 0, len(series))
	for s := range series {
		if s != "SPY" && s != "QQQ" { // אלה סלים, לא מניות — לא נכנסים ליקום
			syms = append(syms, s)
		}
	}
	sort.Strings(syms)

	px := map[string][]float64{} // סימול → מחיר לכל יום בלוח (0 = עוד לא נסחרה)
	for _, s := range syms {
		row := make([]float64, len(cal))
		last := 0.0
		for i, d := range cal {
			if p, ok := series[s][d]; ok {
				last = p
			}
			row[i] = last
		}
		px[s] = row
	}
	spyPx := make([]float64, len(cal))
	for i, d := range cal {
		spyPx[i] = spy[d]
	}

	sma := func(v []float64, end, n int) float64 {
		if end-n+1 < 0 {
			return 0
		}
		s := 0.0
		for i := end - n + 1; i <= end; i++ {
			s += v[i]
		}
		return s / float64(n)
	}
	maxDD := func(eq []float64) float64 {
		peak, dd := 0.0, 0.0
		for _, v := range eq {
			peak = math.Max(peak, v)
			if peak > 0 {
				dd = math.Max(dd, (peak-v)/peak)
			}
		}
		return dd * 100
	}

	// טווח הבדיקה: 10 שנים אחרונות, אבל מתחילים רק אחרי שיש מספיק היסטוריה למומנטום
	start := len(cal) - 2520
	if start < 273 {
		start = 273
	}

	// run — מריץ אסטרטגיית תיק: כל rebal ימי מסחר מדרגים לפי מומנטום 12-1,
	// מחזיקים את topN בחלקים שווים. trendFilter → יוצאים למזומן כשה-SPY מתחת לממוצע 200.
	run := func(from, to, topN, rebal int, trendFilter, dailyCheck bool) (ret, dd, cagr float64, turns int) {
		start, end := from, to
		eq := 1.0
		curve := make([]float64, 0, len(cal)-start)
		var holds []string
		shares := map[string]float64{}
		cash := true

		for i := start; i < end; i++ {
			// שווי התיק היום
			if !cash && len(holds) > 0 {
				v := 0.0
				for _, s := range holds {
					v += shares[s] * px[s][i]
				}
				eq = v
			}
			curve = append(curve, eq)

			// מתג יומי: ברגע שהשוק שובר את הממוצע — החוצה, בלי לחכות ליום האיזון
			if dailyCheck && !cash && spyPx[i] <= sma(spyPx, i, 200) {
				cash, holds, shares = true, nil, map[string]float64{}
				continue
			}
			if (i-start)%rebal != 0 {
				continue
			}
			// יום איזון: מדרגים לפי מומנטום 12-1
			inMarket := true
			if trendFilter {
				inMarket = spyPx[i] > sma(spyPx, i, 200)
			}
			if !inMarket {
				cash, holds, shares = true, nil, map[string]float64{}
				continue
			}
			type sc struct {
				sym string
				mom float64
			}
			var ranked []sc
			for _, s := range syms {
				if i-252 < 0 || px[s][i-252] <= 0 || px[s][i-21] <= 0 || px[s][i] <= 0 {
					continue // אין מספיק היסטוריה — לא משתתפת
				}
				ranked = append(ranked, sc{s, px[s][i-21]/px[s][i-252] - 1})
			}
			if len(ranked) < topN {
				continue
			}
			sort.Slice(ranked, func(a, b int) bool { return ranked[a].mom > ranked[b].mom })

			newHolds := make([]string, 0, topN)
			for _, r := range ranked[:topN] {
				newHolds = append(newHolds, r.sym)
			}
			for _, s := range newHolds { // כמה מהתיק התחלף
				found := false
				for _, o := range holds {
					if o == s {
						found = true
					}
				}
				if !found {
					turns++
				}
			}
			shares = map[string]float64{}
			for _, s := range newHolds {
				shares[s] = (eq / float64(topN)) / px[s][i]
			}
			holds, cash = newHolds, false
		}
		years := float64(end-start) / 252.0
		return (eq - 1) * 100, maxDD(curve), (math.Pow(eq, 1/years) - 1) * 100, turns
	}

	// נקודות ייחוס לחלון נתון
	bench := func(from, to int) (spyRet, spyCagr, spyDD, ewRet, ewCagr, ewDD float64) {
		yrs := float64(to-from) / 252.0
		sc := []float64{}
		for i := from; i < to; i++ {
			sc = append(sc, spyPx[i]/spyPx[from])
		}
		spyRet = (sc[len(sc)-1] - 1) * 100
		spyCagr = (math.Pow(sc[len(sc)-1], 1/yrs) - 1) * 100
		spyDD = maxDD(sc)

		ec := []float64{}
		for i := from; i < to; i++ {
			v, n := 0.0, 0
			for _, s := range syms {
				if px[s][from] > 0 && px[s][i] > 0 {
					v += px[s][i] / px[s][from]
					n++
				}
			}
			ec = append(ec, v/float64(n))
		}
		ewRet = (ec[len(ec)-1] - 1) * 100
		ewCagr = (math.Pow(ec[len(ec)-1], 1/yrs) - 1) * 100
		ewDD = maxDD(ec)
		return
	}

	mid := len(cal) - 1260
	windows := []struct {
		name     string
		from, to int
	}{
		{"5 השנים האחרונות", mid, len(cal)},
		{"5 השנים שלפניהן (מחוץ למדגם)", start, mid},
	}

	strats := []struct {
		name             string
		top, rebal       int
		filter, dailyChk bool
	}{
		{"מומנטום 12-1: 10 החזקות, רבעוני", 10, 63, false, false},
		{"מומנטום 12-1: 10 החזקות, חודשי", 10, 21, false, false},
		{"מומנטום 12-1: 5 החזקות, רבעוני", 5, 63, false, false},
		{"מומנטום 12-1: 20 החזקות, רבעוני", 20, 63, false, false},
		{"מומנטום כפול (מתג ביום איזון)", 10, 63, true, false},
		{"מומנטום כפול (מתג נבדק כל יום)", 10, 63, true, true},
	}

	for _, w := range windows {
		sr, sc, sd, er, ec, ed := bench(w.from, w.to)
		fmt.Printf("\n══════ %s  (%s → %s) ══════\n\n", w.name, cal[w.from], cal[w.to-1])
		fmt.Printf("%-36s %9s %8s %8s\n", "", "תשואה", "שנתי", "נפילה")
		fmt.Printf("%-36s %8.0f%% %7.1f%% %7.0f%%   ← המדד\n", "SPY (להחזיק את המדד)", sr, sc, sd)
		fmt.Printf("%-36s %8.0f%% %7.1f%% %7.0f%%   ← הרף האמיתי (היקום שלנו)\n", "כל היקום בחלקים שווים", er, ec, ed)
		fmt.Println(strings.Repeat("-", 72))
		for _, st := range strats {
			r, dd, cagr, _ := run(w.from, w.to, st.top, st.rebal, st.filter, st.dailyChk)
			mark := "  "
			if cagr > ec {
				mark = "✔ "
			}
			fmt.Printf("%s%-34s %8.0f%% %7.1f%% %7.0f%%\n", mark, st.name, r, cagr, dd)
		}
	}
	fmt.Println("\n✔ = היכה את הרף האמיתי (היקום בחלקים שווים), לא רק את המדד.")
	fmt.Println("אזהרה כנה: היקום הוא 64 מניות ששרדו עד היום — הטיית שורדים שמנפחת את *כל* השורות.")
	fmt.Println("ההשוואה שמשמעותית היא בין האסטרטגיה לרף, לא המספר המוחלט.")
}
