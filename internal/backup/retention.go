package backup

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Retention keeps the newest snapshot of each of the last Daily days, Weekly
// ISO weeks and Monthly months that have snapshots. The newest snapshot is
// always kept. All zero means keep everything.
type Retention struct {
	Daily, Weekly, Monthly int
}

func (r Retention) String() string {
	return fmt.Sprintf("%d daily, %d weekly, %d monthly", r.Daily, r.Weekly, r.Monthly)
}

// Expired returns the snapshots that fall outside the retention policy.
func (r Retention) Expired(snaps []Snapshot) []Snapshot {
	if r.Daily <= 0 && r.Weekly <= 0 && r.Monthly <= 0 {
		return nil
	}
	s := append([]Snapshot(nil), snaps...)
	sortSnapshots(s)
	keep := make([]bool, len(s))
	if len(s) > 0 {
		keep[0] = true
	}
	bucket := func(n int, key func(time.Time) string) {
		seen := map[string]bool{}
		for i, sn := range s {
			if len(seen) >= n {
				return
			}
			k := key(sn.Time)
			if !seen[k] {
				seen[k] = true
				keep[i] = true
			}
		}
	}
	bucket(r.Daily, func(t time.Time) string { return t.Format("2006-01-02") })
	bucket(r.Weekly, func(t time.Time) string { y, w := t.ISOWeek(); return fmt.Sprintf("%d-W%02d", y, w) })
	bucket(r.Monthly, func(t time.Time) string { return t.Format("2006-01") })
	var out []Snapshot
	for i, sn := range s {
		if !keep[i] {
			out = append(out, sn)
		}
	}
	return out
}

// ParseClock parses "HH:MM". "off" (or empty) disables the schedule.
func ParseClock(s string) (hour, min int, ok bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "off" || s == "never" {
		return 0, 0, false
	}
	h, m, found := strings.Cut(s, ":")
	if !found {
		return 0, 0, false
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, 0, false
	}
	return hh, mm, true
}

// NextRun is the next time after now that the daily clock time comes round.
func NextRun(now time.Time, clock string) (time.Time, bool) {
	h, m, ok := ParseClock(clock)
	if !ok {
		return time.Time{}, false
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if !next.After(now) {
		next = time.Date(now.Year(), now.Month(), now.Day()+1, h, m, 0, 0, now.Location())
	}
	return next, true
}
