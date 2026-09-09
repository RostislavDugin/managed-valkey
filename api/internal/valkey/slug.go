package valkey

import (
	"crypto/rand"
	"fmt"
	"math/big"
)

const slugAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

type SlugGenerator interface {
	Suffix() (string, error)
}

type CryptoSlugGenerator struct{}

func (CryptoSlugGenerator) Suffix() (string, error) {
	result := make([]byte, 6)
	for index := range result {
		position, err := rand.Int(rand.Reader, big.NewInt(int64(len(slugAlphabet))))
		if err != nil {
			return "", fmt.Errorf("сгенерировать slug: %w", err)
		}

		result[index] = slugAlphabet[position.Int64()]
	}

	return string(result), nil
}
