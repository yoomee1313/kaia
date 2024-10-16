package impl

import (
	"errors"
)

var (
	errInitUnexpectedNil    = errors.New("unexpected nil during module init")
	errExtractIstanbulExtra = errors.New("extract Istanbul Extra from block header of the given block number")
	errNilHeader            = errors.New("nil block header")
)
