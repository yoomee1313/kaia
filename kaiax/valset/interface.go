package valset

import (
	"github.com/kaiachain/kaia/kaiax"
)

type ValsetModule interface {
	kaiax.BaseModule
	kaiax.JsonRpcModule
	kaiax.ConsensusModule

	// GetCouncil(uint64) []common.Address
}
