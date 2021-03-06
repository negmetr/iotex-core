// Copyright (c) 2018 IoTeX
// This is an alpha (internal) release and is not suitable for production. This source code is provided ‘as is’ and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package rolldpos

import (
	"github.com/iotexproject/iotex-core/consensus/fsm"
)

// ruleDKGGenerate checks if the event is dkg generate
type ruleDKGGenerate struct {
	*RollDPoS
}

func (r ruleDKGGenerate) Condition(event *fsm.Event) bool {
	// Prevent cascading transition to DKG_GENERATE when moving back to EPOCH_START
	if event.State == stateEpochStart {
		return false
	}
	// Trigger the proposer election after entering the first round of consensus in an epoch if no delay
	if r.cfg.ProposerInterval == 0 {
		r.prnd.Do()
	}
	return true
}
