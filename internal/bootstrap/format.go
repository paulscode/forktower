package bootstrap

import (
	"fmt"
	"strconv"
	"time"
)

// HumanBytes writes a size the way a person would say it.
//
// Powers of 1024 with the short names, because that is what every disk-space
// dialogue on the machines this runs on uses, and a dashboard that disagreed with
// the platform's own free-space figure by seven percent would send somebody
// looking for the discrepancy.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d bytes", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 3; m /= unit {
		div *= unit
		exp++
	}
	value := float64(n) / float64(div)
	// One decimal below ten, none above: "8.7 GB" carries information, and
	// "512.0 MB" carries a false sense of measurement.
	format := "%.0f %s"
	if value < 10 {
		format = "%.1f %s"
	}
	return fmt.Sprintf(format, value, [...]string{"KB", "MB", "GB", "TB"}[exp])
}

// Commas groups a number for reading. Block heights are quoted back to users and
// compared by eye against a block explorer, and 935000 and 93500 are one glance
// apart without them.
func Commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	sign := ""
	if s[0] == '-' {
		sign, s = "-", s[1:]
	}
	head := len(s) % 3
	if head == 0 {
		head = 3
	}
	out := s[:head]
	for i := head; i < len(s); i += 3 {
		out += "," + s[i:i+3]
	}
	return sign + out
}

// HumanDuration writes a remaining time somebody can plan around.
//
// Deliberately coarse. This is fed by a throughput estimate over a link whose
// speed varies by an order of magnitude between one minute and the next, and
// "about 4 hours" is honest where "3h 52m 14s" is a claim to precision the
// underlying number does not have.
func HumanDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return ""
	case d < time.Minute:
		return "less than a minute"
	case d < 2*time.Minute:
		return "about a minute"
	case d < time.Hour:
		return fmt.Sprintf("about %d minutes", int(d.Round(time.Minute)/time.Minute))
	case d < 2*time.Hour:
		return "about an hour"
	case d < 24*time.Hour:
		return fmt.Sprintf("about %d hours", int(d.Round(time.Hour)/time.Hour))
	default:
		days := int(d.Round(24*time.Hour) / (24 * time.Hour))
		if days == 1 {
			return "about a day"
		}
		return fmt.Sprintf("about %d days", days)
	}
}

// ETA estimates how long the rest of a transfer will take.
//
// Returns zero when it has nothing to go on. A progress bar that shows a
// confident time remaining after two seconds is wrong, is known to be wrong when
// it is written, and costs the reader their trust in every later estimate — so
// this declines to guess until enough has moved to mean something.
func ETA(done, total int64, elapsed time.Duration) time.Duration {
	const enoughToJudge = 16 << 20

	if done < enoughToJudge || done >= total || elapsed <= 0 {
		return 0
	}
	rate := float64(done) / elapsed.Seconds()
	if rate <= 0 {
		return 0
	}
	return time.Duration(float64(total-done)/rate) * time.Second
}
