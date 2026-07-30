package store

import (
	"fmt"
	"math"
	"strconv"
)

func formatInt64(v int64) string { return strconv.FormatInt(v, 10) }

func parseInt64(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }

// int32FromDB narrows a value read from an INTEGER column, refusing rather than
// truncating when it does not fit.
//
// Heights are int32 throughout the daemon and real ones are nowhere near the
// limit, so a value that overflows means the database is corrupt or was written
// by something else. Silently truncating it would be worst for the fork height,
// which bounds how far the user's own chain is treated as verified — a wrong
// value there is exactly the failure that bound exists to prevent.
func int32FromDB(column string, v int64) (int32, error) {
	if v < math.MinInt32 || v > math.MaxInt32 {
		return 0, fmt.Errorf("store: %s holds %d, which is not a plausible block height", column, v)
	}
	return int32(v), nil
}
