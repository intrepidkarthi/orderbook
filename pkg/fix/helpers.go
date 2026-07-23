package fix

import "strconv"

func stringValue(v int64) string {
	return strconv.FormatInt(v, 10)
}
