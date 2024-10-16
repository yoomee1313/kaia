package impl

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"sort"
	"strings"

	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/kaiax/gov"
	"github.com/kaiachain/kaia/kaiax/staking"
	"github.com/kaiachain/kaia/params"
	"github.com/kaiachain/kaia/reward"
)

type subsetCouncilSlice []common.Address

func (sc subsetCouncilSlice) Len() int {
	return len(sc)
}

func (sc subsetCouncilSlice) Less(i, j int) bool {
	return strings.Compare(sc[i].String(), sc[j].String()) < 0
}

func (sc subsetCouncilSlice) Swap(i, j int) {
	sc[i], sc[j] = sc[j], sc[i]
}

func (sc subsetCouncilSlice) AddressStringList() []string {
	var stringAddrs []string
	for _, val := range sc {
		stringAddrs = append(stringAddrs, val.String())
	}
	return stringAddrs
}

func (sc subsetCouncilSlice) getIdxByAddress(addr common.Address) int {
	for i, val := range sc {
		if addr == val {
			return i
		}
	}
	// TODO-Kaia-Istanbul: Enable this log when non-committee nodes don't call `core.startNewRound()`
	// logger.Warn("failed to find an address in the validator list",
	// 	"address", addr, "validatorAddrs", valSet.validators.AddressStringList())
	return -1
}

// SortedAddressList retrieves the sorted address list of ValidatorSet in "ascending order".
// if public is false, sort it using bytes.Compare. It's for public purpose.
// - public-false usage: (getValidators/getDemotedValidators, defaultSet snap store, prepareExtra.validators)
// if public is true, sort it using strings.Compare. It's used for internal consensus purpose, especially for the source of committee.
// - public-true usage: (snap read/store/apply except defaultSet snap store, vrank log)
// TODO-kaia-valset: unify sorting.
func (sc subsetCouncilSlice) sortedAddressList(public bool) []common.Address {
	copiedSlice := make(subsetCouncilSlice, len(sc))
	copy(copiedSlice, sc)

	if public {
		// want reverse-sort: ascending order - bytes.Compare(ValidatorSet[i][:], ValidatorSet[j][:]) > 0
		sort.Slice(copiedSlice, func(i, j int) bool {
			return bytes.Compare(copiedSlice[i].Bytes(), copiedSlice[j].Bytes()) >= 0
		})
		sort.Sort(sort.Reverse(copiedSlice))
	} else {
		// want sort: descending order - strings.Compare(ValidatorSet[i].String(), ValidatorSet[j].String()) < 0
		sort.Sort(copiedSlice)
	}
	return copiedSlice
}

type Council struct {
	blockNumber uint64
	round uint64
	rules params.Rules
	proposerPolicy params.ProposerPolicy // prevBlockResult.pSet.proposerPolicy

	// To calculate committee(num), we need council,prevHash,stakingInfo of lastProposal/prevBlock
	// which blocknumber is num - 1.
	prevBlockResult *blockResult
	qualified subsetCouncilSlice // qualified is a subset of prev block's council
	demoted   subsetCouncilSlice // demoted is a subset of prev block's council which doesn't fulfill the minimum staking amount

	// latest proposer update block's information for calculating the current block's proposers, however it is deprecated since kaia KF
	// if Council.UseProposers is false, do not use and do not calculate the proposers. see the condition at Council.UseProposers method
	// if it uses cached proposers, do not calculate the proposers
	proposers []common.Address
}

// NewCouncil returns preprocessed council(N-1). It is useful to calculate the committee(N,R) or proposer(N,R).
// defaultSet - do nothing, weightedrandom - filter out by minimum staking amount after istanbul HF and sort it
func (v *ValsetModule) NewCouncil(blockNumber uint64) (*Council, error) {
	prevBlockResult, err := v.getBlockResultsByNumber(blockNumber - 1)
	if err != nil {
		return nil, err
	}

	council := &Council {
		blockNumber: blockNumber,
		rules: v.chain.Config().Rules(big.NewInt(int64(blockNumber))),
		proposerPolicy: params.ProposerPolicy(prevBlockResult.pSet.ProposerPolicy),
		prevBlockResult: prevBlockResult,
	}

	// defaultSet does not filter out demoted validators or calculate proposers since it is PoA
	if council.proposerPolicy.IsDefaultSet(){
		council.qualified = make([]common.Address, len(council.prevBlockResult.councilAddrList))
		copy(council.qualified, council.prevBlockResult.councilAddrList)
		return council, nil
	}

	// weighted random filter out under-staked nodes since istanbul HF
	if council.rules.IsIstanbul {
		council.qualified, council.demoted = splitByMinimumStakingAmount(council.prevBlockResult)
	}

	// latest proposer update block's information for calculating the current block's proposers, however it is deprecated since kaia KF
	// in some cases, skip the proposers calculation
	//   case1. already cached at proposers
	//   case2. after kaia HF, proposers is deprecated
	//   case3. if proposer policy is not weighted random, proposers is not used
	if council.UseProposers() {
		proposerUpdateBlock := params.CalcProposerBlockNumber(blockNumber)
		cachedProposers, ok := v.proposers.Get(proposerUpdateBlock)
		if !ok {
			proposerUpdateBlockRules := v.chain.Config().Rules(big.NewInt(int64(proposerUpdateBlock)))
			proposerUpdatePrevBlockResult, err := v.getBlockResultsByNumber(proposerUpdateBlock - 1)
			if err != nil {
				return nil, err
			}
			proposerUpdateBlockQualified, _ := splitByMinimumStakingAmount(proposerUpdatePrevBlockResult)
			council.proposers = calculateProposers(proposerUpdateBlockQualified, proposerUpdatePrevBlockResult, proposerUpdateBlockRules)
			v.proposers.Add(blockNumber, council.proposers)
		} else {
			council.proposers = cachedProposers.([]common.Address)
		}
	}

	return council, nil
}

func (c Council) UseProposers() bool {
	return c.rules.IsKaia || c.proposerPolicy.IsDefaultSet()
}

func (c Council) proposer(pUpdateBnPreparedCouncil Committee, prevBnResult, pUpdateBnPrevBnResult *blockResult, rules, pUpdateBnRules params.Rules, num uint64, round uint64) (common.Address, int) {
	var (
		policy = params.ProposerPolicy(prevBnResult.pSet.ProposerPolicy)
	)
	if policy.IsDefaultSet() {
		lastProposerIdx := vals.getIdxByAddress(prevBnResult.author)
		seed := defaultSetNextProposerSeed(policy, prevBnResult.author, lastProposerIdx, round)
		idx := int(seed) % len(vals)
		return vals[idx], idx
	}

	// before Randao, weightedrandom uses proposers to calculate the proposers
	if !rules.IsRandao {
		// the proposers is calculated based on last proposer update block
		proposers := calculateProposers(pUpdateBnPreparedCouncil, pUpdateBnPrevBnResult, pUpdateBnRules)
		proposer := proposers[int(num+round)%3600%len(proposers)]
		idx := 0
		for idx = 0; idx < len(vals); idx++ {
			if vals[idx] == proposer {
				break
			}
		}
		return proposer, idx
	}

	// after Randao, proposers is deprecated.
	idx := int(round) % len(vals)
	return vals[idx], idx
}

// calcWeight updates each validator's weight based on the ratio of its staking amount vs. the total staking amount.
func calcWeight(vals []common.Address, pSet gov.ParamSet, staking *staking.StakingInfo) []uint64 {
	// make a stakingAmounts map for faster searching
	consolidatedStakingAmounts := make(map[common.Address]uint64, len(staking.NodeIds))
	for idx, nAddr := range staking.NodeIds {
		consolidatedStakingAmounts[nAddr] = staking.ConsolidatedNodes()[idx].StakingAmount
	}
	stakingAmounts := make([]float64, 0, len(vals))
	for vIdx, val := range vals {
		stakingAmounts[vIdx] = float64(consolidatedStakingAmounts[val])
	}

	// stakingInfo.Gini is calculated among all CNs (i.e. AddressBook.cnStakingContracts)
	// But we want the gini calculated among the subset of CNs (i.e. validators)
	totalStaking, gini := float64(0), reward.DefaultGiniCoefficient
	if pSet.UseGiniCoeff {
		gini = staking.Gini(pSet.MinimumStake.Uint64())
		for _, st := range stakingAmounts {
			totalStaking += math.Round(math.Pow(st, 1.0/(1+gini)))
		}
	} else {
		for _, st := range stakingAmounts {
			totalStaking += st
		}
	}
	logger.Debug("calculate totalStaking", "UseGini", pSet.UseGiniCoeff, "Gini", gini, "totalStaking", totalStaking, "stakingAmounts", stakingAmounts)

	// calculate and store each weight
	weights := make([]uint64, 0, len(vals))
	if totalStaking == 0 {
		return weights
	}
	for i := range vals {
		weight := uint64(math.Round(stakingAmounts[i] * 100 / totalStaking))
		if weight <= 0 {
			// A validator, who holds zero or small stake, has minimum weight, 1.
			weight = 1
		}
		weights[i] = weight
	}
	return weights
}

func calculateProposers(qualified subsetCouncilSlice, prevBnResult *blockResult, rules params.Rules) []common.Address {
	// Although this is for selecting proposer, update it
	// otherwise, all parameters should be re-calculated at `RefreshProposers` method.
	var candidateValsIdx []int
	if !rules.IsKore {
		weights := calcWeight(qualified, prevBnResult.pSet, prevBnResult.staking)
		for index := range qualified {
			for i := uint64(0); i < weights[index]; i++ {
				candidateValsIdx = append(candidateValsIdx, index)
			}
		}
	}

	// All validators has zero weight. Let's use all validators as candidate proposers.
	if len(candidateValsIdx) == 0 {
		for index := 0; index < len(qualified); index++ {
			candidateValsIdx = append(candidateValsIdx, index)
		}
		logger.Trace("Refresh uses all validators as candidate proposers, because all weight is zero.", "candidateValsIdx", candidateValsIdx)
	}

	// shuffle it
	proposers := make([]common.Address, len(candidateValsIdx))
	seed := convertHashToSeed(prevBnResult.header.Hash())
	picker := rand.New(rand.NewSource(seed))
	for i := 0; i < len(candidateValsIdx); i++ {
		randIndex := picker.Intn(len(candidateValsIdx))
		candidateValsIdx[i], candidateValsIdx[randIndex] = candidateValsIdx[randIndex], candidateValsIdx[i]
	}

	// copy it
	for i := 0; i < len(candidateValsIdx); i++ {
		proposers[i] = qualified[candidateValsIdx[i]]
	}
	return proposers
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

func GetNextProposerByRound() {

}

func (c Council) selectCommittee(round uint64) (subsetCouncilSlice, error) {
	if c.proposerPolicy.IsDefaultSet() || (c.proposerPolicy.IsWeightedCouncil() && !c.rules.IsRandao) {
		return c.selectRandomCommittee(c.proposers, c.blockNumber, round)
	}
	return c.selectRandaoCommittee(c.blockNumber, round)
}
// selectRandomCommittee composes a committee selecting validators randomly based on the seed value.
// It returns nil if the given committeeSize is bigger than validatorSize or proposer indexes are invalid.
func (c Council) selectRandomCommittee(proposers []common.Address, num uint64, round uint64) ([]common.Address, error) {
	proposer, proposerIdx := preparedCouncil.proposer(prevBnResult, rules,num, round)

	// a closest proposer who is not identical with the original proposer
	for i := uint64(0); i < params.ProposerUpdateInterval(); i++ {
		nextProposer, nextProposerIdx := preparedCouncil.proposer(prevBnResult,  rules, num, round+i)
		if proposer != nextProposerIdx {
			break
		}
	}
	closestDifferentProposerIdx := preparedCouncil.GetNextProposerByRound(, round)
	// nextProposer := vals.calcProposer(policy, chain, pSet, staking, prevAuthor, num, round, rules)
	// nextProposerIdx :=

	// ensure validator indexes are valid
	if proposerIdx < 0 || nextProposerIdx < 0 || proposerIdx == nextProposerIdx {
		logger.Error("invalid indexes of validators", "proposerIdx", proposerIdx, "nextProposerIdx", nextProposerIdx)
		return nil, errors.New("invalid indexes of validators")
	}

	// ensure committeeSize and proposer indexes are valid
	validatorSize := len(validators)
	if validatorSize < int(committeeSize) || validatorSize <= proposerIdx || validatorSize <= nextProposerIdx {
		logger.Error("invalid committee size or validator indexes", "validatorSize", validatorSize,
			"committeeSize", committeeSize, "proposerIdx", proposerIdx, "nextProposerIdx", nextProposerIdx)
		return nil, errors.New("invalid committee size or validator indexes")
	}

	// it cannot be happened. just to make sure
	if committeeSize < 2 {
		if committeeSize == 0 {
			logger.Error("committee size has an invalid value", "committeeSize", committeeSize)
			return nil, errors.New("committee size has an invalid value")
		}
		return []common.Address{validators[proposerIdx]}, nil
	}

	seed := convertHashToSeed(prevHash)
	if rules.IsIstanbul {
		seed += int64(round)
	}

	// first committee is the proposer and the second committee is the next proposer
	committee := make([]common.Address, committeeSize)
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

	return committee, nil
}

func convertHashToSeed(hash common.Hash) int64 {
	// Take the first 8 bytes (64 bits) of the hash and convert to int64
	bytes := hash.Bytes()[:8]
	return int64(binary.BigEndian.Uint64(bytes))
}

// SelectRandaoCommittee composes a committee selecting validators randomly based on the mixHash.
// It returns nil if the given committeeSize is bigger than validatorSize.
//
// def select_committee_KIP146(validators, committee_size, seed):
//
//	shuffled = shuffle_validators_KIP146(validators, seed)
//	return shuffled[:min(committee_size, len(validators))]
func (c Council) selectRandaoCommittee(validators []common.Address, committeeSize uint64, mixHash []byte) ([]common.Address, error) {
	// This committee must include proposers for all rounds because
	// the proposer is picked from the this committee. See weightedRandomProposer().
	if mixHash == nil {
		return nil, fmt.Errorf("nil mixHash")
	}

	// it cannot be happened. just to make sure
	if committeeSize < 2 {
		if committeeSize == 0 {
			logger.Error("invalid committee size", "committeeSize", committeeSize)
			return nil, errors.New("invalid committee size")
		}
		return validators, nil
	}

	seed := int64(binary.BigEndian.Uint64(mixHash[:8]))
	size := committeeSize
	if committeeSize > uint64(len(validators)) {
		size = uint64(len(validators))
	}

	ret := make(Council, len(validators))
	copy(ret, validators)

	rand.New(rand.NewSource(seed)).Shuffle(len(ret), ret.Swap)
	return ret[:size], nil
}

// splitByMinimumStakingAmount split the council members into qualified, demoted.
// Qualified is a subset of the council who have staked more than minimum staking amount. Demoted stakes less than minimum.
// There's two rules.
// (1) If governance mode is single, always include the governing node.
// (2) If no council members has enough KAIA, all members become qualified.
func splitByMinimumStakingAmount(prevBlockResult *blockResult) (subsetCouncilSlice, subsetCouncilSlice){
	var (
		qualified subsetCouncilSlice
		demoted subsetCouncilSlice

		// get params
		isSingleMode  = prevBlockResult.pSet.GovernanceMode == "single"
		govNode       = prevBlockResult.pSet.GoverningNode
		minStaking    = prevBlockResult.pSet.MinimumStake.Uint64()
		stakingAmount = prevBlockResult.consolidatedStakingAmount()
	)

	// filter out the demoted members who have staked less than minimum staking amounts
	for addr, val := range stakingAmount {
		if val >= minStaking || (isSingleMode && addr == govNode) {
			qualified = append(qualified, addr)
		} else {
			demoted = append(qualified, addr)
		}
	}

	// include all council members if case1 or case2
	//   case1. not a single mode && no qualified
	//   case2. single mode && len(qualified) is 1 && govNode is not qualified
	if len(qualified) == 0 || (isSingleMode && len(qualified) == 1 && stakingAmount[govNode] < minStaking) {
		demoted = subsetCouncilSlice{} // ensure demoted is empty
		qualified = make(subsetCouncilSlice, len(prevBlockResult.councilAddrList))
		copy(qualified, prevBlockResult.councilAddrList)
	}

	sort.Sort(qualified)
	sort.Sort(demoted)
	return qualified, demoted
}
