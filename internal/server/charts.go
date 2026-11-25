package server

import (
	"fmt"
	"html"
	"strings"
	"time"

	"seatime/internal/server/store"
	"seatime/pkg/ledger"
)

// All charts are server-rendered inline SVG styled by CSS classes (dark/light safe).

func dayLabel(d int64) string { return time.Unix(d*86400, 0).UTC().Format("Mon Jan 2") }

func heatLevel(sec float64) int {
	switch {
	case sec < 60:
		return 0
	case sec < 15*60:
		return 1
	case sec < 60*60:
		return 2
	case sec < 150*60:
		return 3
	}
	return 4
}

// heatmap draws `weeks` Monday-first columns ending with today's: one
// rounded square per day, deeper blue for more counted time.
func heatmap(days map[int64]store.DaySum, today int64, weeks int) string {
	const cell, gap, left, top = 14, 3, 32, 18
	start := today - (today+3)%7 - int64(7*(weeks-1))
	w, h := left+weeks*(cell+gap), top+7*(cell+gap)
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="heat" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-label="Counted time per day, last %d weeks">`, w, h, w, h, weeks)
	for i, l := range []string{"Mon", "Wed", "Fri"} {
		fmt.Fprintf(&b, `<text x="0" y="%d" class="axis">%s</text>`, top+10+i*2*(cell+gap), l)
	}
	lastMonth, lastCol := "", -9
	for d := start; d <= today; d++ {
		col, row := int(d-start)/7, int(d-start)%7
		if row == 0 {
			if m := time.Unix(d*86400, 0).UTC().Format("Jan"); m != lastMonth {
				if col-lastCol >= 3 { // a month that starts a week or two in would collide with the one before
					fmt.Fprintf(&b, `<text x="%d" y="9" class="axis">%s</text>`, left+col*(cell+gap), m)
					lastCol = col
				}
				lastMonth = m
			}
		}
		ds := days[d]
		tip := dayLabel(d) + ": nothing logged"
		if ds.Struggle > 0 {
			tip = dayLabel(d) + ": " + fmtDur(ds.Struggle)
		}
		fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="%d" rx="3.5" class="h%d"><title>%s</title></rect>`,
			left+col*(cell+gap), top+row*(cell+gap), cell, cell, heatLevel(ds.Struggle), html.EscapeString(tip))
	}
	b.WriteString(`</svg>`)
	return b.String()
}

// dailyBars draws the last n days as rounded bars of counted time against
// an hour scale.
func dailyBars(days map[int64]store.DaySum, today int64, n int) string {
	const h, top, left = 96, 10, 30
	bw, slot, cls := 14, 48, "bars"
	switch {
	case n > 31:
		bw, slot, cls = 5, 8, "bars wide"
	case n > 14:
		slot, cls = 31, "bars wide"
	}
	max := 3600.0
	for d := today - int64(n) + 1; d <= today; d++ {
		if t := days[d].Struggle; t > max {
			max = t
		}
	}
	step := 3600.0 // one scale line an hour, fewer when the days run long
	for max/step > 4 {
		step *= 2
	}
	max = float64(int((max+step-1)/step)) * step
	w := left + n*slot
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="%s" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-label="Counted time per day, last %d days">`, cls, w, h+top+20, w, h+top+20, n)
	for v := step; v <= max; v += step {
		y := float64(top+h) - v/max*h
		fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" class="grid"/><text x="0" y="%.1f" class="axis">%.0fh</text>`, left, y, w, y, y+3.5, v/3600)
	}
	for i := 0; i < n; i++ {
		d := today - int64(n) + 1 + int64(i)
		ds := days[d]
		x := left + i*slot
		bx := x + (slot-bw)/2
		hs := ds.Struggle / max * h
		tip := fmt.Sprintf("%s: %s", dayLabel(d), fmtDur(ds.Struggle))
		rx := min(6, slot/2)
		fmt.Fprintf(&b, `<g><title>%s</title><rect x="%d" y="%d" width="%d" height="%d" rx="%d" class="bg"/>`, html.EscapeString(tip), x, top, slot, h, rx)
		if hs > 0 {
			fmt.Fprintf(&b, `<rect x="%d" y="%.1f" width="%d" height="%.1f" rx="%d" class="bs"/>`, bx, top+h-hs, bw, hs, min(3, bw/2))
		}
		lbl, lc := time.Unix(d*86400, 0).UTC().Format("Mon")[:1], "axis"
		if n > 14 {
			lbl = time.Unix(d*86400, 0).UTC().Format("2")
			if n > 31 && time.Unix(d*86400, 0).UTC().Day() != 1 {
				lbl = ""
			} else if n > 31 {
				lbl = time.Unix(d*86400, 0).UTC().Format("Jan")
			}
		}
		if i == n-1 {
			lc = "axis now"
		}
		fmt.Fprintf(&b, `<text x="%d" y="%d" class="%s" text-anchor="middle">%s</text></g>`, x+slot/2, h+top+15, lc, lbl)
	}
	fmt.Fprintf(&b, `<line x1="%d" y1="%d" x2="%d" y2="%d" class="base"/></svg>`, left, top+h, w, top+h)
	return b.String()
}

type commitMark struct {
	At  time.Time
	SHA string
	URL string
}

// dayTimeline draws 5-minute columns for one local day with commits marked.
func dayTimeline(bs []ledger.Bucket, marks []commitMark, dayStart time.Time) string {
	const cols, cw, h, top, left = 288, 3, 100, 14, 4
	var s [cols]float64
	for _, b := range bs {
		i := int(b.Start.Sub(dayStart) / (5 * time.Minute))
		if i >= 0 && i < cols {
			s[i] += b.Seconds()
		}
	}
	w := left*2 + cols*cw
	var out strings.Builder
	fmt.Fprintf(&out, `<svg class="timeline" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-label="The day in 5-minute columns: counted time and commits">`, w, h+top+18, w, h+top+18)
	for hr := 0; hr <= 24; hr += 3 {
		x := left + hr*12*cw
		fmt.Fprintf(&out, `<line x1="%d" y1="%d" x2="%d" y2="%d" class="grid"/>`, x, top, x, top+h)
		anchor := "middle"
		if hr == 0 {
			anchor = "start"
		} else if hr == 24 {
			anchor = "end"
		}
		fmt.Fprintf(&out, `<text x="%d" y="%d" class="axis" text-anchor="%s">%02d:00</text>`, x, top+h+13, anchor, hr%24)
	}
	fmt.Fprintf(&out, `<line x1="%d" y1="%d" x2="%d" y2="%d" class="base"/>`, left, top+h, left+cols*cw, top+h)
	for i := 0; i < cols; i++ {
		if s[i] == 0 {
			continue
		}
		x := left + i*cw
		hs := s[i] / 300 * h
		t := dayStart.Add(time.Duration(i) * 5 * time.Minute)
		fmt.Fprintf(&out, `<g><title>%s</title><rect x="%d" y="%.1f" width="%d" height="%.1f" class="bs"/></g>`,
			html.EscapeString(t.Format("15:04")+": "+fmtDur(s[i])), x, top+h-hs, cw, hs)
	}
	for _, m := range marks {
		i := int(m.At.Sub(dayStart) / (5 * time.Minute))
		if i < 0 || i >= cols {
			continue
		}
		x := left + i*cw + cw/2
		tip := "commit " + short(m.SHA)
		open, closeTag := "", ""
		if m.URL != "" {
			open, closeTag = fmt.Sprintf(`<a href="%s" rel="noopener">`, html.EscapeString(m.URL)), `</a>`
		}
		fmt.Fprintf(&out, `%s<g class="cm"><title>%s</title><line x1="%d" y1="%d" x2="%d" y2="%d"/><circle cx="%d" cy="%d" r="3.5"/></g>%s`,
			open, html.EscapeString(tip), x, top, x, top+h, x, top-4, closeTag)
	}
	out.WriteString(`</svg>`)
	return out.String()
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
