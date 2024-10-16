package impl

import (
	"encoding/json"
	"fmt"

	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/storage/database"
)

// read valset from db
// store valset at db
var councilAddressPrefix = []byte("councilAddress")

func councilAddressKey(num uint64) []byte {
	return append(councilAddressPrefix, common.Int64ToByteLittleEndian(num)...)
}

func ReadCouncilAddressListFromDb(db database.Database, voteBlk uint64) ([]common.Address, error) {
	b, err := db.Get(councilAddressKey(voteBlk))
	if err != nil || len(b) == 0 {
		return nil, fmt.Errorf("failed to read council addresses from db at voteBlk %d. error=%v, b=%v", voteBlk, err, string(b))
	}

	var set []common.Address
	if err = json.Unmarshal(b, &set); err != nil {
		return nil, fmt.Errorf("failed to unmarshal encoded council addresses at voteBlk %d. err=%v", voteBlk, err)
	}
	return set, nil
}

func WriteCouncilAddressListToDb(db database.Database, voteBlk uint64, councilAddressList []common.Address) error {
	b, err := json.Marshal(councilAddressList)
	if err != nil {
		return err
	}
	if err = db.Put(councilAddressKey(voteBlk), b); err != nil {
		return err
	}
	return nil
}
