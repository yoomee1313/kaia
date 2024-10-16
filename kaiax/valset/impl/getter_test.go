package impl

import (
	"strconv"
	"strings"
	"testing"

	"github.com/kaiachain/kaia/common"
	"github.com/stretchr/testify/assert"
)

func TestConvertHashToSeed(t *testing.T) {
	// Prepare test hash value
	hash := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")

	convertHashToSeedOld := func(hash common.Hash) (int64, error) {
		// ConvertHashToSeed returns a random seed used to calculate proposer.
		// It converts first 7.5 bytes of the given hash to int64.
		// TODO-Kaia-Istanbul: convert hash.Hex() to int64 directly without string conversion
		hashstring := strings.TrimPrefix(hash.Hex(), "0x")
		if len(hashstring) > 15 {
			hashstring = hashstring[:15]
		}

		seed, err := strconv.ParseInt(hashstring, 16, 64)
		if err != nil {
			logger.Error("fail to make sub-list of validators", "hash", hash.Hex(), "seed", seed, "err", err)
			return 0, err
		}
		return seed, nil
	}

	// legacy function call
	oldSeed, errOld := convertHashToSeedOld(hash)
	assert.Nil(t, errOld)

	// new function call
	newSeed := ConvertHashToSeed(hash)

	// verify the seed is identical
	assert.Equal(t, oldSeed, newSeed, "old and new seed are not equal")
}
