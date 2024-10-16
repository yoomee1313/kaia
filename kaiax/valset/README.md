# kaiax/valset

This module is responsible for getting/storing council and calculating committee and proposer.

## Concepts
### council: a list of registered CN
A member of council is added/removed by "governance.addvalidator" vote. But the genesis council is restored via genesis extraData.
The council(N) is decided after the block(N).vote and govParam(N) is applied, and it will be used to calculate next committee or proposer.

### committee: a subset of council who participates on consensus
A committee(N, R) is calculated based on previous block's results(council, prevHeader, stakingInfo of council) and sorted. However, the genesis committee is copied from the genesis council.
- minimum staking amount - The council members who have less than minimum staking amount is demoted, so it cannot be a member of committee. 
- committeesize - The committee size can be updated via "istanbul.committeeSize". It decides the size of committee.
- committee shuffle seed - The seed is calculated using previous block's information. The copied council is shuffled with the calculated seed to get the committee.

Committee selection logic is different before/after Randao Hardfork when it's proposer policy is weightedCouncil. So the condition to activate RandaoCommittee is `policy.IsDefaultSet() || (policy.IsWeightedCouncil() && !rules.IsRandao)`
- committee shuffle seed calculation logic
  - before Randao: the seed is calculated using prevHash. `seed = int64(binary.BigEndian.Uint64(prevHash.Bytes()[:8]))`. 
  - after Randao: the seed is calculated using mixHash. `seed = int64(binary.BigEndian.Uint64(mixHash[:8]))`
- council shuffle
  - before Randao: extract (proposer, next proposer which is differnt from proposer) and shuffle it. Attach the proposers again and slice the council.
  - after Randao: shuffle the council and slice the committee.

Example of BeforeRandaoCommittee
```go
Condition: proposerIdx = 3, nextProposerIdx = 7, committeesize = 6, council = [0,1,2,3,4,5,6,7,8,9]
Step1. extract proposers to committee: proposers = [3,7], council = [0,1,2,4,5,6,8,9]
Step2. shuffle the council. proposers = [3,7], council = [4,5,6,8,9,0,1,2]
Step3. merge. council = [3,7,4,5,6,8,9,0,1,2]
Step4. slice the council by committee size. committee = [3,7,4,5,6,8]
```

### proposer: a member of committee who proposes the block
A proposer proposes the block. We call as author after the block is created.

seed에 따라 proposers 또는 valSet에서 pick 됨.
proposer(N) = council(N-1).CalcProposer(block(N-1).Hash(), )

#### ProposerPolicy 
Proposer is selected based on the proposer policy the network chosen. Also, each proposer selection logic has been updated per HF.
- simple (RoundRobin, Sticky)
- WeightedRandom
  - we need the proposerUpdateInterval block's result is used to calculate the proposer.

However, the selection of proposer policy is limited by consensus algorithm.
- clique - 0: RoundRobin [default: 0]
- istanbul - 0: RoundRobin, 1: Sticky, 2: WeightedRandom [default: 0]

Proposer

## Persistent Schema
### Valset
Store at miscDB
- validator voteBlks - validator voteBlks
- ReadCouncil(n) - n is the addvalidator/removevalidator voting blks
- StoreCouncil(n, council) - n is the addvalidator/removevalidator voting blks

## In-memory Structures
###  ValidatorSet
- proposers (deprecated since randao/kaia HF): weight에 따른 proposers 조직 및 셔플링.

## Module lifecycle

### Init

- Dependencies:
    - ChainDB: Raw key-value database to access this module's persistent schema.
    - ChainConfig: Holds the StakingInterval value at genesis.
    - Chain: Provides the blocks and states.
    - kaiax/gov: Provides the useGini and minStake for the API.
- Notable dependents:
    - kaiax/valset: To filter validators based on the staking amounts.
    - kaiax/reward: To calculate the rewards based on the staking amounts.

### Start and stop

This module does not have any background threads.

## Block processing

### Consensus

This module does not have any consensus-related block processing logic.

### Execution

This module does not have any execution-related block processing logic. This module could have pre-calculate the StakingInfo for the next block (after Kaia hardfork), but it still can be calculated on-demand, hence no execution hooks.

### Rewind

Upon rewind, this module deletes the related persistent data and flushes the in-memory cache.

## APIs

### kaia_getStakingInfo, governance_getStakingInfo


## Getters
```python
council(N) = a council of block N.
committee(N, Round) = council(N-1).GetCommittee(block(N-1).hash(), block(N-1).author, view{N, round})
```
### validator
### 
seed별로 (valid)valset을 셔플한뒤, subSize로 짜름. proposer는 0번째 idx.



## Structure
### (valid)valSet: sequence(block)마다 (valid)valset은 지정되어 있다. demoted는 제외가 되어있다.
    - HandleGovernanceVote - voting마다 업데이트. reward,voting,demoted는 나중에 업뎃됨.
    - refreshValSet - stakingInfo에 따른 demoted 필터링, weight 계산.
      (1) (valid)valSet은 매블록 혹은 stakingUpdateInterval, epoch 마다 업데이트됨.
    - (매블록) addval, removeval
    - (epoch - 604800 블록) mintingamount, mode, governingnode. 예를 들어, singlemode면 governingnode가 무조건 (valid)valSet에 포함되어야함.
    - (stakingInterval - 86400 블록) weight, demoted가 업뎃됨.
      (2) istanbul 이후부터 stakingInterval(86400) 블록마다 demoted 계산. 새롭게 addval된 애들은 staking 정보 없어 demoted,zero weight(1000 voting power)임.
      (3) kore 이후부터 stakingInfo에 따른 weight가 zero로 하드코딩됨. -> 나중에 proposers에 영향줌.
      (4) kaia 이후부터 stakingInterval이 1로 하드코딩됨. 즉, minimum staking amount는 매블록 계산되게됨.
### proposers (deprecated since randao/kaia HF): weight에 따른 proposers 조직 및 셔플링.
    - refreshProposers
      (1) proposers는 proposersUpdateInterval(3600) 마다 업데이트됨. 즉, 새로운 validator가 proposer가 되려면 다음 interval까지 기다려야함.
      (2) weight 에 따라 slot을 재배정. 만약 weight가 모두 0이라면, validator 복사.
      (2) seed에 따라 셔플함. valSet.RefreshProposers에서 seed를 가지고 있음.
      (3) kaia 이후부터 proposers는 deprecate되어 쓰이지 않음. (kaia 블록이후부터 nil로 초기화해도 무방..)
### proposer: seed에 따라 proposers 또는 valSet에서 pick 됨.
    - calcProposer: Proposers가 업데이트된 이후로 한칸씩 더해서 업뎃.
      (1) kaia 이전: Proposers 사용. seed는 (blockNum+round)%proposerUpdateInterval(3600) %
### committee: seed별로 (valid)valset을 셔플한뒤, subSize로 짜름. proposer는 0번째 idx.
    - subListWithProposer:
      (1) kaia 이전: 현재 Proposer와 이와 다른 제일 가까운 다음 proposer를 proposers에서 뽑아서 0,1번째에 포함시키고 라운드별로 valSet 셔플한뒤 subsize로 짜름.
      (2) kaia 이후: 블록당 한번만 valSet 셔플함. n번째 idx of committee는 n번째 round의 proposer.
