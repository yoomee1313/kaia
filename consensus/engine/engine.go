package engine

import (
	"crypto/ecdsa"
	"errors"

	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/consensus"
	"github.com/kaiachain/kaia/consensus/istanbul"
	istanbulBackend "github.com/kaiachain/kaia/consensus/istanbul/backend"
	"github.com/kaiachain/kaia/consensus/kaiabft"
	"github.com/kaiachain/kaia/params"
)

// EngineType identifies the consensus engine variant.
type EngineType string

const (
	Istanbul EngineType = "istanbul"
	KaiaBFT  EngineType = "kaiabft"
)

// Opts bundles the inputs needed to instantiate a consensus engine.
type Opts struct {
	EngineType     EngineType
	IstanbulConfig *istanbul.Config
	PrivateKey     *ecdsa.PrivateKey
	NodeType       common.ConnType
}

// ErrIstanbulWithAmber is returned when the Istanbul engine is selected for a
// chain with the Amber hardfork, which only KaiaBFT implements.
var ErrIstanbulWithAmber = errors.New(`the Amber hardfork needs the "kaiabft" consensus engine`)

// CheckEngine checks that the engine type implements the hardforks of the
// chain. Amber (block access lists in consensus) is implemented by KaiaBFT only.
func CheckEngine(engineType EngineType, config *params.ChainConfig) error {
	if engineType != KaiaBFT && config != nil && config.AmberCompatibleBlock != nil {
		return ErrIstanbulWithAmber
	}
	return nil
}

// NewEngine creates the consensus engine selected by the given configuration.
func NewEngine(opts *Opts) consensus.Engine {
	if opts == nil || opts.IstanbulConfig == nil || opts.PrivateKey == nil {
		panic("NewEngine receives invalid opts")
	}

	sealer := istanbul.NewSealerImpl(opts.PrivateKey)

	if opts.EngineType == KaiaBFT {
		return kaiabft.New(&kaiabft.Opts{
			Timeout:    opts.IstanbulConfig.Timeout,
			PrivateKey: opts.PrivateKey,
			NodeType:   opts.NodeType,
			Sealer:     sealer,
		})
	}

	return istanbulBackend.New(&istanbulBackend.BackendOpts{
		IstanbulConfig: opts.IstanbulConfig,
		PrivateKey:     opts.PrivateKey,
		NodeType:       opts.NodeType,
	})
}
