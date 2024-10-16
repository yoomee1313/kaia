// Copyright 2024 The Kaia Authors
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

package impl

import (
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/networks/rpc"
)

func (v *ValsetModule) APIs() []rpc.API {
	return []rpc.API{
		{
			Namespace: "kaia",
			Version:   "1.0",
			Service:   newValidatorAPI(v),
			Public:    true,
		},
		/*
			{
				Namespace: "noop",
				Version:   "1.0",
				Service:   NewNoopAPI(m),
				Public:    true,
			},
		*/
	}
}

type ValidatorAPI struct {
	v *ValsetModule
}

func newValidatorAPI(v *ValsetModule) *ValidatorAPI {
	return &ValidatorAPI{v}
}

func (api *ValidatorAPI) GetValidators(num uint64) ([]common.Address, error) {
	council := api.v.GetCouncil(num)
	if council == nil {
		return nil, ("")
	}
	return
}
