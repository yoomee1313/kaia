// Copyright 2026 The Kaia Authors
// This file is part of the Kaia library.
//
// The Kaia library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The Kaia library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the Kaia library. If not, see <http://www.gnu.org/licenses/>.

package system

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/kaiachain/kaia"
	"github.com/kaiachain/kaia/accounts/abi/bind"
	"github.com/kaiachain/kaia/accounts/abi/bind/backends"
	"github.com/kaiachain/kaia/blockchain"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/common/hexutil"
	abv2contracts "github.com/kaiachain/kaia/contracts/bindings/addressbookv2"
	"github.com/kaiachain/kaia/contracts/bindings/multicall"
	"github.com/kaiachain/kaia/crypto"
	"github.com/kaiachain/kaia/crypto/bls"
	"github.com/kaiachain/kaia/kaiax/valset"
	"github.com/kaiachain/kaia/log"
	"github.com/kaiachain/kaia/params"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewNodeMapFromABv2(t *testing.T) {
	addr1 := common.HexToAddress("0x0001")
	addr2 := common.HexToAddress("0x0002")
	addr3 := common.HexToAddress("0x0003")

	profiles := []multicall.Profile{
		{NodeId: addr1, State: valset.ValReady.ToUint8(), TimeoutAt: big.NewInt(100)},
		{NodeId: addr2, State: valset.ValPaused.ToUint8(), TimeoutAt: big.NewInt(200)},
		{NodeId: addr3, State: valset.ValActive.ToUint8(), TimeoutAt: big.NewInt(300)},
	}
	stakingAmounts := []*big.Int{
		new(big.Int).Mul(big.NewInt(3), big.NewInt(params.KAIA)),
		new(big.Int).Mul(big.NewInt(4), big.NewInt(params.KAIA)),
		new(big.Int).Mul(big.NewInt(5), big.NewInt(params.KAIA)),
	}

	nodes, err := NewNodeMapFromABv2(profiles, stakingAmounts)
	require.NoError(t, err)

	assert.Equal(t, valset.ValReady, nodes[addr1].State)
	assert.Equal(t, uint64(3), nodes[addr1].StakingAmount)
	assert.Equal(t, time.Unix(100, 0), nodes[addr1].IdleTimeout)
	assert.True(t, nodes[addr1].PausedTimeout.IsZero())

	assert.Equal(t, valset.ValPaused, nodes[addr2].State)
	assert.Equal(t, uint64(4), nodes[addr2].StakingAmount)
	assert.Equal(t, time.Unix(200, 0), nodes[addr2].PausedTimeout)
	assert.True(t, nodes[addr2].IdleTimeout.IsZero())

	assert.Equal(t, valset.ValActive, nodes[addr3].State)
	assert.Equal(t, uint64(5), nodes[addr3].StakingAmount)
	assert.True(t, nodes[addr3].IdleTimeout.IsZero())
	assert.True(t, nodes[addr3].PausedTimeout.IsZero())
}

func TestNewNodeMapFromABv2LengthMismatch(t *testing.T) {
	_, err := NewNodeMapFromABv2([]multicall.Profile{{}}, nil)
	require.ErrorContains(t, err, "profile/staking amount length mismatch")
}

func TestABv2SnapshotTransitionParam(t *testing.T) {
	snapshot := &ABv2Snapshot{
		ABv2TransitionParam: ABv2TransitionParam{
			EpochVACount:            10,
			PfsThreshold:            20,
			CfsThreshold:            30,
			IdleTimeout:             40 * time.Second,
			PauseTimeout:            50 * time.Second,
			MaxValActivePausedCount: 60,
		},
	}

	param := snapshot.TransitionParam()

	assert.Equal(t, uint64(10), param.EpochVACount)
	assert.Equal(t, uint64(20), param.PfsThreshold)
	assert.Equal(t, uint64(30), param.CfsThreshold)
	assert.Equal(t, 40*time.Second, param.IdleTimeout)
	assert.Equal(t, 50*time.Second, param.PauseTimeout)
	assert.Equal(t, uint64(60), param.MaxValActivePausedCount)
}

func TestABv2Saturate(t *testing.T) {
	var (
		pow2       = func(n uint) *big.Int { return new(big.Int).Lsh(common.Big1, n) }
		maxUint256 = new(big.Int).Sub(pow2(256), common.Big1)
		maxSeconds = time.Duration(math.MaxInt64/int64(time.Second)) * time.Second
		maxUnix    = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC).Unix()
	)
	cases := []struct {
		name    string
		x       *big.Int
		uint64  uint64
		seconds time.Duration
		unix    int64
	}{
		{"zero", big.NewInt(0), 0, 0, 0},
		{"normal", big.NewInt(28800), 28800, 8 * time.Hour, 28800},
		{"max duration seconds", big.NewInt(math.MaxInt64 / int64(time.Second)), math.MaxInt64 / uint64(time.Second), maxSeconds, math.MaxInt64 / int64(time.Second)},
		{"max duration seconds + 1", big.NewInt(math.MaxInt64/int64(time.Second) + 1), math.MaxInt64/uint64(time.Second) + 1, maxSeconds, math.MaxInt64/int64(time.Second) + 1},
		{"max unix", big.NewInt(maxUnix), uint64(maxUnix), maxSeconds, maxUnix},
		{"max unix + 1", big.NewInt(maxUnix + 1), uint64(maxUnix) + 1, maxSeconds, maxUnix},
		{"2^63-1", big.NewInt(math.MaxInt64), math.MaxInt64, maxSeconds, maxUnix},
		{"2^63", pow2(63), 1 << 63, maxSeconds, maxUnix},
		{"2^64-1", new(big.Int).SetUint64(math.MaxUint64), math.MaxUint64, maxSeconds, maxUnix},
		{"2^64", pow2(64), math.MaxUint64, maxSeconds, maxUnix},
		{"2^64 + 5", new(big.Int).Add(pow2(64), big.NewInt(5)), math.MaxUint64, maxSeconds, maxUnix},
		{"2^255", pow2(255), math.MaxUint64, maxSeconds, maxUnix},
		{"uint256 max", maxUint256, math.MaxUint64, maxSeconds, maxUnix},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.uint64, saturateUint64(tc.x))
			assert.Equal(t, tc.seconds, saturateSeconds(tc.x))
			assert.Equal(t, time.Unix(tc.unix, 0), saturateUnix(tc.x))
			assert.Equal(t, tc.unix, saturateUnix(tc.x).Unix())
		})
	}
}

func TestNewABv2TransitionParam(t *testing.T) {
	// Kairos values are read unchanged.
	param := NewABv2TransitionParam(multicall.MultiCallContractNodeStatesResult{
		PauseTimeout:            big.NewInt(28800),
		IdleTimeout:             big.NewInt(2592000),
		PfsThreshold:            big.NewInt(2),
		CfsThreshold:            big.NewInt(300),
		EpochVACount:            big.NewInt(4),
		MaxValActivePausedCount: big.NewInt(50),
	})
	assert.Equal(t, ABv2TransitionParam{
		EpochVACount:            4,
		PfsThreshold:            2,
		CfsThreshold:            300,
		IdleTimeout:             30 * 24 * time.Hour,
		PauseTimeout:            8 * time.Hour,
		MaxValActivePausedCount: 50,
	}, param)

	// A value whose low 64 bits are zero saturates instead of truncating to 0.
	pow64 := new(big.Int).Lsh(common.Big1, 64)
	param = NewABv2TransitionParam(multicall.MultiCallContractNodeStatesResult{
		PauseTimeout:            pow64,
		IdleTimeout:             pow64,
		PfsThreshold:            pow64,
		CfsThreshold:            pow64,
		EpochVACount:            pow64,
		MaxValActivePausedCount: pow64,
	})
	maxSeconds := time.Duration(math.MaxInt64/int64(time.Second)) * time.Second
	assert.Equal(t, ABv2TransitionParam{
		EpochVACount:            math.MaxUint64,
		PfsThreshold:            math.MaxUint64,
		CfsThreshold:            math.MaxUint64,
		IdleTimeout:             maxSeconds,
		PauseTimeout:            maxSeconds,
		MaxValActivePausedCount: math.MaxUint64,
	}, param)
}

// The deadline from a saturated timeout must survive the write to ABv2 and the read back.
func TestABv2SaturatedDeadlineRoundTrip(t *testing.T) {
	maxUint256 := new(big.Int).Sub(new(big.Int).Lsh(common.Big1, 256), common.Big1)
	blockTime := time.Unix(1700000000, 0)
	deadline := blockTime.Add(saturateSeconds(maxUint256))
	require.True(t, deadline.After(blockTime.AddDate(290, 0, 0)))

	addr := common.HexToAddress("0x0001")
	_, msg, err := EncodeProcessSystemTransition(params.Rules{}, valset.NodeMap{
		addr: {State: valset.ValInactive, IdleTimeout: deadline},
	}, 0)
	require.NoError(t, err)
	args, err := AddressBookV2ABI.Methods["processSystemTransition"].Inputs.Unpack(msg.Data()[4:])
	require.NoError(t, err)
	timeoutAts := args[2].([]*big.Int)
	require.Len(t, timeoutAts, 1)
	assert.Equal(t, deadline.Unix(), timeoutAts[0].Int64())

	nodes, err := NewNodeMapFromABv2(
		[]multicall.Profile{{NodeId: addr, State: valset.ValInactive.ToUint8(), TimeoutAt: timeoutAts[0]}},
		[]*big.Int{big.NewInt(0)},
	)
	require.NoError(t, err)
	assert.Equal(t, deadline, nodes[addr].IdleTimeout)
}

func TestNewNodeMapFromABv2HugeValues(t *testing.T) {
	var (
		addr1      = common.HexToAddress("0x0001")
		addr2      = common.HexToAddress("0x0002")
		maxUint256 = new(big.Int).Sub(new(big.Int).Lsh(common.Big1, 256), common.Big1)
		// block.timestamp + pauseTimeout from pause(); its low 64 bits are a past time.
		pausedAt = new(big.Int).Add(new(big.Int).Lsh(common.Big1, 255), big.NewInt(1700000000))
		maxTime  = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	)
	nodes, err := NewNodeMapFromABv2([]multicall.Profile{
		{NodeId: addr1, State: valset.ValInactive.ToUint8(), TimeoutAt: maxUint256},
		{NodeId: addr2, State: valset.ValPaused.ToUint8(), TimeoutAt: pausedAt},
	}, []*big.Int{maxUint256, maxUint256})
	require.NoError(t, err)

	assert.True(t, maxTime.Equal(nodes[addr1].IdleTimeout))
	assert.True(t, maxTime.Equal(nodes[addr2].PausedTimeout))
	assert.Equal(t, uint64(math.MaxUint64), nodes[addr1].StakingAmount)
}

// The real AddressBookV2 bytecode must accept the proof SignCreateNodeProof builds.
func TestSignCreateNodeProof_AcceptedByAddressBookV2(t *testing.T) {
	log.EnableLogForTest(log.LvlCrit, log.LvlWarn)

	config, keys := makeTestPermissionlessConfigWithKeys(2)
	// deleteNode only accepts Registered; node 0 stays active.
	config.NodeInfos[1].State = valset.Registered.ToUint8()
	alloc, err := AllocPermissionless(config, 0)
	require.NoError(t, err)
	// This manager deployed this staking contract, so createNode clears its deployer check.
	manager, staking := config.NodeInfos[1].Manager, config.NodeInfos[1].StakingContract
	alloc[manager] = blockchain.GenesisAccount{Balance: new(big.Int).Mul(big.NewInt(100), big.NewInt(params.KAIA))}

	backend := backends.NewSimulatedBackend(blockchain.GenesisAlloc(alloc))
	defer backend.Close()
	// Frees nodeId, staking and reward so createNode can take the staking contract again.
	deleteNodeAs(t, backend, keys[1], config.NodeIds[1])

	nodeKey, _ := crypto.GenerateKey()
	nodeId := crypto.PubkeyToAddress(nodeKey.PublicKey)
	otherKey, _ := crypto.GenerateKey()
	blsSk, err := bls.DeriveFromECDSA(nodeKey)
	require.NoError(t, err)
	abv2ABI, err := abv2contracts.AddressBookV2MetaData.GetAbi()
	require.NoError(t, err)

	createNode := func(signer *ecdsa.PrivateKey) error {
		sig, err := SignCreateNodeProof(signer, params.TestChainConfig.ChainID, manager, nodeId, staking)
		require.NoError(t, err)
		input, err := abv2ABI.Pack("createNode", nodeId, staking,
			common.BytesToAddress(crypto.Keccak256(nodeId.Bytes())), nodeId,
			abv2contracts.BlsPublicKeyInfo{PublicKey: blsSk.PublicKey().Marshal(), Pop: bls.PopProve(blsSk).Marshal()},
			"node", "", sig)
		require.NoError(t, err)
		_, err = backend.CallContract(context.Background(), kaia.CallMsg{
			From: manager, To: &AddressBookAddr, Gas: 1e7, Data: input,
		}, nil)
		return err
	}

	require.NoError(t, createNode(nodeKey), "nodeId's own proof must be accepted")
	proofInvalid := hexutil.Encode(crypto.Keccak256([]byte("NodeIdProofInvalid()"))[:4])
	assert.Equal(t, proofInvalid, revertSelector(createNode(otherKey)), "another key's proof must be rejected")
}

// revertSelector returns the 4-byte custom error selector of a reverted eth_call,
// or "" when the call succeeded or carried no revert data.
func revertSelector(err error) string {
	var revert *blockchain.RevertError
	if !errors.As(err, &revert) {
		return ""
	}
	data, _ := revert.ErrorData().(string)
	if len(data) < 10 {
		return ""
	}
	return data[:10]
}

// deleteNodeAs deletes a Registered node, freeing its addresses for reuse.
func deleteNodeAs(t *testing.T, backend *backends.SimulatedBackend, managerKey *ecdsa.PrivateKey, nodeId common.Address) {
	abv2, err := abv2contracts.NewAddressBookV2Transactor(AddressBookAddr, backend)
	require.NoError(t, err)
	tx, err := abv2.DeleteNode(bind.NewKeyedTransactor(managerKey), nodeId)
	require.NoError(t, err)
	backend.Commit()

	receipt, err := backend.TransactionReceipt(context.Background(), tx.Hash())
	require.NoError(t, err)
	require.EqualValues(t, 1, receipt.Status, "deleteNode must succeed")
}
