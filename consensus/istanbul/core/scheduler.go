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
package core

import (
	"time"

	"github.com/kaiachain/kaia/consensus/istanbul"
)

type coreTimer interface {
	Stop() bool
}

// coreScheduler separates event scheduling from consensus transitions. The
// normal implementation uses EventMux and wall time; scenario tests can queue
// the same events and timer callbacks without starting background goroutines.
type coreScheduler interface {
	Post(interface{})
	PostAsync(interface{})
	AfterFunc(time.Duration, func()) coreTimer
}

type realCoreScheduler struct {
	backend istanbul.Backend
}

func (s *realCoreScheduler) Post(ev interface{}) {
	s.backend.EventMux().Post(ev)
}

func (s *realCoreScheduler) PostAsync(ev interface{}) {
	go s.Post(ev)
}

func (s *realCoreScheduler) AfterFunc(delay time.Duration, fn func()) coreTimer {
	return time.AfterFunc(delay, fn)
}
