package util

import (
	"strconv"
)

func toInt(val interface{}) (int, bool) {
	switch v := val.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case string:
		if v == "" {
			return 0, false
		}
		out, err := strconv.Atoi(v)
		return out, err == nil
	default:
		return 0, false
	}
}
