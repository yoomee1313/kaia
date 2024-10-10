// Modifications Copyright 2024 The Kaia Authors
// Modifications Copyright 2018 The klaytn Authors
// Copyright 2017 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.
//
// This file is derived from quorum/consensus/istanbul/validator/validator.go (2018/06/04).
// Modified and improved for the klaytn development.
// Modified and improved for the Kaia development.

package validator

import (
	"encoding/binary"
	"math/rand"
	"strconv"
	"strings"

	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus"
	"github.com/kaiachain/kaia/consensus/istanbul"
	"github.com/kaiachain/kaia/log"
	"github.com/kaiachain/kaia/params"
)

var logger = log.NewModuleLogger(log.ConsensusIstanbulValidator)

func New(addr common.Address) istanbul.Validator {
	return &defaultValidator{
		address: addr,
	}
}

func NewValidatorSet(addrs, demotedAddrs []common.Address, proposerPolicy params.ProposerPolicy, subGroupSize uint64, chain consensus.ChainReader) istanbul.ValidatorSet {
	var valSet istanbul.ValidatorSet
	if proposerPolicy.IsWeightedCouncil() {
		valSet = NewWeightedCouncil(addrs, demotedAddrs, nil, nil, nil, proposerPolicy, subGroupSize, 0, 0, chain)
	} else {
		valSet = NewSubSet(addrs, proposerPolicy, subGroupSize)
	}

	return valSet
}

func NewSet(addrs []common.Address, policy params.ProposerPolicy) istanbul.ValidatorSet {
	return newDefaultSubSet(addrs, policy, defaultSubSetLength)
}

func NewSubSet(addrs []common.Address, policy params.ProposerPolicy, subSize uint64) istanbul.ValidatorSet {
	return newDefaultSubSet(addrs, policy, subSize)
}

func ExtractValidators(extraData []byte) []common.Address {
	// get the validator addresses
	addrs := make([]common.Address, (len(extraData) / common.AddressLength))
	for i := 0; i < len(addrs); i++ {
		copy(addrs[i][:], extraData[i*common.AddressLength:])
	}

	return addrs
}

// ConvertHashToSeed returns a random seed used to calculate proposer.
// It converts first 7.5 bytes of the given hash to int64.
func ConvertHashToSeed(hash common.Hash) (int64, error) {
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

// SelectRandomCommittee composes a committee selecting validators randomly based on the seed value.
// It returns nil if the given committeeSize is bigger than validatorSize or proposer indexes are invalid.
func SelectRandomCommittee(validators []istanbul.Validator, committeeSize uint64, seed int64, proposerIdx int, nextProposerIdx int) []istanbul.Validator {
	// ensure validator indexes are valid
	if proposerIdx < 0 || nextProposerIdx < 0 || proposerIdx == nextProposerIdx {
		logger.Error("invalid indexes of validators", "proposerIdx", proposerIdx, "nextProposerIdx", nextProposerIdx)
		return nil
	}

	// ensure committeeSize and proposer indexes are valid
	validatorSize := len(validators)
	if validatorSize < int(committeeSize) || validatorSize <= proposerIdx || validatorSize <= nextProposerIdx {
		logger.Error("invalid committee size or validator indexes", "validatorSize", validatorSize,
			"committeeSize", committeeSize, "proposerIdx", proposerIdx, "nextProposerIdx", nextProposerIdx)
		return nil
	}

	// it cannot be happened. just to make sure
	if committeeSize < 2 {
		if committeeSize == 0 {
			logger.Error("committee size has an invalid value", "committeeSize", committeeSize)
			return nil
		}
		return []istanbul.Validator{validators[proposerIdx]}
	}

	// first committee is the proposer and the second committee is the next proposer
	committee := make([]istanbul.Validator, committeeSize)
	committee[0] = validators[proposerIdx]
	committee[1] = validators[nextProposerIdx]

	// select the reset of committee members randomly
	picker := rand.New(rand.NewSource(seed))
	pickSize := validatorSize - 2
	indexs := make([]int, pickSize)
	idx := 0
	for i := 0; i < validatorSize; i++ {
		if i != proposerIdx && i != nextProposerIdx {
			indexs[idx] = i
			idx++
		}
	}

	for i := 0; i < pickSize; i++ {
		randIndex := picker.Intn(pickSize)
		indexs[i], indexs[randIndex] = indexs[randIndex], indexs[i]
	}

	for i := uint64(0); i < committeeSize-2; i++ {
		committee[i+2] = validators[indexs[i]]
	}

	return committee
}

// SelectRandaoCommittee composes a committee selecting validators randomly based on the mixHash.
// It returns nil if the given committeeSize is bigger than validatorSize.
func SelectRandaoCommittee(validators []istanbul.Validator, committeeSize uint64, mixHash []byte) []istanbul.Validator {
	// it cannot be happened. just to make sure
	if committeeSize < 2 {
		if committeeSize == 0 {
			logger.Error("invalid committee size", "committeeSize", committeeSize)
			return nil
		}
		return validators
	}

	seed := int64(binary.BigEndian.Uint64(mixHash[:8]))
	size := committeeSize
	if committeeSize > uint64(len(validators)) {
		size = uint64(len(validators))
	}
	return shuffleValidators(validators, seed)[:size]
}

func shuffleValidators(validators istanbul.Validators, seed int64) []istanbul.Validator {
	ret := make([]istanbul.Validator, len(validators))
	copy(ret, validators)
	swap := func(x, y int) {
		ret[x], ret[y] = ret[y], ret[x]
	}

	r := rand.New(rand.NewSource(seed))
	// The Fisher-Yates algorithm used in this shuffle is deterministic for a given seed.
	r.Shuffle(len(ret), swap)

	return ret
}

func defaultSetNextProposerSeed(policy params.ProposerPolicy, proposer common.Address, proposerIdx int, round uint64) uint64 {
	seed := round
	if proposerIdx > -1 {
		seed += uint64(proposerIdx)
	}
	if policy == params.RoundRobin && !common.EmptyAddress(proposer) {
		seed += 1
	}
	return seed
}
