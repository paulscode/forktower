package cln

import (
	"fmt"
	"strconv"
	"strings"
)

// msatFrom reads one of Core Lightning's amounts.
//
// They arrive in two shapes depending on the version and the field: a JSON
// number of millisatoshis, or a string like "150000000msat". Both are read here
// rather than in each caller, and anything else is an error — a silently-zero
// capacity would be worse than a refusal, because it would make a channel look
// like it holds nothing.
func msatFrom(v any, what string) (int64, error) {
	switch value := v.(type) {
	case nil:
		return 0, nil
	case float64:
		// JSON's own number type. Amounts here are millisatoshis and can exceed
		// what a float64 represents exactly, so anything at that scale must have
		// arrived as a string; refuse rather than round.
		if value != float64(int64(value)) || value > 1<<53 {
			return 0, fmt.Errorf("the %s %v cannot be read exactly as a number", what, value)
		}
		return int64(value), nil
	case string:
		trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(value), "msat"))
		if trimmed == "" {
			return 0, nil
		}
		n, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("reading the %s %q: %w", what, value, err)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("the %s arrived as %T, which this cannot read", what, v)
	}
}
