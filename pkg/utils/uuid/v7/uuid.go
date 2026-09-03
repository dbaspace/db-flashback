package v7

import (
	"fmt"

	"github.com/google/uuid"
)

func NewUUIDv7() string {
	total := 5
	for {
		id, err := uuid.NewV7()
		if err == nil {
			return id.String()
		}
		if total == 0 {
			panic(fmt.Sprintf("uuidv7 error: %v", err))
		}
		total--
	}
}
