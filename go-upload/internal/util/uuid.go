package util

import "github.com/google/uuid"

func NewUUID() string {
	return uuid.NewString()
}
