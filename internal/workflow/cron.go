package workflow

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed five-field cron expression (minute hour day-of-month month
// day-of-week), evaluated in the server's local time at minute resolution.
type Cron struct {
	minute, hour, dom, month, dow uint64 // bit sets
	domStar, dowStar              bool
}

var monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
var dayNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

// ParseCron parses "*/15 * * * 1-5" style expressions.
func ParseCron(spec string) (*Cron, error) {
	f := strings.Fields(spec)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron %q must have 5 fields (minute hour day month weekday)", spec)
	}
	c := &Cron{}
	var err error
	if c.minute, err = cronField(f[0], 0, 59, nil); err != nil {
		return nil, fmt.Errorf("cron %q minute: %w", spec, err)
	}
	if c.hour, err = cronField(f[1], 0, 23, nil); err != nil {
		return nil, fmt.Errorf("cron %q hour: %w", spec, err)
	}
	if c.dom, err = cronField(f[2], 1, 31, nil); err != nil {
		return nil, fmt.Errorf("cron %q day of month: %w", spec, err)
	}
	if c.month, err = cronField(f[3], 1, 12, monthNames); err != nil {
		return nil, fmt.Errorf("cron %q month: %w", spec, err)
	}
	if c.dow, err = cronField(f[4], 0, 7, dayNames); err != nil {
		return nil, fmt.Errorf("cron %q weekday: %w", spec, err)
	}
	if c.dow&(1<<7) != 0 { // 7 is Sunday too
		c.dow |= 1
	}
	c.domStar = f[2] == "*" || f[2] == "?"
	c.dowStar = f[4] == "*" || f[4] == "?"
	return c, nil
}

func cronField(s string, lo, hi int, names map[string]int) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(s, ",") {
		step := 1
		rng := part
		if i := strings.IndexByte(part, '/'); i >= 0 {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n < 1 {
				return 0, fmt.Errorf("bad step in %q", part)
			}
			step, rng = n, part[:i]
		}
		start, end := lo, hi
		switch {
		case rng == "*" || rng == "?":
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err error
			if start, err = cronValue(a, names); err != nil {
				return 0, err
			}
			if end, err = cronValue(b, names); err != nil {
				return 0, err
			}
		default:
			v, err := cronValue(rng, names)
			if err != nil {
				return 0, err
			}
			start = v
			if strings.Contains(part, "/") {
				end = hi
			} else {
				end = v
			}
		}
		if start < lo || end > hi || start > end {
			return 0, fmt.Errorf("%q is out of range %d-%d", part, lo, hi)
		}
		for v := start; v <= end; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, nil
}

func cronValue(s string, names map[string]int) (int, error) {
	if v, ok := names[strings.ToLower(s)]; ok {
		return v, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("bad value %q", s)
	}
	return n, nil
}

// Match reports whether the schedule fires in t's minute.
func (c *Cron) Match(t time.Time) bool {
	if c.minute&(1<<uint(t.Minute())) == 0 || c.hour&(1<<uint(t.Hour())) == 0 || c.month&(1<<uint(t.Month())) == 0 {
		return false
	}
	domOK := c.dom&(1<<uint(t.Day())) != 0
	dowOK := c.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dowOK
	case c.dowStar:
		return domOK
	}
	return domOK || dowOK // both restricted: either matches (classic cron)
}
