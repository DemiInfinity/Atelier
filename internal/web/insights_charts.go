package web

// Insights charts: server-rendered inline SVG, no JavaScript. Every chart is
// an <svg role="img"> with a <title> and <desc>, a <title> on each mark for
// hover, and its numbers in a <details> data table. Text that must stay
// readable at any width (axis labels) is HTML beside the SVG, so the plot
// can stretch (preserveAspectRatio="none") without squashing letters.
// Colours come from classes in static/insights.css (CSP: no inline styles);
// SVG geometry attributes are fine.

import (
	"fmt"
	"html"
	"html/template"
	"math"
	"strings"
	"time"
)

var insEsc = html.EscapeString

// insNice rounds a maximum up to a tidy axis value (1, 2, 5 × 10^n).
func insNice(v float64) float64 {
	if v <= 0 {
		return 1
	}
	exp := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if m*exp >= v {
			return m * exp
		}
	}
	return 10 * exp
}

func insNum(v float64) string {
	switch {
	case math.Abs(v) >= 1_000_000:
		return trimZero(fmt.Sprintf("%.1f", v/1e6)) + "M"
	case math.Abs(v) >= 10_000:
		return trimZero(fmt.Sprintf("%.0f", v/1e3)) + "k"
	case math.Abs(v) >= 1000:
		return trimZero(fmt.Sprintf("%.1f", v/1e3)) + "k"
	case v == math.Trunc(v):
		return fmt.Sprintf("%.0f", v)
	}
	return trimZero(fmt.Sprintf("%.1f", v))
}

func trimZero(s string) string {
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// insColumns is a column chart: one value per column, or, when Down is set,
// a diverging chart with Up above the baseline and Down below it.
type insColumns struct {
	ID, Title, Desc string
	Labels          []string // one per column, for tooltips and the table
	Up              []float64
	Down            []float64 // optional
	UpName          string    // series names (table headers, legend)
	DownName        string
	UpClass         string // ins-c-commits, ins-c-add…
	DownClass       string
	Unit            string    // "commits", "lines"
	XTicks          [3]string // left, middle, right
	TableHead       string    // first column header
}

const (
	insColW = 728.0
	insColH = 180.0
)

func (c insColumns) HTML() template.HTML {
	n := len(c.Up)
	if n == 0 {
		return ""
	}
	maxV := 0.0
	for i := 0; i < n; i++ {
		maxV = math.Max(maxV, c.Up[i])
		if c.Down != nil {
			maxV = math.Max(maxV, c.Down[i])
		}
	}
	top := insNice(maxV)
	base := insColH // baseline y
	scale := insColH / top
	if c.Down != nil {
		base = insColH / 2
		scale = (insColH / 2) / top
	}
	slot := insColW / float64(n)
	gap := math.Min(2, slot*0.25)

	var b strings.Builder
	fmt.Fprintf(&b, `<figure class="ins-chart" id="%s">`, insEsc(c.ID))
	if c.Down != nil {
		fmt.Fprintf(&b, `<div class="ins-legend" aria-hidden="true"><span><i class="ins-swatch %s"></i>%s (up)</span><span><i class="ins-swatch %s"></i>%s (down)</span></div>`,
			insEsc(c.UpClass), insEsc(c.UpName), insEsc(c.DownClass), insEsc(c.DownName))
	}
	b.WriteString(`<div class="ins-plot"><div class="ins-yaxis" aria-hidden="true">`)
	if c.Down != nil {
		fmt.Fprintf(&b, `<span>+%s</span><span>0</span><span>−%s</span>`, insNum(top), insNum(top))
	} else {
		fmt.Fprintf(&b, `<span>%s</span><span>%s</span><span>0</span>`, insNum(top), insNum(top/2))
	}
	b.WriteString(`</div>`)
	fmt.Fprintf(&b, `<svg class="ins-svg" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" role="img" aria-labelledby="%s-t %s-d" focusable="false">`,
		insColW, insColH, insEsc(c.ID), insEsc(c.ID))
	fmt.Fprintf(&b, `<title id="%s-t">%s</title><desc id="%s-d">%s</desc>`, insEsc(c.ID), insEsc(c.Title), insEsc(c.ID), insEsc(c.Desc))
	for _, y := range []float64{0, insColH / 2, insColH} {
		cls := "ins-grid"
		if y == base {
			cls = "ins-base"
		}
		fmt.Fprintf(&b, `<line class="%s" x1="0" x2="%.0f" y1="%.1f" y2="%.1f" vector-effect="non-scaling-stroke"/>`, cls, insColW, y, y)
	}
	for i := 0; i < n; i++ {
		x := float64(i) * slot
		tip := c.Labels[i] + ": " + insNum(c.Up[i]) + " " + c.Unit
		if c.Down != nil {
			tip = fmt.Sprintf("%s: +%s / −%s %s", c.Labels[i], insNum(c.Up[i]), insNum(c.Down[i]), c.Unit)
		}
		fmt.Fprintf(&b, `<g class="ins-col"><title>%s</title><rect class="ins-hit" x="%.2f" y="0" width="%.2f" height="%.0f"/>`,
			insEsc(tip), x, slot, insColH)
		if h := c.Up[i] * scale; h > 0 {
			h = math.Max(h, 1.5)
			fmt.Fprintf(&b, `<rect class="ins-bar %s" x="%.2f" y="%.2f" width="%.2f" height="%.2f" rx="1.5"/>`,
				insEsc(c.UpClass), x+gap/2, base-h, slot-gap, h)
		}
		if c.Down != nil {
			if h := c.Down[i] * scale; h > 0 {
				h = math.Max(h, 1.5)
				fmt.Fprintf(&b, `<rect class="ins-bar %s" x="%.2f" y="%.2f" width="%.2f" height="%.2f" rx="1.5"/>`,
					insEsc(c.DownClass), x+gap/2, base, slot-gap, h)
			}
		}
		b.WriteString(`</g>`)
	}
	b.WriteString(`</svg></div>`)
	fmt.Fprintf(&b, `<div class="ins-xaxis" aria-hidden="true"><span>%s</span><span>%s</span><span>%s</span></div>`,
		insEsc(c.XTicks[0]), insEsc(c.XTicks[1]), insEsc(c.XTicks[2]))
	// Data table.
	b.WriteString(`<details class="ins-data"><summary>Show data table</summary><div class="ins-table-scroll"><table class="ins-table">`)
	fmt.Fprintf(&b, `<caption class="sr-only">%s</caption><thead><tr><th scope="col">%s</th><th scope="col" class="right">%s</th>`,
		insEsc(c.Title), insEsc(c.TableHead), insEsc(c.UpName))
	if c.Down != nil {
		fmt.Fprintf(&b, `<th scope="col" class="right">%s</th>`, insEsc(c.DownName))
	}
	b.WriteString(`</tr></thead><tbody>`)
	for i := n - 1; i >= 0; i-- { // newest first
		fmt.Fprintf(&b, `<tr><th scope="row">%s</th><td class="right">%.0f</td>`, insEsc(c.Labels[i]), c.Up[i])
		if c.Down != nil {
			fmt.Fprintf(&b, `<td class="right">%.0f</td>`, c.Down[i])
		}
		b.WriteString(`</tr>`)
	}
	b.WriteString(`</tbody></table></div></details></figure>`)
	return template.HTML(b.String())
}

// insSpark is a tiny column chart without axes (a contributor's weeks).
func insSpark(id, title string, values []float64) template.HTML {
	n := len(values)
	if n == 0 {
		return ""
	}
	maxV := 0.0
	total := 0.0
	for _, v := range values {
		maxV = math.Max(maxV, v)
		total += v
	}
	const w, h = 208.0, 32.0
	slot := w / float64(n)
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="ins-spark" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" role="img" aria-labelledby="%s-t" focusable="false"><title id="%s-t">%s: %s commits in %d weeks</title>`,
		w, h, insEsc(id), insEsc(id), insEsc(title), insNum(total), n)
	fmt.Fprintf(&b, `<line class="ins-base" x1="0" x2="%.0f" y1="%.0f" y2="%.0f" vector-effect="non-scaling-stroke"/>`, w, h, h)
	for i, v := range values {
		if v <= 0 || maxV == 0 {
			continue
		}
		bh := math.Max(v/maxV*h, 1.5)
		fmt.Fprintf(&b, `<rect class="ins-bar ins-c-commits" x="%.2f" y="%.2f" width="%.2f" height="%.2f"/>`, float64(i)*slot+0.5, h-bh, slot-1, bh)
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// insCalCell is one day of the contribution calendar.
type insCalCell struct {
	Day   time.Time
	Count int
}

// insCalendar draws a GitHub-style contribution calendar: 53 weeks of days
// (columns are weeks starting Sunday), shaded in five amber levels. It keeps
// its natural size and scrolls sideways inside its own box on small screens.
func insCalendar(id, title string, days []insCalCell, total int) template.HTML {
	if len(days) == 0 {
		return ""
	}
	const cell, step, left, topPad = 11.0, 14.0, 30.0, 18.0
	first := days[0].Day
	weeks := 0
	for _, d := range days {
		w := int(d.Day.Sub(insWeekStart(first)).Hours()/24+0.5) / 7
		if w+1 > weeks {
			weeks = w + 1
		}
	}
	width := left + float64(weeks)*step
	height := topPad + 7*step
	maxC := 0
	busiest := days[0]
	for _, d := range days {
		if d.Count > maxC {
			maxC, busiest = d.Count, d
		}
	}
	level := func(n int) int {
		switch {
		case n == 0:
			return 0
		case maxC <= 4:
			return min(n, 4)
		}
		f := float64(n) / float64(maxC)
		switch {
		case f > 0.75:
			return 4
		case f > 0.5:
			return 3
		case f > 0.25:
			return 2
		}
		return 1
	}
	desc := fmt.Sprintf("%s commits from %s to %s.", insNum(float64(total)), first.Format("2 Jan 2006"), days[len(days)-1].Day.Format("2 Jan 2006"))
	if maxC > 0 {
		desc += fmt.Sprintf(" Busiest day: %s with %d.", busiest.Day.Format("Mon 2 Jan 2006"), maxC)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<figure class="ins-chart ins-cal" id="%s"><div class="ins-cal-scroll" tabindex="0" aria-label="%s, scrollable">`, insEsc(id), insEsc(title))
	fmt.Fprintf(&b, `<svg class="ins-cal-svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f" role="img" aria-labelledby="%s-t %s-d" focusable="false">`,
		width, height, width, height, insEsc(id), insEsc(id))
	fmt.Fprintf(&b, `<title id="%s-t">%s</title><desc id="%s-d">%s</desc>`, insEsc(id), insEsc(title), insEsc(id), insEsc(desc))
	for i, name := range []string{"Mon", "Wed", "Fri"} {
		fmt.Fprintf(&b, `<text class="ins-cal-label" x="0" y="%.1f">%s</text>`, topPad+float64(1+2*i)*step+cell-2, name)
	}
	lastMonth := time.Month(0)
	for _, d := range days {
		w := int(d.Day.Sub(insWeekStart(first)).Hours()/24+0.5) / 7
		x := left + float64(w)*step
		y := topPad + float64(d.Day.Weekday())*step
		if d.Day.Day() <= 7 && d.Day.Weekday() == 0 || (lastMonth == 0 && d.Day.Weekday() == 0) {
			if d.Day.Month() != lastMonth && x+24 < width {
				fmt.Fprintf(&b, `<text class="ins-cal-label" x="%.1f" y="11">%s</text>`, x, d.Day.Format("Jan"))
				lastMonth = d.Day.Month()
			}
		}
		fmt.Fprintf(&b, `<rect class="ins-l%d" x="%.1f" y="%.1f" width="%.0f" height="%.0f" rx="2"><title>%s</title></rect>`,
			level(d.Count), x, y, cell, cell, insEsc(plural(d.Count, "commit")+" on "+d.Day.Format("Mon 2 Jan 2006")))
	}
	b.WriteString(`</svg></div>`)
	b.WriteString(`<div class="ins-cal-legend small muted" aria-hidden="true"><span>Less</span>`)
	for i := 0; i <= 4; i++ {
		fmt.Fprintf(&b, `<svg width="11" height="11" viewBox="0 0 11 11"><rect class="ins-l%d" width="11" height="11" rx="2"/></svg>`, i)
	}
	b.WriteString(`<span>More</span></div>`)
	// Table: commits per month.
	type month struct {
		label string
		n     int
	}
	var months []month
	for _, d := range days {
		l := d.Day.Format("Jan 2006")
		if len(months) == 0 || months[len(months)-1].label != l {
			months = append(months, month{label: l})
		}
		months[len(months)-1].n += d.Count
	}
	fmt.Fprintf(&b, `<details class="ins-data"><summary>Show data table</summary><div class="ins-table-scroll"><table class="ins-table"><caption class="sr-only">%s, by month</caption><thead><tr><th scope="col">Month</th><th scope="col" class="right">Commits</th></tr></thead><tbody>`, insEsc(title))
	for i := len(months) - 1; i >= 0; i-- {
		fmt.Fprintf(&b, `<tr><th scope="row">%s</th><td class="right">%d</td></tr>`, insEsc(months[i].label), months[i].n)
	}
	b.WriteString(`</tbody></table></div></details></figure>`)
	return template.HTML(b.String())
}

// insShare is one part of a whole (a language, a workflow's minutes).
type insShare struct {
	Name   string
	Color  string // a hex colour for languages; "" uses Class
	Class  string
	Value  float64
	Text   string // the value as shown
	Pct    float64
	Files  int
	Swatch template.HTML
}

// insStackBar draws one horizontal bar split into shares (the language bar).
func insStackBar(id, title string, parts []insShare) template.HTML {
	total := 0.0
	for _, p := range parts {
		total += p.Value
	}
	if total <= 0 {
		return ""
	}
	const w, h = 1000.0, 12.0
	var desc []string
	for _, p := range parts {
		desc = append(desc, fmt.Sprintf("%s %.1f%%", p.Name, p.Value/total*100))
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="ins-stack" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" role="img" aria-labelledby="%s-t %s-d" focusable="false"><title id="%s-t">%s</title><desc id="%s-d">%s</desc>`,
		w, h, insEsc(id), insEsc(id), insEsc(id), insEsc(title), insEsc(id), insEsc(strings.Join(desc, ", ")))
	x := 0.0
	for _, p := range parts {
		pw := p.Value / total * w
		if pw <= 0 {
			continue
		}
		draw := pw - 2 // 2-unit gap between segments
		if draw < 1 {
			draw = math.Max(pw, 1)
		}
		fill := ""
		if p.Color != "" {
			fill = ` fill="` + insEsc(p.Color) + `"`
		}
		fmt.Fprintf(&b, `<rect class="%s" x="%.2f" y="0" width="%.2f" height="%.0f"%s><title>%s: %.1f%%</title></rect>`,
			insEsc(p.Class), x, draw, h, fill, insEsc(p.Name), p.Value/total*100)
		x += pw
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// insMeter is a single horizontal bar showing pct (0–100) of a track.
func insMeter(class string, pct float64) template.HTML {
	pct = math.Max(0, math.Min(100, pct))
	draw := pct
	if pct > 0 && pct < 1 {
		draw = 1
	}
	return template.HTML(fmt.Sprintf(`<svg class="ins-meter" viewBox="0 0 100 8" preserveAspectRatio="none" aria-hidden="true" focusable="false"><rect class="ins-meter-track" width="100" height="8" rx="1"/><rect class="%s" width="%.2f" height="8" rx="1"/></svg>`,
		insEsc(class), draw))
}

// insSwatch is a legend chip in a language's colour.
func insSwatch(color string) template.HTML {
	return template.HTML(fmt.Sprintf(`<svg class="ins-swatch-svg" width="10" height="10" viewBox="0 0 10 10" aria-hidden="true"><rect width="10" height="10" rx="2" fill="%s"/></svg>`, insEsc(color)))
}
