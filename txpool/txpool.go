// Copyright (c) 2018 IoTeX
// This is an alpha (internal) release and is not suitable for production. This source code is provided ‘as is’ and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package txpool

import (
	"container/heap"
	"container/list"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iotexproject/iotex-core/blockchain"
	trx "github.com/iotexproject/iotex-core/blockchain/trx"
	"github.com/iotexproject/iotex-core/common"
	"github.com/iotexproject/iotex-core/logger"
)

// Basic constant settings for TxPool
const (
	orphanTxTTL                = time.Hour / 2
	orphanTxExpireScanInterval = time.Minute * 5
	maxOrphanTxNum             = 10000
	maxOrphanTxSize            = 8192
	enableTagIndex             = false
	defaultBlockPrioritySize   = 12345
	defaultBlockMaxSize        = 23456
	defaultTxMaxNumInBlock     = 350
)

// PqDeletionWL indicates txDescPriorityQueue deletion waitlist
var PqDeletionWL map[common.Hash32B]*TxDesc

// Tag for OrphanTx
type Tag uint64

// TxDesc contains Transaction with misc
type TxDesc struct {
	Tx          *trx.Tx
	AddedTime   time.Time
	BlockHeight uint64
	Fee         int64
	FeePerKB    int64
	Priority    float64
	idx         int
}

type orphanTx struct {
	Tag            Tag
	Tx             *trx.Tx
	ExpirationTime time.Time
}

// TxSourcePointer is to refer to the output tx, hash + index in the output tx
type TxSourcePointer struct {
	Hash  common.Hash32B
	Index int32
}

// NewTxSourcePointer creates a new TxSourcePointer given TxInput
func NewTxSourcePointer(in *trx.TxInput) TxSourcePointer {
	hash := common.ZeroHash32B
	copy(hash[:], in.TxHash)
	return TxSourcePointer{
		hash,
		in.OutIndex,
	}
}

// TxPool is a pool of received txs
type TxPool interface {
	// RemoveOrphanTx remove an orphan transaction, but not its descendants
	RemoveOrphanTx(tx *trx.Tx)
	// RemoveOrphanTxsByTag remove all the orphan transactions with tag
	RemoveOrphanTxsByTag(tag Tag) uint64
	// HasOrphanTx check whether hash is an orphan transaction in the pool
	HasOrphanTx(hash common.Hash32B) bool
	// HasTxOrOrphanTx check whether hash is an accepted or orphan transaction in the pool
	HasTxOrOrphanTx(hash common.Hash32B) bool
	// RemoveTx remove an accepted transaction
	RemoveTx(tx *trx.Tx, removeDescendants bool, updateTxDescPriorityQueue bool)
	// RemoveDoubleSpends remove the double spending transaction
	RemoveDoubleSpends(tx *trx.Tx)
	// FetchTx fetch accepted transaction from the pool
	FetchTx(hash *common.Hash32B) (*trx.Tx, error)
	// MaybeAcceptTx add Tx into pool if it could be accepted
	MaybeAcceptTx(tx *trx.Tx, isNew bool, rateLimit bool) ([]common.Hash32B, *TxDesc, error)
	// ProcessOrphanTxs process the orphan txs depending on the accepted tx
	ProcessOrphanTxs(acceptedTx *trx.Tx) []*TxDesc
	// ProcessTx process the tx
	ProcessTx(tx *trx.Tx, allowOrphan bool, rateLimit bool, tag Tag) ([]*TxDesc, error)
	// TxDescs return all the transaction descs
	TxDescs() []*TxDesc
	// PickTxs return transactions to be committed to a block
	PickTxs() []*trx.Tx
	// RemoveTxInBlock remove all transactions in a block
	RemoveTxInBlock(block *blockchain.Block) error
	// LastTimePoolUpdated get the last time the pool got updated
	LastTimePoolUpdated() time.Time
}

// txPool implements TxPool interface
// Note that all locks should be placed in public functions (no lock inside of any private function)
type txPool struct {
	bc blockchain.Blockchain

	lastUpdatedUnixTime    int64
	mutex                  sync.RWMutex
	txDescs                map[common.Hash32B]*TxDesc
	txDescPriorityQueue    txDescPriorityQueue
	orphanTxs              map[common.Hash32B]*orphanTx
	orphanTxSourcePointers map[TxSourcePointer]map[common.Hash32B]*trx.Tx
	txSourcePointers       map[TxSourcePointer]*trx.Tx
	tags                   map[Tag]map[common.Hash32B]*trx.Tx
	nextExpirationScanTime time.Time
}

// NewTxPool creates a TxPool instance
func NewTxPool(bc blockchain.Blockchain) TxPool {
	return &txPool{
		bc:                     bc,
		tags:                   make(map[Tag]map[common.Hash32B]*trx.Tx),
		txDescs:                make(map[common.Hash32B]*TxDesc),
		txSourcePointers:       make(map[TxSourcePointer]*trx.Tx),
		orphanTxs:              make(map[common.Hash32B]*orphanTx),
		orphanTxSourcePointers: make(map[TxSourcePointer]map[common.Hash32B]*trx.Tx),
	}
}

// remove an orphan transaction, and all the descendant orphan transactions if removeDescendants is true
func (tp *txPool) removeOrphanTx(tx *trx.Tx, removeDescendants bool) {
	hash := tx.Hash()
	orphanTx, ok := tp.orphanTxs[hash]
	if !ok {
		logger.Info().
			Bytes("hash", hash[:]).
			Msg("cannot find orphan tx")
		return
	}

	for _, in := range orphanTx.Tx.TxIn {
		txSourcePointer := NewTxSourcePointer(in)
		orphanTxs, ok := tp.orphanTxSourcePointers[txSourcePointer]
		if ok {
			delete(orphanTxs, hash)
			if len(orphanTxs) == 0 {
				delete(tp.orphanTxSourcePointers, txSourcePointer)
			}
		}
	}

	if removeDescendants {
		txSourcePointer := TxSourcePointer{Hash: hash}
		for index := range tx.TxOut {
			txSourcePointer.Index = int32(index)
			for _, orphanTx := range tp.orphanTxSourcePointers[txSourcePointer] {
				tp.removeOrphanTx(orphanTx, true)
			}
		}
	}
	if enableTagIndex {
		tagHashs, ok := tp.tags[orphanTx.Tag]
		if ok {
			_, ok := tagHashs[hash]
			if ok {
				delete(tagHashs, hash)
				if len(tagHashs) == 0 {
					delete(tp.tags, orphanTx.Tag)
				}
			}
		}
	}
	// WARNING: is it possible that the hash is deleted twice?
	delete(tp.orphanTxs, hash)
}

// RemoveOrphanTx Remove an orphan transaction, but not its descendants
func (tp *txPool) RemoveOrphanTx(tx *trx.Tx) {
	tp.mutex.Lock()
	tp.removeOrphanTx(tx, false)
	tp.mutex.Unlock()
}

// RemoveOrphanTxsByTag removes all orphan txs with the given tag
func (tp *txPool) RemoveOrphanTxsByTag(tag Tag) uint64 {
	var retval uint64
	tp.mutex.Lock()
	if enableTagIndex {
		hashs, ok := tp.tags[tag]
		if ok {
			for _, tx := range hashs {
				tp.removeOrphanTx(tx, true)
				retval++
			}
			delete(tp.tags, tag)
		}
	} else {
		for _, orphanTx := range tp.orphanTxs {
			if orphanTx.Tag == tag {
				tp.removeOrphanTx(orphanTx.Tx, true)
				retval++
			}
		}
	}
	tp.mutex.Unlock()
	return retval
}

func (tp *txPool) deleteExpiredOrphanTxs() {
	now := time.Now()
	if now.Before(tp.nextExpirationScanTime) {
		return
	}
	for _, orphanTx := range tp.orphanTxs {
		if now.After(orphanTx.ExpirationTime) {
			tp.removeOrphanTx(orphanTx.Tx, true)
		}
	}
	tp.nextExpirationScanTime = now.Add(orphanTxExpireScanInterval)
	logger.Info().Msg("scan and delete expired orphan transactions")
}

func (tp *txPool) emptyASpaceForNewOrphanTx() error {
	if len(tp.orphanTxs) < maxOrphanTxNum {
		return nil
	}

	for _, orphanTx := range tp.orphanTxs {
		tp.removeOrphanTx(orphanTx.Tx, false)
		break
	}

	return nil
}

func (tp *txPool) addOrphanTx(tnx *trx.Tx, tag Tag) {
	if maxOrphanTxNum <= 0 {
		return
	}

	tp.deleteExpiredOrphanTxs()
	tp.emptyASpaceForNewOrphanTx()
	hash := tnx.Hash()
	tp.orphanTxs[hash] = &orphanTx{
		tag,
		tnx,
		time.Now().Add(orphanTxTTL),
	}
	if enableTagIndex {
		if _, ok := tp.tags[tag]; !ok {
			tp.tags[tag] = make(map[common.Hash32B]*trx.Tx)
		}
		tp.tags[tag][hash] = tnx
	}
	for _, txIn := range tnx.TxIn {
		txSourcePointer := NewTxSourcePointer(txIn)
		if _, ok := tp.orphanTxSourcePointers[txSourcePointer]; !ok {
			tp.orphanTxSourcePointers[txSourcePointer] = make(map[common.Hash32B]*trx.Tx)
		}
		tp.orphanTxSourcePointers[txSourcePointer][hash] = tnx
	}
	logger.Info().
		Bytes("hash", hash[:]).
		Msg("Add orphan tx to pool")
}

func (tp *txPool) maybeAddOrphanTx(tx *trx.Tx, tag Tag) error {
	serialize, error := tx.Serialize()
	if error != nil {
		return error
	}
	if len(serialize) > maxOrphanTxSize {
		return fmt.Errorf("tx %x's size is larger than limit", tx.Hash())
	}
	tp.addOrphanTx(tx, tag)

	return nil
}

func (tp *txPool) removeOrphanTxDoubleSpends(tx *trx.Tx) {
	for _, txIn := range tx.TxIn {
		for _, orphanTx := range tp.orphanTxSourcePointers[NewTxSourcePointer(txIn)] {
			tp.removeOrphanTx(orphanTx, true)
		}
	}
}

func (tp *txPool) hasTx(hash common.Hash32B) bool {
	_, ok := tp.txDescs[hash]

	return ok
}

// HasTx Check whether the pool contains tx with the given hash
func (tp *txPool) HasTx(hash common.Hash32B) bool {
	tp.mutex.RLock()
	retval := tp.hasTx(hash)
	tp.mutex.RUnlock()

	return retval
}

func (tp *txPool) hasOrphanTx(hash common.Hash32B) bool {
	_, ok := tp.orphanTxs[hash]

	return ok
}

// HasOrphanTx Check whether the pool contains orphan tx with the given hash
func (tp *txPool) HasOrphanTx(hash common.Hash32B) bool {
	tp.mutex.RLock()
	retval := tp.hasOrphanTx(hash)
	tp.mutex.RUnlock()

	return retval
}

func (tp *txPool) hasTxOrOrphanTx(hash common.Hash32B) bool {
	return tp.hasTx(hash) || tp.hasOrphanTx(hash)
}

// HasTxOrOrphanTx Check whether the pool contains tx or orphan tx with the given hash
func (tp *txPool) HasTxOrOrphanTx(hash common.Hash32B) bool {
	tp.mutex.RLock()
	retval := tp.hasTxOrOrphanTx(hash)
	tp.mutex.RUnlock()

	return retval
}

func (tp *txPool) setLastUpdateUnixTime() {
	atomic.StoreInt64(&tp.lastUpdatedUnixTime, time.Now().Unix())
}

func (tp *txPool) removeTx(tx *trx.Tx, removeDescendants bool, updateTxDescPriorityQueue bool) {
	hash := tx.Hash()
	if removeDescendants {
		txSourcePointer := TxSourcePointer{Hash: hash}
		for index := int32(0); index < int32(len(tx.TxOut)); index++ {
			txSourcePointer.Index = index
			if tx, ok := tp.txSourcePointers[txSourcePointer]; ok {
				tp.removeTx(tx, true, updateTxDescPriorityQueue)
			}
		}
	}
	desc, ok := tp.txDescs[hash]
	if !ok {
		return
	}

	for _, txIn := range desc.Tx.TxIn {
		delete(tp.txSourcePointers, NewTxSourcePointer(txIn))
	}

	delete(tp.txDescs, hash)
	tp.setLastUpdateUnixTime()
	if updateTxDescPriorityQueue {
		// Use the heap built-in Remove() to remove TxDesc pointer from txDescPriorityQueue
		heap.Remove(&tp.txDescPriorityQueue, desc.idx)
	}
}

// RemoveTx removes tx from the pool
func (tp *txPool) RemoveTx(tx *trx.Tx, removeDescendants bool, updateTxDescPriorityQueue bool) {
	tp.mutex.Lock()
	tp.removeTx(tx, removeDescendants, updateTxDescPriorityQueue)
	tp.mutex.Unlock()
}

// RemoveDoubleSpends removes all transactions which share source pointers with input tx
func (tp *txPool) RemoveDoubleSpends(tx *trx.Tx) {
	tp.mutex.Lock()
	hash := tx.Hash()
	for _, txIn := range tx.TxIn {
		txSourcePointer := NewTxSourcePointer(txIn)
		if txDescendant, ok := tp.txSourcePointers[txSourcePointer]; ok {
			if txDescendant.Hash() != hash {
				tp.removeTx(txDescendant, true, true)
			}
		}
	}
	tp.mutex.Unlock()
}

func (tp *txPool) addTx(utxoTracker *blockchain.UtxoTracker, tx *trx.Tx, height uint64, fee int64) *TxDesc {
	serialize, err := tx.Serialize()
	if err != nil {
		return nil
	}
	desc := TxDesc{
		Tx:          tx,
		AddedTime:   time.Now(),
		BlockHeight: height,
		Fee:         fee,
		FeePerKB:    fee * 1000 / int64(len(serialize)),
		Priority:    float64(0),
	}
	tp.txDescs[tx.Hash()] = &desc
	heap.Push(&tp.txDescPriorityQueue, &desc)
	for _, txIn := range tx.TxIn {
		tp.txSourcePointers[NewTxSourcePointer(txIn)] = tx
	}
	tp.setLastUpdateUnixTime()

	return &desc
}

// Check whether any of tx's inputs have been spent by other transactions
func (tp *txPool) checkPoolDoubleSpend(tx *trx.Tx) error {
	for _, txIn := range tx.TxIn {
		txSourcePointer := NewTxSourcePointer(txIn)
		if txSpend, ok := tp.txSourcePointers[txSourcePointer]; ok {
			return fmt.Errorf("%v has already been spent by %v", txSourcePointer, txSpend.Hash())
		}
	}

	return nil
}

// IsFullySpent Check whether the output txs have been fully spent
func IsFullySpent(outputs []*trx.TxOutput) bool {
	return false
}

func (tp *txPool) fetchInputUtxos(tx *trx.Tx) (*blockchain.UtxoTracker, error) {
	utxoTracker := blockchain.NewUtxoTracker()
	utxoPool := tp.bc.UtxoPool()
	for _, txIn := range tx.TxIn {
		hash := common.ZeroHash32B
		copy(hash[:], txIn.TxHash)
		outputs := utxoPool[hash]
		utxoTracker.GetPool()[hash] = outputs
	}

	// attempt to populate any missing input from the transaction pool
	for hash, outputs := range utxoTracker.GetPool() {
		if outputs != nil && !IsFullySpent(outputs) {
			continue
		}
		if desc, ok := tp.txDescs[hash]; ok {
			utxoTracker.AddTx(desc.Tx, 0)
		}
	}

	return utxoTracker, nil
}

// FetchTx gets the tx with the given hash
func (tp *txPool) FetchTx(hash *common.Hash32B) (*trx.Tx, error) {
	tp.mutex.RLock()
	desc, ok := tp.txDescs[*hash]
	tp.mutex.RUnlock()
	if ok {
		return desc.Tx, nil
	}

	return nil, fmt.Errorf("cannot find transaction in the pool")
}

func calculateMinFee(size uint32) int64 {
	return 0
}

func (tp *txPool) maybeAcceptTx(tx *trx.Tx, isNew bool, rateLimit bool, rejectDuplicateOrphanTxs bool) ([]common.Hash32B, *TxDesc, error) {
	hash := tx.Hash()
	if tp.hasTx(hash) || (rejectDuplicateOrphanTxs && tp.hasOrphanTx(hash)) {
		return nil, nil, fmt.Errorf("duplicate transaction")
	}
	if tx.IsCoinbase() {
		return nil, nil, fmt.Errorf("unexpected coinbase transaction")
	}

	err := tp.checkPoolDoubleSpend(tx)
	if err != nil {
		return nil, nil, err
	}

	utxoTracker, err := tp.fetchInputUtxos(tx)
	if err != nil {
		// if it is chain rule error
		//   return chain rule error
		return nil, nil, err
	}

	outputs := utxoTracker.GetPool()[hash]
	if outputs != nil && !IsFullySpent(outputs) {
		// return nil, nil, fmt.Errorf("duplicate transaction")
	}
	delete(utxoTracker.GetPool(), hash)

	var missingParents []common.Hash32B
	for originHash, outputs := range utxoTracker.GetPool() {
		if outputs == nil || IsFullySpent(outputs) {
			missingParents = append(missingParents, originHash)
		}
	}
	if len(missingParents) > 0 {
		return missingParents, nil, nil
	}
	if int64(tx.LockTime) > time.Now().Unix() {
		return nil, nil, fmt.Errorf("tx %s is still in lock", hash)
	}

	fee := int64(0)
	size := tx.TotalSize()
	minFee := calculateMinFee(size)
	if (size >= defaultBlockPrioritySize-1000) && fee < minFee {
		return nil, nil, fmt.Errorf("fee is lower than min requirement fee")
	}

	height, err := tp.bc.TipHeight()
	if err != nil {
		return nil, nil, err
	}
	txDesc := tp.addTx(utxoTracker, tx, height, fee)

	return nil, txDesc, nil
}

// MaybeAcceptTx Add Tx into pool if it will be accepted
func (tp *txPool) MaybeAcceptTx(tx *trx.Tx, isNew bool, rateLimit bool) ([]common.Hash32B, *TxDesc, error) {
	tp.mutex.Lock()
	hashes, desc, error := tp.maybeAcceptTx(tx, isNew, rateLimit, true)
	tp.mutex.Unlock()

	return hashes, desc, error
}

func (tp *txPool) processOrphanTxs(acceptedTx *trx.Tx) []*TxDesc {
	var acceptedTxDescs []*TxDesc
	processList := list.New()
	processList.PushBack(acceptedTx)
	// remove all descendants
	for processList.Len() > 0 {
		firstElement := processList.Remove(processList.Front())
		item := firstElement.(*trx.Tx)
		txSourcePointer := TxSourcePointer{Hash: item.Hash()}
		for idx := range item.TxOut {
			txSourcePointer.Index = int32(idx)
			orphanTxs, ok := tp.orphanTxSourcePointers[txSourcePointer]
			if !ok {
				continue
			}
			for _, tx := range orphanTxs {
				missingParents, desc, err := tp.maybeAcceptTx(
					tx,
					true,  // is new
					true,  // rate limit
					false, // reject duplicate
				)
				if err != nil {
					tp.removeOrphanTx(tx, true)
					break
				}
				if len(missingParents) > 0 {
					continue
				}
				acceptedTxDescs = append(acceptedTxDescs, desc)
				tp.removeOrphanTx(tx, false)
				processList.PushBack(tx)
				break
			}
		}
	}

	tp.removeOrphanTxDoubleSpends(acceptedTx)
	for _, desc := range acceptedTxDescs {
		tp.removeOrphanTxDoubleSpends(desc.Tx)
	}

	return acceptedTxDescs
}

// ProcessOrphanTxs Go through all the orphan txs, and process the ones depending on the accepted tx
func (tp *txPool) ProcessOrphanTxs(acceptedTx *trx.Tx) []*TxDesc {
	tp.mutex.Lock()
	acceptedTxDescs := tp.processOrphanTxs(acceptedTx)
	tp.mutex.Unlock()

	return acceptedTxDescs
}

// ProcessTx Process the tx as accepted or orphan
func (tp *txPool) ProcessTx(tx *trx.Tx, allowOrphan bool, rateLimit bool, tag Tag) ([]*TxDesc, error) {
	// Protect concurrent access.
	tp.mutex.Lock()
	defer tp.mutex.Unlock()

	missingParents, desc, err := tp.maybeAcceptTx(
		tx,
		true, // is new
		rateLimit,
		true, // prevent duplications
	)
	if err != nil {
		return nil, err
	}

	// if no missing parents, return all the descendants with no missing parents
	if len(missingParents) == 0 {
		newTxDescs := tp.processOrphanTxs(tx)
		acceptedTxDescs := make([]*TxDesc, len(newTxDescs)+1)

		acceptedTxDescs[0] = desc
		copy(acceptedTxDescs[1:], newTxDescs)

		return acceptedTxDescs, nil
	}

	if !allowOrphan {
		return nil, fmt.Errorf("orphan transaction %v references "+
			"outputs of unknown or fully-spent "+
			"transaction %v", tx.Hash(), missingParents[0])
	}

	err = tp.maybeAddOrphanTx(tx, tag)
	return nil, err
}

// Count The number of accepted txs in the pool
func (tp *txPool) count() int {
	tp.mutex.RLock()
	count := len(tp.txDescs)
	tp.mutex.RUnlock()

	return count
}

// TxDescs The list of descs of accepted txs
func (tp *txPool) TxDescs() []*TxDesc {
	tp.mutex.RLock()
	txDescs := make([]*TxDesc, len(tp.txDescs))
	i := 0
	for _, desc := range tp.txDescs {
		txDescs[i] = desc
		i++
	}
	tp.mutex.RUnlock()

	return txDescs
}

// PickTxs returns the list of txs to be committed to a block
func (tp *txPool) PickTxs() []*trx.Tx {
	tp.mutex.RLock()
	tp.updateTxDescPriority()
	PqDeletionWL = map[common.Hash32B]*TxDesc{}
	txs := []*trx.Tx{}
	curSize := uint32(0)
	for len(tp.txDescPriorityQueue) > 0 {
		tx := tp.txDescPriorityQueue[0].Tx
		if curSize+tx.TotalSize() > defaultBlockMaxSize || len(txs) >= defaultTxMaxNumInBlock {
			break
		}
		curSize += tx.TotalSize()
		txs = append(txs, tx)

		PqDeletionWL[tx.Hash()] = heap.Pop(&tp.txDescPriorityQueue).(*TxDesc)
	}
	tp.mutex.RUnlock()

	return txs
}

// RemoveTxInBlock removes the transaction in the block from pool
func (tp *txPool) RemoveTxInBlock(block *blockchain.Block) error {
	tp.mutex.Lock()
	for _, tx := range block.Tranxs {
		tp.removeTx(tx, true, false)
		delete(PqDeletionWL, tx.Hash())
	}
	for _, txDesc := range PqDeletionWL {
		heap.Push(&tp.txDescPriorityQueue, txDesc)
	}
	tp.mutex.Unlock()
	return nil
}

// LastTimePoolUpdated The last unix time the pool get updated
func (tp *txPool) LastTimePoolUpdated() time.Time {
	return time.Unix(atomic.LoadInt64(&tp.lastUpdatedUnixTime), 0)
}

// Dynamically update the priority of each TxDesc in TxDescPriorityQueue
func (tp *txPool) updateTxDescPriority() {
	tp.setLastUpdateUnixTime()
	for idx, txDesc := range tp.txDescPriorityQueue {
		txSize := txDesc.Tx.TotalSize()
		amount := float64(0)
		for _, txOut := range txDesc.Tx.TxOut {
			amount += float64(txOut.Value)
		}
		txDesc.Priority = (float64(tp.lastUpdatedUnixTime-int64(txDesc.AddedTime.Nanosecond())) * amount) / float64(txSize)
		heap.Fix(&tp.txDescPriorityQueue, idx)
	}
}
