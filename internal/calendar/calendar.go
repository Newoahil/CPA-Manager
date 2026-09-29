// Package calendar answers "is today a working day in China?" for the
// capacity-advice logic.
//
// Scope is intentionally small: the watcher only needs to know whether a quota
// window will reset before the team returns to work. There is no attempt to be
// a general holiday library, and the built-in data has a hard year boundary so
// that an out-of-date table degrades loudly instead of silently.
package calendar

import (
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// dataYears is the range of years for which the tables below are authoritative.
//
// Outside this range the calendar falls back to a plain weekend rule and says
// so in Label. Do not extend the range by guessing: update the tables from the
// State Council notice and bump this value.
const (
	dataFirstYear = 2026
	dataLastYear  = 2026
	dataSource    = "国务院办公厅《关于2026年部分节假日安排的通知》"
)

// holidayPeriod is one statutory holiday range, inclusive of both ends.
type holidayPeriod struct {
	Name  string
	Month int
	From  int
	To    int
}

// makeupDay is a weekend day that the notice converts into a working day.
type makeupDay struct {
	Name  string
	Month int
	Day   int
}

// holidays2026 is the authoritative 2026 table.
//
// Source: 2026 年部分节假日安排 (State Council). Verified against the public
// holiday calendar feed, including the makeup workdays that fall on weekends.
var holidays2026 = []holidayPeriod{
	{Name: "元旦", Month: 1, From: 1, To: 3},
	{Name: "春节", Month: 2, From: 15, To: 23},
	{Name: "清明节", Month: 4, From: 4, To: 6},
	{Name: "劳动节", Month: 5, From: 1, To: 5},
	{Name: "端午节", Month: 6, From: 19, To: 21},
	{Name: "中秋节", Month: 9, From: 25, To: 27},
	{Name: "国庆节", Month: 10, From: 1, To: 7},
}

// makeup2026 lists the 调休上班日: weekend days worked to make up the holidays.
//
// These override the weekend rule; missing one would wrongly report a run of
// non-working days longer than reality.
var makeup2026 = []makeupDay{
	{Name: "元旦后补班", Month: 1, Day: 4},    // Sunday
	{Name: "春节前补班", Month: 2, Day: 14},   // Saturday
	{Name: "春节后补班", Month: 2, Day: 28},   // Saturday
	{Name: "劳动节后补班", Month: 5, Day: 9},   // Saturday
	{Name: "中秋节前补班", Month: 9, Day: 20},  // Sunday
	{Name: "国庆节后补班", Month: 10, Day: 10}, // Saturday
}

// Calendar resolves holiday context in a configured location.
type Calendar struct {
	loc *time.Location
}

// New returns a Calendar whose dates are interpreted in loc. A nil location
// falls back to time.Local so callers cannot accidentally crash on a missing
// timezone.
func New(loc *time.Location) *Calendar {
	if loc == nil {
		loc = time.Local
	}
	return &Calendar{loc: loc}
}

// Context describes now relative to the Chinese working calendar.
func (c *Calendar) Context(now time.Time) domain.HolidayContext {
	local := now.In(c.loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, c.loc)

	inRange := day.Year() >= dataFirstYear && day.Year() <= dataLastYear

	ctx := domain.HolidayContext{Date: day.Format("2006-01-02")}

	if !inRange {
		// Degrade honestly: only the weekend rule applies and Label says the
		// table is out of date, so nobody trusts a stale holiday name.
		work := c.weekendWorkday(day)
		ctx.IsWorkday = work
		ctx.IsHoliday = !work
		if work {
			ctx.Label = "工作日（节假日数据已过期，仅按周末规则）"
		} else {
			ctx.Label = "周末（节假日数据已过期，仅按周末规则）"
		}
		ctx.DaysToNextWork = c.daysToNextWorkday(day)
		ctx.HolidayRunLength = c.runLength(day)
		return ctx
	}

	work, label := c.lookup(day)
	ctx.IsWorkday = work
	ctx.IsHoliday = !work
	ctx.Label = label
	ctx.DaysToNextWork = c.daysToNextWorkday(day)
	ctx.HolidayRunLength = c.runLength(day)
	return ctx
}

// lookup classifies a day for a year covered by the tables.
func (c *Calendar) lookup(day time.Time) (work bool, label string) {
	m, d := int(day.Month()), day.Day()

	for _, mk := range makeup2026 {
		if mk.Month == m && mk.Day == d {
			return true, mk.Name + "（调休上班）"
		}
	}
	for _, h := range holidays2026 {
		if h.Month == m && d >= h.From && d <= h.To {
			return false, h.Name
		}
	}
	if isWeekend(day) {
		return false, "周末"
	}
	return true, "工作日"
}

// weekendWorkday is the fallback rule outside the data range.
func (c *Calendar) weekendWorkday(day time.Time) bool {
	return !isWeekend(day)
}

func isWeekend(day time.Time) bool {
	switch day.Weekday() {
	case time.Saturday, time.Sunday:
		return true
	default:
		return false
	}
}

// isWorkdayAt classifies an arbitrary day using the same rules as Context. It is
// used to scan forward for the next working day and for the run length.
func (c *Calendar) isWorkdayAt(day time.Time) bool {
	if day.Year() < dataFirstYear || day.Year() > dataLastYear {
		return c.weekendWorkday(day)
	}
	work, _ := c.lookup(day)
	return work
}

// daysToNextWorkday counts calendar days from day until the next working day,
// where today being a working day yields 0.
func (c *Calendar) daysToNextWorkday(day time.Time) int {
	for i := 0; i < 400; i++ {
		if c.isWorkdayAt(day.AddDate(0, 0, i)) {
			return i
		}
	}
	return 0
}

// runLength counts consecutive non-working days starting at day. A working day
// returns 0. It stops as soon as a working day (including a 调休上班日) appears,
// which is why the makeup table matters for long-holiday advice.
func (c *Calendar) runLength(day time.Time) int {
	if c.isWorkdayAt(day) {
		return 0
	}
	n := 0
	for i := 0; i < 400; i++ {
		if c.isWorkdayAt(day.AddDate(0, 0, i)) {
			break
		}
		n++
	}
	return n
}

// DataSource reports the provenance of the built-in table for diagnostics.
func DataSource() string { return dataSource }
