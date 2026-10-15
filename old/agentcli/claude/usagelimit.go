// Adapted from github.com/bwsalmon/grain pkg/agent/claude/usagelimit.go at c950ca0858bc4b381e5ce7e2bd716997f63bd24d, Apache License 2.0.

package claude

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

// resetEpochPattern is the strict form: the CLI's own
// "...limit reached|<unix seconds>". The digit bound admits both a
// seconds epoch (10 digits until 2286) and a milliseconds one, which
// resetFromEpoch tells apart; it excludes a short number that happened
// to follow a pipe.
var resetEpochPattern = regexp.MustCompile(`(?i)usage limit reached\|(\d{9,13})`)

// looseUsageLimitMarkers are the phrases that mean "out of budget" in
// text that already belongs to a failed run. Lowercase, matched against
// a lowercased haystack.
//
// "429" is deliberately not one of them on its own: it is three digits
// that appear in file paths, diffs and test output, and the run whose
// text this reads is one whose sandbox has been echoing all three back
// for an hour. Every marker here is a phrase a provider writes and a
// repository does not.
var looseUsageLimitMarkers = []string{
	"usage limit reached",
	"usage limit exceeded",
	"rate_limit_error",
	"rate limit exceeded",
	"exceeded your rate limit",
}

func mentionsUsageLimit(text string) bool {
	lower := strings.ToLower(text)
	for _, marker := range looseUsageLimitMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return hitLimitPattern.MatchString(text)
}

// usageLimitNow is the clock resetFromClock measures "the next 5:30pm"
// from; a variable so a test can stand at a chosen moment.
var usageLimitNow = time.Now

// hitLimitPattern is the CLI's worded refusal: "hit your session
// limit", "hit your usage limit", "hit your weekly limit" -- and, so the
// next window it names does not need a release of grain to be
// recognised, any one or two words in that slot ("hit your Opus weekly
// limit"). Loose: consulted on its own only for a run that has already
// failed.
var hitLimitPattern = regexp.MustCompile(`(?i)\bhit your (?:[\w-]+ ){1,2}limit\b`)

// hitLimitAnswerPattern is the same refusal as the CLI words a whole
// answer: it opens the text. Apostrophe straight or curly.
var hitLimitAnswerPattern = regexp.MustCompile(`(?i)^(?:you['’]ve|you have) hit your (?:[\w-]+ ){1,2}limit\b`)

// maxHitLimitAnswer bounds how long a successful run's final answer can
// be and still be read as the CLI's refusal. The CLI's own is about
// fifty characters; an agent's report that happens to open with the
// same words is a paragraph.
const maxHitLimitAnswer = 200

// isHitLimitAnswer is the strict reading of the worded form, for a
// successful run's final answer: the whole text is short and is the
// refusal, from its first word.
func isHitLimitAnswer(resultText string) bool {
	text := strings.TrimSpace(resultText)
	return len(text) <= maxHitLimitAnswer && hitLimitAnswerPattern.MatchString(text)
}

// resetClockPattern is the "resets ..." clause the worded form carries:
// an optional date ("Oct 5,", "October 5 at"), a clock time in either
// convention ("5:30pm", "5pm", "17:30"), and an optional zone in
// parentheses ("(UTC)", "(America/New_York)", "(PDT)").
var resetClockPattern = regexp.MustCompile(`(?i)\bresets\s+(?:([a-z]{3,9})\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(?:at\s+)?)?(\d{1,2})(?::(\d{2}))?\s*([ap]\.?m\.?)?(?:\s*\(([^)]+)\))?`)

// zoneAbbreviations are the zone names the CLI might print that the
// tz database does not know by that name. Fixed offsets: an
// abbreviation already says which half of the year it means.
var zoneAbbreviations = map[string]int{
	"UTC": 0, "GMT": 0, "Z": 0,
	"BST": 1, "CET": 1, "CEST": 2, "EET": 2, "EEST": 3,
	"IST": 5*60 + 30, "JST": 9 * 60, "AEST": 10 * 60, "AEDT": 11 * 60,
	"EST": -5 * 60, "EDT": -4 * 60, "CST": -6 * 60, "CDT": -5 * 60,
	"MST": -7 * 60, "MDT": -6 * 60, "PST": -8 * 60, "PDT": -7 * 60,
}

// resetFromClock reads the worded form's "resets <time>" into the
// instant the window reopens: the next occurrence of that wall-clock
// time after now -- tomorrow's, when today's has already passed -- or,
// when a date is named, that date's, in the coming year if this year's
// is well past. A time with no zone is read as UTC, which is what the
// sandboxes the CLI runs in keep. Anything that does not read cleanly
// yields the zero time, which agent.UsageLimitError reads as "no reset
// named": the limit is still a limit, and the pause falls back to its
// own default window.
func resetFromClock(text string, now time.Time) time.Time {
	m := resetClockPattern.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}
	}
	monthName, dayDigits, hourDigits, minuteDigits, meridiem, zone := m[1], m[2], m[3], m[4], m[5], m[6]
	// A bare "resets 5" says neither which half of the day nor that it
	// is a time at all.
	if minuteDigits == "" && meridiem == "" {
		return time.Time{}
	}
	hour, _ := strconv.Atoi(hourDigits)
	minute := 0
	if minuteDigits != "" {
		minute, _ = strconv.Atoi(minuteDigits)
	}
	if minute > 59 {
		return time.Time{}
	}
	if meridiem != "" {
		if hour < 1 || hour > 12 {
			return time.Time{}
		}
		hour %= 12
		if strings.HasPrefix(strings.ToLower(meridiem), "p") {
			hour += 12
		}
	} else if hour > 23 {
		return time.Time{}
	}
	loc, ok := resetLocation(zone)
	if !ok {
		return time.Time{}
	}
	local := now.In(loc)
	if monthName != "" {
		month, ok := parseMonth(monthName)
		if !ok {
			return time.Time{}
		}
		day, _ := strconv.Atoi(dayDigits)
		at := time.Date(local.Year(), month, day, hour, minute, 0, 0, loc)
		if at.Month() != month || at.Day() != day {
			return time.Time{} // "Feb 30" normalised into March
		}
		// A date more than a day behind now is next year's: a weekly
		// limit named on Dec 30 that resets "Jan 2".
		if at.Before(now.Add(-24 * time.Hour)) {
			at = at.AddDate(1, 0, 0)
		}
		return at.UTC()
	}
	at := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
	if !at.After(now) {
		at = time.Date(local.Year(), local.Month(), local.Day()+1, hour, minute, 0, 0, loc)
	}
	return at.UTC()
}

// resetLocation reads the zone the CLI put in parentheses: an IANA name
// or one of zoneAbbreviations. No zone at all is UTC; one that is
// neither is not guessed at.
func resetLocation(zone string) (*time.Location, bool) {
	zone = strings.TrimSpace(zone)
	if zone == "" {
		return time.UTC, true
	}
	if offset, ok := zoneAbbreviations[strings.ToUpper(zone)]; ok {
		if offset == 0 {
			return time.UTC, true
		}
		return time.FixedZone(strings.ToUpper(zone), offset*60), true
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, false
	}
	return loc, true
}

// parseMonth reads an English month name, full or abbreviated to its
// first three letters ("Sept" too).
func parseMonth(name string) (time.Month, bool) {
	lower := strings.ToLower(name)
	if len(lower) < 3 {
		return 0, false
	}
	for m := time.January; m <= time.December; m++ {
		full := strings.ToLower(m.String())
		if lower == full || lower == full[:3] || (m == time.September && lower == "sept") {
			return m, true
		}
	}
	return 0, false
}

// resetFromEpoch turns the digits captured after the pipe into an
// instant, reading a 12-or-more-digit number as milliseconds and
// anything shorter as seconds -- the two spellings a Unix epoch arrives
// in, told apart by magnitude rather than by trusting one. A number that
// does not parse at all (longer than an int64) yields the zero time,
// which agent.UsageLimitError already reads as "the provider named no
// reset".
func resetFromEpoch(digits string) time.Time {
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return time.Time{}
	}
	if len(digits) >= 12 {
		return time.UnixMilli(n).UTC()
	}
	return time.Unix(n, 0).UTC()
}
