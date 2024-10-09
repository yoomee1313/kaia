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
// This file is derived from quorum/consensus/istanbul/validator.go (2018/06/04).
// Modified and improved for the klaytn development.
// Modified and improved for the Kaia development.

package istanbul

import (
	"bytes"
	"sort"
	"strings"

	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/params"
)

type Validator interface {
	// Address returns address
	Address() common.Address

	// String representation of Validator
	String() string
	Copy() Validator

	RewardAddress() common.Address
	VotingPower() uint64
	Weight() uint64
}

// ----------------------------------------------------------------------------

type Validators []Validator

func (slice Validators) Len() int {
	return len(slice)
}

func (slice Validators) Less(i, j int) bool {
	return strings.Compare(slice[i].String(), slice[j].String()) < 0
}

func (slice Validators) Swap(i, j int) {
	slice[i], slice[j] = slice[j], slice[i]
}

func (slice Validators) AddressStringList() []string {
	var stringAddrs []string
	for _, val := range slice {
		stringAddrs = append(stringAddrs, val.Address().String())
	}
	return stringAddrs
}

func (slice Validators) Copy() Validators {
	copiedSlice := make(Validators, 0, len(slice))
	for _, val := range slice {
		copiedSlice = append(copiedSlice, val.Copy())
	}
	return copiedSlice
}

// SortedAddressList retrieves the sorted address list of validators in "ascending order".
// if public is false, sort it using bytes.Compare. It's for public purpose.
// - public-false usage: (getValidators/getDemotedValidators, defaultSet snap store, prepareExtra.validators)
// if public is true, sort it using strings.Compare. It's used for internal consensus purpose, especially for the source of committee.
// - public-true usage: (snap read/store/apply except defaultSet snap store, vrank log)
// TODO-kaia-valset: unify sorting.
func (slice Validators) SortedAddressList(public bool) []common.Address {
	var (
		copiedSlice = slice.Copy()
		stringAddrs = make([]common.Address, 0, len(slice))
	)

	// Sorting based on the public flag
	if public {
		// want reverse-sort: ascending order - bytes.Compare(validators[i][:], validators[j][:]) > 0
		sort.Slice(copiedSlice, func(i, j int) bool {
			return bytes.Compare(copiedSlice[i].Address().Bytes(), copiedSlice[j].Address().Bytes()) < 0
		})
		sort.Sort(sort.Reverse(copiedSlice))
	} else {
		// want sort: descending order - strings.Compare(validators[i].String(), validators[j].String()) < 0
		sort.Sort(copiedSlice)
	}

	// extract the address list from the validator list
	for _, val := range copiedSlice {
		stringAddrs = append(stringAddrs, val.Address())
	}
	return stringAddrs
}

// ----------------------------------------------------------------------------

type ValidatorSet interface {
	// Calculate the proposer
	CalcProposer(lastProposer common.Address, round uint64)
	// Return the validator size
	Size() uint64
	// Return the sub validator group size
	SubGroupSize() uint64
	// Set the sub validator group size
	SetSubGroupSize(size uint64)
	// Return the validator array
	List() Validators
	// Return the demoted validator array
	DemotedList() Validators
	// SubList composes a committee after setting a proposer with a default value.
	SubList(prevHash common.Hash, view *View) []Validator
	// Return whether the given address is one of sub-list
	CheckInSubList(prevHash common.Hash, view *View, addr common.Address) bool
	// SubListWithProposer composes a committee with given parameters.
	SubListWithProposer(prevHash common.Hash, proposer common.Address, view *View) []Validator
	// Get validator by index
	GetByIndex(i uint64) Validator
	// Get validator by given address
	GetByAddress(addr common.Address) (int, Validator)
	// Get demoted validator by given address
	GetDemotedByAddress(addr common.Address) (int, Validator)
	// Get current proposer
	GetProposer() Validator
	// Check whether the validator with given address is a proposer
	IsProposer(address common.Address) bool
	// Add validator
	AddValidator(address common.Address) bool
	// Remove validator
	RemoveValidator(address common.Address) bool
	// Copy validator set
	Copy() ValidatorSet
	// Get the maximum number of faulty nodes
	F() int
	// Get proposer policy
	Policy() params.ProposerPolicy

	IsSubSet() bool

	// Refreshes a list of validators at given blockNum
	RefreshValSet(blockNum uint64, config *params.ChainConfig, isSingle bool, governingNode common.Address, minStaking uint64) error

	// Refreshes a list of candidate proposers with given hash and blockNum
	RefreshProposers(hash common.Hash, blockNum uint64, config *params.ChainConfig) error

	SetMixHash(mixHash []byte)

	TotalVotingPower() uint64

	GetNextProposerByRound(lastProposer common.Address, round uint64) Validator

	ApplyValSetFromVoteSnapshot(config *params.ChainConfig, number uint64, committeeSize uint64, mixHash []byte)
	GetSnapshotJSONValSetData() (validators []common.Address, demotedValidators []common.Address,
		rewardAddrs []common.Address, votingPowers []uint64, weights []uint64, proposers []common.Address, proposersBlockNum uint64, mixHash []byte)
}

// ----------------------------------------------------------------------------

type ProposalSelector func(ValidatorSet, common.Address, uint64) Validator
