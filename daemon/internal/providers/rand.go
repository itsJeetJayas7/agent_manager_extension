package providers

import (
	"crypto/rand"
	"io"
)

func readRandom(b []byte) (int, error) {
	return io.ReadFull(rand.Reader, b)
}
