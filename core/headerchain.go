// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"bytes"
	crand "crypto/rand"
	"errors"
	"fmt"
	"math"
	"math/big"
	mrand "math/rand"
	"sort"
	"sync"
	"sync/atomic"

	lru "github.com/hashicorp/golang-lru"
	"github.com/spruce-solutions/go-quai/common"
	"github.com/spruce-solutions/go-quai/consensus"
	"github.com/spruce-solutions/go-quai/consensus/misc"
	"github.com/spruce-solutions/go-quai/core/rawdb"
	"github.com/spruce-solutions/go-quai/core/types"
	"github.com/spruce-solutions/go-quai/core/vm"
	"github.com/spruce-solutions/go-quai/ethdb"
	"github.com/spruce-solutions/go-quai/event"
	"github.com/spruce-solutions/go-quai/log"
	"github.com/spruce-solutions/go-quai/metrics"
	"github.com/spruce-solutions/go-quai/params"
)

var (
	headBlockGauge     = metrics.NewRegisteredGauge("chain/head/block", nil)
	headHeaderGauge    = metrics.NewRegisteredGauge("chain/head/header", nil)
	headFastBlockGauge = metrics.NewRegisteredGauge("chain/head/receipt", nil)

	blockReorgMeter         = metrics.NewRegisteredMeter("chain/reorg/executes", nil)
	blockReorgAddMeter      = metrics.NewRegisteredMeter("chain/reorg/add", nil)
	blockReorgDropMeter     = metrics.NewRegisteredMeter("chain/reorg/drop", nil)
	blockReorgInvalidatedTx = metrics.NewRegisteredMeter("chain/reorg/invalidTx", nil)
)

const (
	headerCacheLimit = 512
	tdCacheLimit     = 1024
	numberCacheLimit = 2048
)

// HeaderChain is responsible for maintaining the header chain including the
// header query and updating.
//
// The components maintained by headerchain includes: (1) total difficult
// (2) header (3) block hash -> number mapping (4) canonical number -> hash mapping
// and (5) head header flag.

type HeaderChain struct {
	config *params.ChainConfig

	bc *BlockChain

	headerDb      ethdb.Database
	genesisHeader *types.Header

	currentHeader     atomic.Value // Current head of the header chain (may be above the block chain!)
	currentHeaderHash common.Hash  // Hash of the current head of the header chain (prevent recomputing all the time)

	headerCache *lru.Cache // Cache for the most recent block headers
	tdCache     *lru.Cache // Cache for the most recent block total difficulties
	numberCache *lru.Cache // Cache for the most recent block numbers

	procInterrupt func() bool

	rand          *mrand.Rand
	engine        consensus.Engine
	chainHeadFeed event.Feed
	headermu      sync.RWMutex
	heads         []*types.Header
}

// NewHeaderChain creates a new HeaderChain structure. ProcInterrupt points
// to the parent's interrupt semaphore.
func NewHeaderChain(db ethdb.Database, cacheConfig *CacheConfig, chainConfig *params.ChainConfig, domClientUrl string, subClientUrls []string, engine consensus.Engine, vmConfig vm.Config, shouldPreserve func(header *types.Header) bool, txLookupLimit *uint64) (*HeaderChain, error) {
	headerCache, _ := lru.New(headerCacheLimit)
	tdCache, _ := lru.New(tdCacheLimit)
	numberCache, _ := lru.New(numberCacheLimit)

	// Seed a fast but crypto originating random generator
	seed, err := crand.Int(crand.Reader, big.NewInt(math.MaxInt64))
	if err != nil {
		return nil, err
	}

	hc := &HeaderChain{
		config:        chainConfig,
		headerDb:      db,
		headerCache:   headerCache,
		tdCache:       tdCache,
		numberCache:   numberCache,
		procInterrupt: procInterrupt,
		rand:          mrand.New(mrand.NewSource(seed.Int64())),
		engine:        engine,
	}

	hc.bc, err = NewBlockChain(db, cacheConfig, chainConfig, engine, vmConfig)
	if err != nil {
		return nil, err
	}

	hc.genesisHeader = hc.GetHeaderByNumber(0)
	if hc.genesisHeader == nil {
		return nil, ErrNoGenesis
	}

	hc.currentHeader.Store(hc.genesisHeader)
	if head := rawdb.ReadHeadBlockHash(db); head != (common.Hash{}) {
		if chead := hc.GetHeaderByHash(head); chead != nil {
			hc.currentHeader.Store(chead)
		}
	}
	hc.currentHeaderHash = hc.CurrentHeader().Hash()
	headHeaderGauge.Update(hc.CurrentHeader().Number[types.QuaiNetworkContext].Int64())

	return hc, nil
}

// Append
func (hc *HeaderChain) Append(block *types.Block) error {
	hc.headermu.Lock()
	defer hc.headermu.Unlock()

	// Append header to the headerchain
	batch := hc.headerDb.NewBatch()
	rawdb.WriteHeader(batch, block.Header())
	if err := batch.Write(); err != nil {
		return err
	}

	// Append block else revert header append
	err := hc.bc.Append(block)
	if err != nil {
		rawdb.DeleteHeader(hc.headerDb, block.Header().Hash(), block.Header().Number64())
		return err
	}

	var nilHeader *types.Header
	// check if the size of the queue is at the maxHeadsQueueLimit
	if len(hc.heads) == maxHeadsQueueLimit {

		// Trim the branch before dequeueing
		err = hc.trimBranch(hc.heads[0], hc.heads[maxHeadsQueueLimit-1])
		if err != nil {
			return err
		}

		// dequeue
		hc.heads[0] = nilHeader
		hc.heads = hc.heads[1:]
	}
	// Add to the heads queue
	hc.heads = append(hc.heads, block.Header())

	// Sort the heads by number
	sort.Slice(hc.heads, func(i, j int) bool {
		return hc.heads[i].Number[types.QuaiNetworkContext].Uint64() < bc.heads[j].Number[types.QuaiNetworkContext].Uint64()
	})

	return nil
}

// SetCurrentHeader sets the in-memory head header marker of the canonical chan
// as the given header.
func (hc *HeaderChain) SetCurrentHeader(head *types.Header) {
	hc.currentHeader.Store(head)
	hc.currentHeaderHash = head.Hash()
	headHeaderGauge.Update(head.Number[types.QuaiNetworkContext].Int64())
}

// Trim
func (hc *HeaderChain) trim(commonHeader *types.Header, startHeader *types.Header) error {
	parent := startHeader
	// Delete each header until common is found
	for {
		if parent.Hash() == commonHeader.Hash() {
			break
		}

		// Delete the header and the block
		rawdb.DeleteHeader(hc.headerDb, parent.Hash(), parent.Number64())
		hc.bc.Trim(parent)

		parent = hc.GetHeader(parent.Parent(), parent.Number64()-1)

		if parent == nil {
			log.Warn("unable to trim blockchain state, one of trimmed blocks not found")
			return nil
		}
	}
	return nil
}

// TrimBranch
func (hc *HeaderChain) trimBranch(oldHeader *types.Header, newHeader *types.Header) error {
	startIndex := oldHeader.Number64()
	startHeader := oldHeader

	for {
		if newHeader.Number64() == startIndex {
			break
		}
		newHeader = hc.GetHeader(newHeader.ParentHash[types.QuaiNetworkContext], newHeader.Number64()-1)
	}

	var commonHeader *types.Header

	// Both sides of the reorg are at the same number, reduce both until the common
	// ancestor is found
	for {
		// If the common ancestor was found, bail out
		if oldHeader.Hash() == newHeader.Hash() {
			commonHeader = oldHeader
			break
		}

		// Step back with both chains
		oldHeader := hc.GetHeader(oldHeader.Parent(), oldHeader.Number64()-1)
		if oldHeader == nil {
			return fmt.Errorf("invalid old chain")
		}

		newBlock := hc.GetHeader(newHeader.Parent(), newHeader.Number64()-1)
		if newBlock == nil {
			return fmt.Errorf("invalid new chain")
		}

	}
	err := hc.trim(commonHeader, startHeader)

	return err
}

// NOTES: Headerchain needs to have head
// Singleton Tds need to get calculated by slice after successful append and then written into headerchain
// Slice uses HLCR to query Headerchains for Tds
// Slice is a collection of references headerchains

// GetBlockNumber retrieves the block number belonging to the given hash
// from the cache or database
func (hc *HeaderChain) GetBlockNumber(hash common.Hash) *uint64 {
	if cached, ok := hc.numberCache.Get(hash); ok {
		number := cached.(uint64)
		return &number
	}
	number := rawdb.ReadHeaderNumber(hc.headerDb, hash)
	if number != nil {
		hc.numberCache.Add(hash, *number)
	}
	return number
}

// GetBlockHashesFromHash retrieves a number of block hashes starting at a given
// hash, fetching towards the genesis block.
func (hc *HeaderChain) GetBlockHashesFromHash(hash common.Hash, max uint64) []common.Hash {
	// Get the origin header from which to fetch
	header := hc.GetHeaderByHash(hash)
	if header == nil {
		return nil
	}
	// Iterate the headers until enough is collected or the genesis reached
	chain := make([]common.Hash, 0, max)
	for i := uint64(0); i < max; i++ {
		next := header.ParentHash[types.QuaiNetworkContext]
		if header = hc.GetHeader(next, header.Number[types.QuaiNetworkContext].Uint64()-1); header == nil {
			break
		}
		chain = append(chain, next)
		if header.Number[types.QuaiNetworkContext].Sign() == 0 {
			break
		}
	}
	return chain
}

// GetAncestor retrieves the Nth ancestor of a given block. It assumes that either the given block or
// a close ancestor of it is canonical. maxNonCanonical points to a downwards counter limiting the
// number of blocks to be individually checked before we reach the canonical chain.
//
// Note: ancestor == 0 returns the same block, 1 returns its parent and so on.
func (hc *HeaderChain) GetAncestor(hash common.Hash, number, ancestor uint64, maxNonCanonical *uint64) (common.Hash, uint64) {
	if ancestor > number {
		return common.Hash{}, 0
	}
	if ancestor == 1 {
		// in this case it is cheaper to just read the header
		if header := hc.GetHeader(hash, number); header != nil {
			return header.ParentHash[types.QuaiNetworkContext], number - 1
		}
		return common.Hash{}, 0
	}
	for ancestor != 0 {
		if rawdb.ReadCanonicalHash(hc.chainDb, number) == hash {
			ancestorHash := rawdb.ReadCanonicalHash(hc.chainDb, number-ancestor)
			if rawdb.ReadCanonicalHash(hc.chainDb, number) == hash {
				number -= ancestor
				return ancestorHash, number
			}
		}
		if *maxNonCanonical == 0 {
			return common.Hash{}, 0
		}
		*maxNonCanonical--
		ancestor--
		header := hc.GetHeader(hash, number)
		if header == nil {
			return common.Hash{}, 0
		}
		hash = header.ParentHash[types.QuaiNetworkContext]
		number--
	}
	return hash, number
}

// GetAncestorByLocation retrieves the first occurrence of a block with a given location from a given block.
//
// Note: location == hash location returns the same block.
func (hc *HeaderChain) GetAncestorByLocation(hash common.Hash, location []byte) (*types.Header, error) {
	header := hc.GetHeaderByHash(hash)
	if header != nil {
		return nil, errors.New("error finding header by hash")
	}

	for !bytes.Equal(header.Location, location) {
		hash = header.ParentHash[types.QuaiNetworkContext]

		header := hc.GetHeaderByHash(hash)
		if header != nil {
			return nil, errors.New("error finding header by hash")
		}
	}
	return header, nil
}

// GetTd retrieves a block's total difficulty in the canonical chain from the
// database by hash and number, caching it if found.
func (hc *HeaderChain) GetTd(hash common.Hash, number uint64) []*big.Int {
	// Short circuit if the td's already in the cache, retrieve otherwise
	if cached, ok := hc.tdCache.Get(hash); ok {
		return cached.([]*big.Int)
	}
	td := rawdb.ReadTd(hc.chainDb, hash, number)
	if td == nil {
		return make([]*big.Int, 3)
	}
	// Cache the found body for next time and return
	hc.tdCache.Add(hash, td)
	return td
}

// GetTdByHash retrieves a block's total difficulty in the canonical chain from the
// database by hash, caching it if found.
func (hc *HeaderChain) GetTdByHash(hash common.Hash) []*big.Int {
	number := hc.GetBlockNumber(hash)
	if number == nil {
		return make([]*big.Int, 3)
	}
	return hc.GetTd(hash, *number)
}

// GetHeader retrieves a block header from the database by hash and number,
// caching it if found.
func (hc *HeaderChain) GetHeader(hash common.Hash, number uint64) *types.Header {
	// Short circuit if the header's already in the cache, retrieve otherwise
	if header, ok := hc.headerCache.Get(hash); ok {
		return header.(*types.Header)
	}
	header := rawdb.ReadHeader(hc.chainDb, hash, number)
	if header == nil {
		return nil
	}
	// Cache the found header for next time and return
	hc.headerCache.Add(hash, header)
	return header
}

// GetHeaderByHash retrieves a block header from the database by hash, caching it if
// found.
func (hc *HeaderChain) GetHeaderByHash(hash common.Hash) *types.Header {
	number := hc.GetBlockNumber(hash)
	if number == nil {
		return nil
	}
	return hc.GetHeader(hash, *number)
}

// HasHeader checks if a block header is present in the database or not.
// In theory, if header is present in the database, all relative components
// like td and hash->number should be present too.
func (hc *HeaderChain) HasHeader(hash common.Hash, number uint64) bool {
	if hc.numberCache.Contains(hash) || hc.headerCache.Contains(hash) {
		return true
	}
	return rawdb.HasHeader(hc.chainDb, hash, number)
}

// GetHeaderByNumber retrieves a block header from the database by number,
// caching it (associated with its hash) if found.
func (hc *HeaderChain) GetHeaderByNumber(number uint64) *types.Header {
	hash := rawdb.ReadCanonicalHash(hc.chainDb, number)
	if hash == (common.Hash{}) {
		return nil
	}
	return hc.GetHeader(hash, number)
}

func (hc *HeaderChain) GetCanonicalHash(number uint64) common.Hash {
	return rawdb.ReadCanonicalHash(hc.chainDb, number)
}

// CurrentHeader retrieves the current head header of the canonical chain. The
// header is retrieved from the HeaderChain's internal cache.
func (hc *HeaderChain) CurrentHeader() *types.Header {
	return hc.currentHeader.Load().(*types.Header)
}

// SetGenesis sets a new genesis block header for the chain
func (hc *HeaderChain) SetGenesis(head *types.Header) {
	hc.genesisHeader = head
}

// Config retrieves the header chain's chain configuration.
func (hc *HeaderChain) Config() *params.ChainConfig { return hc.config }

// Engine retrieves the header chain's consensus engine.
func (hc *HeaderChain) Engine() consensus.Engine { return hc.engine }

// GetBlock implements consensus.ChainReader, and returns nil for every input as
// a header chain does not have blocks available for retrieval.
func (hc *HeaderChain) GetBlock(hash common.Hash, number uint64) *types.Block {
	return nil
}

// GetGasUsedInChain retrieves all the gas used from a given block backwards until
// a specific distance is reached.
func (hc *HeaderChain) GetGasUsedInChain(block *types.Block, length int) int64 {
	return int64(0)
}

// GetUnclesInChain retrieves all the uncles from a given block backwards until
// a specific distance is reached.
func (hc *HeaderChain) GetUnclesInChain(block *types.Block, length int) []*types.Header {
	return nil
}

// CheckContext checks to make sure the range of a context or order is valid
func (hc *HeaderChain) CheckContext(context int) error {
	if context < 0 || context > len(params.FullerOntology) {
		return errors.New("the provided path is outside the allowable range")
	}
	return nil
}

// CheckLocationRange checks to make sure the range of r and z are valid
func (hc *HeaderChain) CheckLocationRange(location []byte) error {
	if int(location[0]) < 1 || int(location[0]) > params.FullerOntology[0] {
		return errors.New("the provided location is outside the allowable region range")
	}
	if int(location[1]) < 1 || int(location[1]) > params.FullerOntology[1] {
		return errors.New("the provided location is outside the allowable zone range")
	}
	return nil
}

// GasLimit returns the gas limit of the current HEAD block.
func (bc *BlockChain) GasLimit() uint64 {
	return bc.CurrentBlock().GasLimit()
}

// CurrentBlock retrieves the current head block of the canonical chain. The
// block is retrieved from the blockchain's internal cache.
func (bc *BlockChain) CurrentBlock() *types.Block {
	return bc.currentBlock.Load().(*types.Block)
}

// GetUnclesInChain retrieves all the uncles from a given block backwards until
// a specific distance is reached.
func (bc *BlockChain) GetUnclesInChain(block *types.Block, length int) []*types.Header {
	uncles := []*types.Header{}
	for i := 0; block != nil && i < length; i++ {
		uncles = append(uncles, block.Uncles()...)
		block = bc.GetBlock(block.ParentHash(), block.NumberU64()-1)
	}
	return uncles
}

// GetGasUsedInChain retrieves all the gas used from a given block backwards until
// a specific distance is reached.
func (bc *BlockChain) GetGasUsedInChain(block *types.Block, length int) int64 {
	gasUsed := 0
	for i := 0; block != nil && i < length; i++ {
		gasUsed += int(block.GasUsed())
		block = bc.GetBlock(block.ParentHash(), block.NumberU64()-1)
	}
	return int64(gasUsed)
}

// GetGasUsedInChain retrieves all the gas used from a given block backwards until
// a specific distance is reached.
func (bc *BlockChain) CalculateBaseFee(header *types.Header) *big.Int {
	return misc.CalcBaseFee(bc.Config(), header, bc.GetHeaderByNumber, bc.GetUnclesInChain, bc.GetGasUsedInChain)
}

// GetBlockStatus returns the status of the block for a given header
func (bc *BlockChain) GetBlockStatus(header *types.Header) WriteStatus {
	canonHash := bc.GetCanonicalHash(header.Number[types.QuaiNetworkContext].Uint64())
	if (canonHash == common.Hash{}) {
		return UnknownStatTy
	}
	if canonHash != header.Hash() {
		return SideStatTy
	}
	return CanonStatTy
}

// CurrentHeader retrieves the current head header of the canonical chain. The
// header is retrieved from the HeaderChain's internal cache.
func (bc *BlockChain) CurrentHeader() *types.Header {
	return bc.hc.CurrentHeader()
}

// GetTd retrieves a block's total difficulty in the canonical chain from the
// database by hash and number, caching it if found.
func (bc *BlockChain) GetTd(hash common.Hash, number uint64) []*big.Int {
	return bc.hc.GetTd(hash, number)
}

// GetTdByHash retrieves a block's total difficulty in the canonical chain from the
// database by hash, caching it if found.
func (bc *BlockChain) GetTdByHash(hash common.Hash) []*big.Int {
	return bc.hc.GetTdByHash(hash)
}

// GetHeader retrieves a block header from the database by hash and number,
// caching it if found.
func (bc *BlockChain) GetHeader(hash common.Hash, number uint64) *types.Header {
	// Blockchain might have cached the whole block, only if not go to headerchain
	if block, ok := bc.blockCache.Get(hash); ok {
		return block.(*types.Block).Header()
	}

	return bc.hc.GetHeader(hash, number)
}

// GetHeaderByHash retrieves a block header from the database by hash, caching it if
// found.
func (bc *BlockChain) GetHeaderByHash(hash common.Hash) *types.Header {
	// Blockchain might have cached the whole block, only if not go to headerchain
	if block, ok := bc.blockCache.Get(hash); ok {
		return block.(*types.Block).Header()
	}

	return bc.hc.GetHeaderByHash(hash)
}
