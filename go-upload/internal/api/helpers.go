package api

import (
	"strconv"
)

func parseInt(val string) (int, error) {
	if val == "" {
		return 0, strconv.ErrSyntax
	}
	return strconv.Atoi(val)
}

func parseIntDefault(val string, fallback int) int {
	if val == "" {
		return fallback
	}
	out, err := strconv.Atoi(val)
	if err != nil {
		return fallback
	}
	return out
}

func parseFloat(val string) (float64, error) {
	if val == "" {
		return 0, strconv.ErrSyntax
	}
	return strconv.ParseFloat(val, 64)
}
