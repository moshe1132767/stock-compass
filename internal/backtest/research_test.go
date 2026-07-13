package backtest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"stockcompass/internal/indicators"
)

// מגרש המחקר: מודד איזה הרכב ציון באמת מנצח, על עשרות מניות ובשני חלונות זמן נפרדים.
//
// הכללים שאני מציב לעצמי כאן, כדי שהתשובה תהיה כנה ולא "התאמה לנתונים":
//  1. ההרכבים נקבעים מראש מתוך תיאוריה (מומנטום, מגמה) — לא חיפוש עיוור על אלפי שילובים.
//  2. כל הרכב נבחן על אותן 63 מניות, כולל כאלה שקרסו (אינטל, פייפאל, בואינג, פורד...).
//  3. נמדדים שני חלונות זמן נפרדים: 5 השנים האחרונות, וחמש השנים שלפניהן.
//     הרכב שמנצח רק באחד מהם — לא באמת מנצח.
//  4. מודדים גם סיכון (הנפילה הכי גדולה בדרך), לא רק תשואה.
//
// RESEARCH=1 DATA=<תיקיית *_all.json> go test ./internal/backtest -run TestResearch -v -timeout 30m
func TestResearch(t *testing.T) {
	if os.Getenv("RESEARCH") == "" {
		t.Skip("מגרש מחקר — הרץ עם RESEARCH=1 ו-DATA=<תיקייה>")
	}
	files, _ := filepath.Glob(os.Getenv("DATA") + "/*_all.json")
	if len(files) == 0 {
		t.Fatal("לא נמצאו קבצי נרות")
	}

	type profile struct {
		name string
		w    indicators.Weights
	}
	profiles := []profile{
		{"נוכחי (8 אינדיקטורים)", indicators.Default},
		{"בלי הנוגדים למגמה", indicators.Weights{SMA200: 2, GoldenCross: 2, Momentum: 2, MACD: 1.5, OBV: 1}},
		{"נוגדים במשקל רבע", indicators.Weights{SMA200: 2, GoldenCross: 2, Momentum: 2, MACD: 1.5,
			RSI: 0.25, Stoch: 0.25, Bollinger: 0.25, OBV: 1}},
		{"מומנטום + מגמה בלבד", indicators.Weights{SMA200: 3, GoldenCross: 2, Momentum: 3}},
		{"מומנטום דומיננטי", indicators.Weights{SMA200: 2, GoldenCross: 1, Momentum: 4, MACD: 1}},
		{"רק ממוצע 200 (פייבר)", indicators.Weights{SMA200: 1}},
	}

	type row struct{ strat, hold, dd, holdDD, trades, timeIn float64 }
	// results[profile][window] = כל המניות
	results := make([][2][]row, len(profiles))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)

	for _, f := range files {
		wg.Add(1)
		go func(f string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			b, err := os.ReadFile(f)
			if err != nil {
				return
			}
			var cs []indicators.Candle
			if json.Unmarshal(b, &cs) != nil || len(cs) < 1600 {
				return
			}
			// שני חלונות: הנתונים המלאים (5 שנים אחרונות), ואותם נתונים חתוכים
			// ב-1260 ימי מסחר — כך שה"5 שנים" מכסות את התקופה הקודמת.
			windows := [2][]indicators.Candle{cs, nil}
			if len(cs) > 1260+1600 {
				windows[1] = cs[:len(cs)-1260]
			}

			for pi, p := range profiles {
				for wi, win := range windows {
					if win == nil {
						continue
					}
					res := RunWith(win, p.w)
					for _, per := range res.Periods {
						if per.Key != "5y" || !per.Available {
							continue
						}
						mu.Lock()
						results[pi][wi] = append(results[pi][wi], row{
							per.Strategy, per.BuyHold, per.MaxDD, per.HoldDD,
							float64(per.Trades), per.TimeIn,
						})
						mu.Unlock()
					}
				}
			}
		}(f)
	}
	wg.Wait()

	med := func(v []float64) float64 {
		if len(v) == 0 {
			return 0
		}
		s := append([]float64(nil), v...)
		sort.Float64s(s)
		return s[len(s)/2]
	}

	for wi, title := range []string{"5 השנים האחרונות", "5 השנים שלפניהן (מחוץ למדגם)"} {
		fmt.Printf("\n══════ %s ══════\n", title)
		fmt.Printf("%-24s %10s %10s %9s %10s %9s %8s\n",
			"הרכב הציון", "תשואה", "קנה־החזק", "ניצח ב־", "נפילה", "מול החזקה", "פעולות")
		for pi, p := range profiles {
			rs := results[pi][wi]
			if len(rs) == 0 {
				continue
			}
			var st, hd, dd, hdd, tr []float64
			wins := 0
			for _, r := range rs {
				st = append(st, r.strat)
				hd = append(hd, r.hold)
				dd = append(dd, r.dd)
				hdd = append(hdd, r.holdDD)
				tr = append(tr, r.trades)
				if r.strat > r.hold {
					wins++
				}
			}
			fmt.Printf("%-24s %9.0f%% %9.0f%% %6d/%-3d %9.0f%% %9.0f%% %8.0f\n",
				p.name, med(st), med(hd), wins, len(rs), med(dd), med(hdd), med(tr))
		}
	}
	fmt.Println("\n(חציון על", len(files), "מניות. 'ניצח ב־' = בכמה מניות ההמלצות היכו קנה־והחזק.)")
	fmt.Println(strings.Repeat("─", 60))
}
