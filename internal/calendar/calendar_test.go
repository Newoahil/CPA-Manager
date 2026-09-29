package calendar

import (
	"strings"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

func testCal(t *testing.T) *Calendar {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	return New(loc)
}

func day(t *testing.T, cal *Calendar, y int, m time.Month, d int) domain.HolidayContext {
	t.Helper()
	return cal.Context(time.Date(y, m, d, 12, 0, 0, 0, cal.loc))
}

// TestOrdinaryWorkday covers a plain weekday with no holiday nearby.
func TestOrdinaryWorkday(t *testing.T) {
	cal := testCal(t)
	ctx := day(t, cal, 2026, time.March, 10) // Tuesday
	if !ctx.IsWorkday || ctx.IsHoliday {
		t.Errorf("workday flags = work:%v holiday:%v, want true/false", ctx.IsWorkday, ctx.IsHoliday)
	}
	if ctx.DaysToNextWork != 0 {
		t.Errorf("DaysToNextWork = %d, want 0", ctx.DaysToNextWork)
	}
	if ctx.HolidayRunLength != 0 {
		t.Errorf("HolidayRunLength = %d, want 0", ctx.HolidayRunLength)
	}
	if ctx.Label != "工作日" {
		t.Errorf("Label = %q, want 工作日", ctx.Label)
	}
	if ctx.Date != "2026-03-10" {
		t.Errorf("Date = %q", ctx.Date)
	}
}

// TestWeekend covers a normal, non-makeup weekend.
func TestWeekend(t *testing.T) {
	cal := testCal(t)
	ctx := day(t, cal, 2026, time.March, 7) // Saturday
	if ctx.IsWorkday || !ctx.IsHoliday {
		t.Errorf("weekend flags = work:%v holiday:%v, want false/true", ctx.IsWorkday, ctx.IsHoliday)
	}
	if ctx.HolidayRunLength != 2 {
		t.Errorf("HolidayRunLength = %d, want 2 (Sat+Sun)", ctx.HolidayRunLength)
	}
	if ctx.DaysToNextWork != 2 {
		t.Errorf("DaysToNextWork = %d, want 2", ctx.DaysToNextWork)
	}
	if ctx.Label != "周末" {
		t.Errorf("Label = %q, want 周末", ctx.Label)
	}
}

// TestStatutoryHoliday covers a day inside 春节, including run length across the
// full break.
func TestStatutoryHoliday(t *testing.T) {
	cal := testCal(t)
	ctx := day(t, cal, 2026, time.February, 16) // 春节内 Monday
	if ctx.IsWorkday || !ctx.IsHoliday {
		t.Errorf("spring festival flags = work:%v holiday:%v, want false/true", ctx.IsWorkday, ctx.IsHoliday)
	}
	if ctx.Label != "春节" {
		t.Errorf("Label = %q, want 春节", ctx.Label)
	}
	// 2026-02-15..02-23 are off (02-14 Sat and 02-28 Sat are makeup workdays).
	if ctx.HolidayRunLength != 8 {
		t.Errorf("HolidayRunLength from 02-16 = %d, want 8", ctx.HolidayRunLength)
	}
	if ctx.DaysToNextWork != 8 {
		t.Errorf("DaysToNextWork = %d, want 8", ctx.DaysToNextWork)
	}
}

// TestNationalDayRun covers the 国庆节 break and that the following Saturday
// makeup day is correctly treated as work.
func TestNationalDayRun(t *testing.T) {
	cal := testCal(t)
	ctx := day(t, cal, 2026, time.October, 1)
	if ctx.Label != "国庆节" || ctx.IsWorkday {
		t.Errorf("national day = %q work=%v, want 国庆节 work=false", ctx.Label, ctx.IsWorkday)
	}
	if ctx.HolidayRunLength != 7 {
		t.Errorf("HolidayRunLength = %d, want 7", ctx.HolidayRunLength)
	}
	// 2026-10-10 is a Saturday converted to a workday.
	mk := day(t, cal, 2026, time.October, 10)
	if !mk.IsWorkday {
		t.Error("2026-10-10 must be a makeup workday")
	}
	if !strings.Contains(mk.Label, "调休上班") {
		t.Errorf("Label = %q, want makeup-workday annotation", mk.Label)
	}
}

// TestMakeupWorkdays covers every 调休上班日 in the table so a typo cannot hide.
func TestMakeupWorkdays(t *testing.T) {
	cal := testCal(t)
	cases := []struct {
		month time.Month
		day   int
	}{
		{time.January, 4},
		{time.February, 14},
		{time.February, 28},
		{time.May, 9},
		{time.September, 20},
		{time.October, 10},
	}
	for _, tc := range cases {
		ctx := day(t, cal, 2026, tc.month, tc.day)
		if !ctx.IsWorkday || ctx.IsHoliday {
			t.Errorf("2026-%02d-%02d workday=%v holiday=%v, want makeup workday", tc.month, tc.day, ctx.IsWorkday, ctx.IsHoliday)
		}
		if ctx.HolidayRunLength != 0 {
			t.Errorf("2026-%02d-%02d HolidayRunLength = %d, want 0", tc.month, tc.day, ctx.HolidayRunLength)
		}
	}
}

// TestOutOfRangeDegradesToWeekendOnly covers the honest degradation: no holiday
// data, only the weekend rule, and a Label that admits the data is stale.
func TestOutOfRangeDegradesToWeekendOnly(t *testing.T) {
	cal := testCal(t)

	// 2027-01-01 is a Friday and, absent data, a working day.
	weekday := day(t, cal, 2027, time.January, 1)
	if !weekday.IsWorkday {
		t.Error("2027-01-01 should fall back to a weekday")
	}
	if !strings.Contains(weekday.Label, "已过期") {
		t.Errorf("Label = %q, want stale-data warning", weekday.Label)
	}

	// 2025-10-01 is a Wednesday in the past, also out of range.
	past := day(t, cal, 2025, time.October, 1)
	if !past.IsWorkday || !strings.Contains(past.Label, "已过期") {
		t.Errorf("2025-10-01 = work:%v label:%q, want weekend-rule workday with stale warning", past.IsWorkday, past.Label)
	}

	// A weekend beyond the range still reports as a weekend.
	sat := day(t, cal, 2027, time.January, 2)
	if sat.IsWorkday || !strings.Contains(sat.Label, "已过期") {
		t.Errorf("2027-01-02 = work:%v label:%q, want weekend with stale warning", sat.IsWorkday, sat.Label)
	}
}

// TestLongHolidayRunLength is the input the recommendation logic keys on: it
// must report the 春节 run as >= 2 so "use more" cannot trigger for a one-day
// weekend.
func TestLongHolidayRunLength(t *testing.T) {
	cal := testCal(t)
	for _, tc := range []struct {
		y     int
		m     time.Month
		d     int
		want  int
		label string
	}{
		{2026, 1, 1, 3, "元旦"},
		{2026, 4, 4, 3, "清明节"},
		{2026, 5, 1, 5, "劳动节"},
		{2026, 6, 19, 3, "端午节"},
		{2026, 9, 25, 3, "中秋节"},
	} {
		ctx := day(t, cal, tc.y, tc.m, tc.d)
		if ctx.Label != tc.label {
			t.Errorf("%s label = %q, want %q", ctx.Date, ctx.Label, tc.label)
		}
		if ctx.HolidayRunLength != tc.want {
			t.Errorf("%s HolidayRunLength = %d, want %d", ctx.Date, ctx.HolidayRunLength, tc.want)
		}
	}
}

// TestCalendarLocationRespected ensures now is interpreted in the configured
// zone, not UTC: 2026-01-01 00:30 +08:00 must not roll back to 2025-12-31.
func TestCalendarLocationRespected(t *testing.T) {
	cal := testCal(t)
	utc := time.Date(2025, 12, 31, 16, 30, 0, 0, time.UTC) // 2026-01-01 00:30 +08
	ctx := cal.Context(utc)
	if ctx.Date != "2026-01-01" {
		t.Errorf("Date = %q, want 2026-01-01", ctx.Date)
	}
}
