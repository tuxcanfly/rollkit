package block

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	goheaderstore "github.com/celestiaorg/go-header/store"
	abci "github.com/cometbft/cometbft/abci/types"
	cmcrypto "github.com/cometbft/cometbft/crypto"
	"github.com/cometbft/cometbft/crypto/merkle"
	"github.com/cometbft/cometbft/proxy"
	cmtypes "github.com/cometbft/cometbft/types"
	ds "github.com/ipfs/go-datastore"
	"github.com/libp2p/go-libp2p/core/crypto"
	pkgErrors "github.com/pkg/errors"

	"github.com/rollkit/rollkit/config"
	"github.com/rollkit/rollkit/da"
	"github.com/rollkit/rollkit/mempool"
	"github.com/rollkit/rollkit/state"
	"github.com/rollkit/rollkit/store"
	"github.com/rollkit/rollkit/third_party/log"
	"github.com/rollkit/rollkit/types"
)

// defaultDABlockTime is used only if DABlockTime is not configured for manager
const defaultDABlockTime = 15 * time.Second

// defaultBlockTime is used only if BlockTime is not configured for manager
const defaultBlockTime = 1 * time.Second

// defaultMempoolTTL is the number of blocks until transaction is dropped from mempool
const defaultMempoolTTL = 25

// maxSubmitAttempts defines how many times Rollkit will re-try to publish block to DA layer.
// This is temporary solution. It will be removed in future versions.
const maxSubmitAttempts = 30

// Applies to most channels, 100 is a large enough buffer to avoid blocking
const channelLength = 100

// Applies to the blockInCh, 10000 is a large enough number for blocks per DA block.
const blockInChLength = 10000

// initialBackoff defines initial value for block submission backoff
var initialBackoff = 100 * time.Millisecond

// NewBlockEvent is used to pass block and DA height to blockInCh
type NewBlockEvent struct {
	Block    *types.Block
	DAHeight uint64
}

// Manager is responsible for aggregating transactions into blocks.
type Manager struct {
	lastState types.State
	// lastStateMtx is used by lastState
	lastStateMtx *sync.RWMutex
	store        store.Store

	conf    config.BlockManagerConfig
	genesis *cmtypes.GenesisDoc

	proposerKey crypto.PrivKey

	executor *state.BlockExecutor

	dalc *da.DAClient
	// daHeight is the height of the latest processed DA block
	daHeight uint64

	HeaderCh chan *types.SignedHeader
	BlockCh  chan *types.Block

	blockInCh  chan NewBlockEvent
	blockStore *goheaderstore.Store[*types.Block]

	blockCache *BlockCache

	// blockStoreCh is used to notify sync goroutine (SyncLoop) that it needs to retrieve blocks from blockStore
	blockStoreCh chan struct{}

	// retrieveCond is used to notify sync goroutine (SyncLoop) that it needs to retrieve data
	retrieveCh chan struct{}

	logger log.Logger

	// Rollkit doesn't have "validators", but
	// we store the sequencer in this struct for compatibility.
	validatorSet *cmtypes.ValidatorSet

	// For usage by Lazy Aggregator mode
	buildingBlock bool
	txsAvailable  <-chan struct{}

	pendingBlocks *PendingBlocks

	// for reporting metrics
	metrics *Metrics
}

// getInitialState tries to load lastState from Store, and if it's not available it reads GenesisDoc.
func getInitialState(store store.Store, genesis *cmtypes.GenesisDoc) (types.State, error) {
	// Load the state from store.
	s, err := store.GetState(context.Background())
	if errors.Is(err, ds.ErrNotFound) {
		// If the user is starting a fresh chain (or hard-forking), we assume the stored state is empty.
		s, err = types.NewFromGenesisDoc(genesis)
		if err != nil {
			return types.State{}, err
		}
		store.SetHeight(context.Background(), s.LastBlockHeight)
	} else if err != nil {
		return types.State{}, err
	} else {
		// Perform a sanity-check to stop the user from
		// using a higher genesis than the last stored state.
		// if they meant to hard-fork, they should have cleared the stored State
		if uint64(genesis.InitialHeight) > s.LastBlockHeight {
			return types.State{}, fmt.Errorf("genesis.InitialHeight (%d) is greater than last stored state's LastBlockHeight (%d)", genesis.InitialHeight, s.LastBlockHeight)
		}
	}

	return s, nil
}

// NewManager creates new block Manager.
func NewManager(
	proposerKey crypto.PrivKey,
	conf config.BlockManagerConfig,
	genesis *cmtypes.GenesisDoc,
	store store.Store,
	mempool mempool.Mempool,
	proxyApp proxy.AppConnConsensus,
	dalc *da.DAClient,
	eventBus *cmtypes.EventBus,
	logger log.Logger,
	blockStore *goheaderstore.Store[*types.Block],
	seqMetrics *Metrics,
	execMetrics *state.Metrics,
) (*Manager, error) {
	s, err := getInitialState(store, genesis)
	if err != nil {
		return nil, err
	}
	// genesis should have exactly one "validator", the centralized sequencer.
	// this should have been validated in the above call to getInitialState.
	valSet := types.GetValidatorSetFromGenesis(genesis)

	if s.DAHeight < conf.DAStartHeight {
		s.DAHeight = conf.DAStartHeight
	}

	if conf.DABlockTime == 0 {
		logger.Info("Using default DA block time", "DABlockTime", defaultDABlockTime)
		conf.DABlockTime = defaultDABlockTime
	}

	if conf.BlockTime == 0 {
		logger.Info("Using default block time", "BlockTime", defaultBlockTime)
		conf.BlockTime = defaultBlockTime
	}

	if conf.DAMempoolTTL == 0 {
		logger.Info("Using default mempool ttl", "MempoolTTL", defaultMempoolTTL)
		conf.DAMempoolTTL = defaultMempoolTTL
	}

	proposerAddress, err := getAddress(proposerKey)
	if err != nil {
		return nil, err
	}

	exec := state.NewBlockExecutor(proposerAddress, genesis.ChainID, mempool, proxyApp, eventBus, logger, execMetrics, valSet.Hash())
	if s.LastBlockHeight+1 == uint64(genesis.InitialHeight) {
		res, err := exec.InitChain(genesis)
		if err != nil {
			return nil, err
		}

		updateState(&s, res)
		if err := store.UpdateState(context.Background(), s); err != nil {
			return nil, err
		}
	}

	var txsAvailableCh <-chan struct{}
	if mempool != nil {
		txsAvailableCh = mempool.TxsAvailable()
	} else {
		txsAvailableCh = nil
	}

	agg := &Manager{
		proposerKey: proposerKey,
		conf:        conf,
		genesis:     genesis,
		lastState:   s,
		store:       store,
		executor:    exec,
		dalc:        dalc,
		daHeight:    s.DAHeight,
		// channels are buffered to avoid blocking on input/output operations, buffer sizes are arbitrary
		HeaderCh:      make(chan *types.SignedHeader, channelLength),
		BlockCh:       make(chan *types.Block, channelLength),
		blockInCh:     make(chan NewBlockEvent, blockInChLength),
		blockStoreCh:  make(chan struct{}, 1),
		blockStore:    blockStore,
		lastStateMtx:  new(sync.RWMutex),
		blockCache:    NewBlockCache(),
		retrieveCh:    make(chan struct{}, 1),
		logger:        logger,
		validatorSet:  &valSet,
		txsAvailable:  txsAvailableCh,
		buildingBlock: false,
		pendingBlocks: NewPendingBlocks(),
		metrics:       seqMetrics,
	}
	return agg, nil
}

func getAddress(key crypto.PrivKey) ([]byte, error) {
	rawKey, err := key.GetPublic().Raw()
	if err != nil {
		return nil, err
	}
	return cmcrypto.AddressHash(rawKey), nil
}

// SetDALC is used to set DataAvailabilityLayerClient used by Manager.
func (m *Manager) SetDALC(dalc *da.DAClient) {
	m.dalc = dalc
}

// SetLastState is used to set lastState used by Manager.
func (m *Manager) SetLastState(state types.State) {
	m.lastStateMtx.Lock()
	defer m.lastStateMtx.Unlock()
	m.lastState = state
}

// GetStoreHeight returns the manager's store height
func (m *Manager) GetStoreHeight() uint64 {
	return m.store.Height()
}

// GetBlockInCh returns the manager's blockInCh
func (m *Manager) GetBlockInCh() chan NewBlockEvent {
	return m.blockInCh
}

// IsBlockHashSeen returns true if the block with the given hash has been seen.
func (m *Manager) IsBlockHashSeen(blockHash string) bool {
	return m.blockCache.isSeen(blockHash)
}

// IsDAIncluded returns true if the block with the given hash has been seen on DA.
func (m *Manager) IsDAIncluded(hash types.Hash) bool {
	return m.blockCache.isDAIncluded(hash.String())
}

// AggregationLoop is responsible for aggregating transactions into rollup-blocks.
func (m *Manager) AggregationLoop(ctx context.Context, lazy bool) {
	initialHeight := uint64(m.genesis.InitialHeight)
	height := m.store.Height()
	var delay time.Duration

	// TODO(tzdybal): double-check when https://github.com/celestiaorg/rollmint/issues/699 is resolved
	if height < initialHeight {
		delay = time.Until(m.genesis.GenesisTime)
	} else {
		lastBlockTime := m.getLastBlockTime()
		delay = time.Until(lastBlockTime.Add(m.conf.BlockTime))
	}

	if delay > 0 {
		m.logger.Info("Waiting to produce block", "delay", delay)
		time.Sleep(delay)
	}

	timer := time.NewTimer(0)
	defer timer.Stop()

	if !lazy {
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			start := time.Now()
			err := m.publishBlock(ctx)
			if err != nil && ctx.Err() == nil {
				m.logger.Error("error while publishing block", "error", err)
			}
			timer.Reset(m.getRemainingSleep(start))
		}
	} else {
		for {
			select {
			case <-ctx.Done():
				return
			// the buildBlock channel is signalled when Txns become available
			// in the mempool, or after transactions remain in the mempool after
			// building a block.
			case _, ok := <-m.txsAvailable:
				if ok && !m.buildingBlock {
					m.buildingBlock = true
					timer.Reset(1 * time.Second)
				}
			case <-timer.C:
				// build a block with all the transactions received in the last 1 second
				err := m.publishBlock(ctx)
				if err != nil && ctx.Err() == nil {
					m.logger.Error("error while publishing block", "error", err)
				}
				m.buildingBlock = false
			}
		}
	}
}

// BlockSubmissionLoop is responsible for submitting blocks to the DA layer.
func (m *Manager) BlockSubmissionLoop(ctx context.Context) {
	timer := time.NewTicker(m.conf.DABlockTime)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if m.pendingBlocks.isEmpty() {
			continue
		}
		err := m.submitBlocksToDA(ctx)
		if err != nil {
			m.logger.Error("error while submitting block to DA", "error", err)
		}
	}
}

// SyncLoop is responsible for syncing blocks.
//
// SyncLoop processes headers gossiped in P2P network to know what's the latest block height,
// block data is retrieved from DA layer.
func (m *Manager) SyncLoop(ctx context.Context, cancel context.CancelFunc) {
	daTicker := time.NewTicker(m.conf.DABlockTime)
	defer daTicker.Stop()
	blockTicker := time.NewTicker(m.conf.BlockTime)
	defer blockTicker.Stop()
	for {
		select {
		case <-daTicker.C:
			m.sendNonBlockingSignalToRetrieveCh()
		case <-blockTicker.C:
			m.sendNonBlockingSignalToBlockStoreCh()
		case blockEvent := <-m.blockInCh:
			// Only validated blocks are sent to blockInCh, so we can safely assume that blockEvent.block is valid
			block := blockEvent.Block
			daHeight := blockEvent.DAHeight
			blockHash := block.Hash().String()
			blockHeight := block.Height()
			m.logger.Debug("block body retrieved",
				"height", blockHeight,
				"daHeight", daHeight,
				"hash", blockHash,
			)
			if blockHeight <= m.store.Height() || m.blockCache.isSeen(blockHash) {
				m.logger.Debug("block already seen", "height", blockHeight, "block hash", blockHash)
				continue
			}
			m.blockCache.setBlock(blockHeight, block)

			m.sendNonBlockingSignalToBlockStoreCh()
			m.sendNonBlockingSignalToRetrieveCh()

			err := m.trySyncNextBlock(ctx, daHeight)
			if err != nil {
				m.logger.Info("failed to sync next block", "error", err)
				continue
			}
			m.blockCache.setSeen(blockHash)
		case <-ctx.Done():
			return
		}
	}
}

func (m *Manager) sendNonBlockingSignalToBlockStoreCh() {
	select {
	case m.blockStoreCh <- struct{}{}:
	default:
	}
}

func (m *Manager) sendNonBlockingSignalToRetrieveCh() {
	select {
	case m.retrieveCh <- struct{}{}:
	default:
	}
}

// trySyncNextBlock tries to execute as many blocks as possible from the blockCache.
//
//	Note: the blockCache contains only valid blocks that are not yet synced
//
// For every block, to be able to apply block at height h, we need to have its Commit. It is contained in block at height h+1.
// If commit for block h+1 is available, we proceed with sync process, and remove synced block from sync cache.
func (m *Manager) trySyncNextBlock(ctx context.Context, daHeight uint64) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		currentHeight := m.store.Height()
		b, ok := m.blockCache.getBlock(currentHeight + 1)
		if !ok {
			m.logger.Debug("block not found in cache", "height", currentHeight+1)
			return nil
		}

		bHeight := b.Height()
		m.logger.Info("Syncing block", "height", bHeight)
		// Validate the received block before applying
		if err := m.executor.Validate(m.lastState, b); err != nil {
			return fmt.Errorf("failed to validate block: %w", err)
		}
		newState, responses, err := m.applyBlock(ctx, b)
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			// if call to applyBlock fails, we halt the node, see https://github.com/cometbft/cometbft/pull/496
			panic(fmt.Errorf("failed to ApplyBlock: %w", err))
		}
		err = m.store.SaveBlock(ctx, b, &b.SignedHeader.Commit)
		if err != nil {
			return fmt.Errorf("failed to save block: %w", err)
		}
		_, _, err = m.executor.Commit(ctx, newState, b, responses)
		if err != nil {
			return fmt.Errorf("failed to Commit: %w", err)
		}

		err = m.store.SaveBlockResponses(ctx, bHeight, responses)
		if err != nil {
			return fmt.Errorf("failed to save block responses: %w", err)
		}

		// Height gets updated
		m.store.SetHeight(ctx, bHeight)

		if daHeight > newState.DAHeight {
			newState.DAHeight = daHeight
		}
		err = m.updateState(ctx, newState)
		if err != nil {
			m.logger.Error("failed to save updated state", "error", err)
		}
		m.blockCache.deleteBlock(currentHeight + 1)
	}
}

// BlockStoreRetrieveLoop is responsible for retrieving blocks from the Block Store.
func (m *Manager) BlockStoreRetrieveLoop(ctx context.Context) {
	lastBlockStoreHeight := uint64(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.blockStoreCh:
		}
		blockStoreHeight := m.blockStore.Height()
		if blockStoreHeight > lastBlockStoreHeight {
			blocks, err := m.getBlocksFromBlockStore(ctx, lastBlockStoreHeight+1, blockStoreHeight)
			if err != nil {
				m.logger.Error("failed to get blocks from Block Store", "lastBlockHeight", lastBlockStoreHeight, "blockStoreHeight", blockStoreHeight, "errors", err.Error())
				continue
			}
			daHeight := atomic.LoadUint64(&m.daHeight)
			for _, block := range blocks {
				// Check for shut down event prior to logging
				// and sending block to blockInCh. The reason
				// for checking for the shutdown event
				// separately is due to the inconsistent nature
				// of the select statement when multiple cases
				// are satisfied.
				select {
				case <-ctx.Done():
					return
				default:
				}
				// early validation to reject junk blocks
				if !m.isUsingExpectedCentralizedSequencer(block) {
					continue
				}
				m.logger.Debug("block retrieved from p2p block sync", "blockHeight", block.Height(), "daHeight", daHeight)
				m.blockInCh <- NewBlockEvent{block, daHeight}
			}
		}
		lastBlockStoreHeight = blockStoreHeight
	}
}

func (m *Manager) getBlocksFromBlockStore(ctx context.Context, startHeight, endHeight uint64) ([]*types.Block, error) {
	if startHeight > endHeight {
		return nil, fmt.Errorf("startHeight (%d) is greater than endHeight (%d)", startHeight, endHeight)
	}
	if startHeight == 0 {
		startHeight++
	}
	blocks := make([]*types.Block, endHeight-startHeight+1)
	for i := startHeight; i <= endHeight; i++ {
		block, err := m.blockStore.GetByHeight(ctx, i)
		if err != nil {
			return nil, err
		}
		blocks[i-startHeight] = block
	}
	return blocks, nil
}

// RetrieveLoop is responsible for interacting with DA layer.
func (m *Manager) RetrieveLoop(ctx context.Context) {
	// blockFoundCh is used to track when we successfully found a block so
	// that we can continue to try and find blocks that are in the next DA height.
	// This enables syncing faster than the DA block time.
	blockFoundCh := make(chan struct{}, 1)
	defer close(blockFoundCh)
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.retrieveCh:
		case <-blockFoundCh:
		}
		daHeight := atomic.LoadUint64(&m.daHeight)
		err := m.processNextDABlock(ctx)
		if err != nil && ctx.Err() == nil {
			m.logger.Error("failed to retrieve block from DALC", "daHeight", daHeight, "errors", err.Error())
			continue
		}
		// Signal the blockFoundCh to try and retrieve the next block
		select {
		case blockFoundCh <- struct{}{}:
		default:
		}
		atomic.AddUint64(&m.daHeight, 1)
	}
}

func (m *Manager) processNextDABlock(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// TODO(tzdybal): extract configuration option
	maxRetries := 10
	daHeight := atomic.LoadUint64(&m.daHeight)

	var err error
	m.logger.Debug("trying to retrieve block from DA", "daHeight", daHeight)
	for r := 0; r < maxRetries; r++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		blockResp, fetchErr := m.fetchBlock(ctx, daHeight)
		if fetchErr == nil {
			if blockResp.Code == da.StatusNotFound {
				m.logger.Debug("no block found", "daHeight", daHeight, "reason", blockResp.Message)
				return nil
			}
			m.logger.Debug("retrieved potential blocks", "n", len(blockResp.Blocks), "daHeight", daHeight)
			for _, block := range blockResp.Blocks {
				// early validation to reject junk blocks
				if !m.isUsingExpectedCentralizedSequencer(block) {
					continue
				}
				blockHash := block.Hash().String()
				m.blockCache.setDAIncluded(blockHash)
				m.logger.Info("block marked as DA included", "blockHeight", block.Height(), "blockHash", blockHash)
				if !m.blockCache.isSeen(blockHash) {
					// Check for shut down event prior to logging
					// and sending block to blockInCh. The reason
					// for checking for the shutdown event
					// separately is due to the inconsistent nature
					// of the select statement when multiple cases
					// are satisfied.
					select {
					case <-ctx.Done():
						return pkgErrors.WithMessage(ctx.Err(), "unable to send block to blockInCh, context done")
					default:
					}
					m.blockInCh <- NewBlockEvent{block, daHeight}
				}
			}
			return nil
		}

		// Track the error
		err = errors.Join(err, fetchErr)
		// Delay before retrying
		select {
		case <-ctx.Done():
			return err
		case <-time.After(100 * time.Millisecond):
		}
	}
	return err
}

func (m *Manager) isUsingExpectedCentralizedSequencer(block *types.Block) bool {
	return bytes.Equal(block.SignedHeader.ProposerAddress, m.genesis.Validators[0].Address.Bytes()) && block.ValidateBasic() == nil
}

func (m *Manager) fetchBlock(ctx context.Context, daHeight uint64) (da.ResultRetrieveBlocks, error) {
	var err error
	blockRes := m.dalc.RetrieveBlocks(ctx, daHeight)
	if blockRes.Code == da.StatusError {
		err = fmt.Errorf("failed to retrieve block: %s", blockRes.Message)
	}
	return blockRes, err
}

func (m *Manager) getRemainingSleep(start time.Time) time.Duration {
	publishingDuration := time.Since(start)
	sleepDuration := m.conf.BlockTime - publishingDuration
	if sleepDuration < 0 {
		sleepDuration = 0
	}
	return sleepDuration
}

func (m *Manager) getCommit(header types.Header) (*types.Commit, error) {
	// note: for compatibility with tendermint light client
	consensusVote := header.MakeCometBFTVote()

	sign, err := m.proposerKey.Sign(consensusVote)
	if err != nil {
		return nil, err
	}
	return &types.Commit{
		Signatures: []types.Signature{sign},
	}, nil
}

// IsProposer returns whether or not the manager is a proposer
func (m *Manager) IsProposer() (bool, error) {
	signerPubBytes, err := m.proposerKey.GetPublic().Raw()
	if err != nil {
		return false, err
	}

	return bytes.Equal(m.genesis.Validators[0].PubKey.Bytes(), signerPubBytes), nil
}

func (m *Manager) publishBlock(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	isProposer, err := m.IsProposer()
	if err != nil {
		return fmt.Errorf("error while checking for proposer: %w", err)
	}
	if !isProposer {
		return nil
	}

	var lastCommit *types.Commit
	var lastHeaderHash types.Hash
	height := m.store.Height()
	newHeight := height + 1

	// this is a special case, when first block is produced - there is no previous commit
	if newHeight == uint64(m.genesis.InitialHeight) {
		lastCommit = &types.Commit{}
	} else {
		lastCommit, err = m.store.GetCommit(ctx, height)
		if err != nil {
			return fmt.Errorf("error while loading last commit: %w", err)
		}
		lastBlock, err := m.store.GetBlock(ctx, height)
		if err != nil {
			return fmt.Errorf("error while loading last block: %w", err)
		}
		lastHeaderHash = lastBlock.Hash()
	}

	var block *types.Block
	var commit *types.Commit

	// Check if there's an already stored block at a newer height
	// If there is use that instead of creating a new block
	pendingBlock, err := m.store.GetBlock(ctx, newHeight)
	if err == nil {
		m.logger.Info("Using pending block", "height", newHeight)
		block = pendingBlock
	} else {
		m.logger.Info("Creating and publishing block", "height", newHeight)
		block, err = m.createBlock(newHeight, lastCommit, lastHeaderHash)
		if err != nil {
			return nil
		}
		m.logger.Debug("block info", "num_tx", len(block.Data.Txs))

		block.SignedHeader.Validators = m.validatorSet
		block.SignedHeader.ValidatorHash = m.validatorSet.Hash()

		/*
		   here we set the SignedHeader.DataHash, and SignedHeader.Commit as a hack
		   to make the block pass ValidateBasic() when it gets called by applyBlock on line 681
		   these values get overridden on lines 687-698 after we obtain the IntermediateStateRoots.
		*/
		block.SignedHeader.DataHash, err = block.Data.Hash()
		if err != nil {
			return nil
		}

		commit, err = m.getCommit(block.SignedHeader.Header)
		if err != nil {
			return err
		}

		// set the commit to current block's signed header
		block.SignedHeader.Commit = *commit
		err = m.store.SaveBlock(ctx, block, commit)
		if err != nil {
			return err
		}
	}

	newState, responses, err := m.applyBlock(ctx, block)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		// if call to applyBlock fails, we halt the node, see https://github.com/cometbft/cometbft/pull/496
		panic(err)
	}

	// Before taking the hash, we need updated ISRs, hence after ApplyBlock
	block.SignedHeader.Header.DataHash, err = block.Data.Hash()
	if err != nil {
		return err
	}

	commit, err = m.getCommit(block.SignedHeader.Header)
	if err != nil {
		return err
	}

	// set the commit to current block's signed header
	block.SignedHeader.Commit = *commit
	// Validate the created block before storing
	if err := m.executor.Validate(m.lastState, block); err != nil {
		return fmt.Errorf("failed to validate block: %w", err)
	}

	blockHeight := block.Height()
	// Update the stored height before submitting to the DA layer and committing to the DB
	m.store.SetHeight(ctx, blockHeight)

	blockHash := block.Hash().String()
	m.blockCache.setSeen(blockHash)

	// SaveBlock commits the DB tx
	err = m.store.SaveBlock(ctx, block, commit)
	if err != nil {
		return err
	}

	// Submit block to be published to the DA layer
	m.pendingBlocks.addPendingBlock(block)

	// Commit the new state and block which writes to disk on the proxy app
	appHash, _, err := m.executor.Commit(ctx, newState, block, responses)
	if err != nil {
		return err
	}
	// Update app hash in state
	newState.AppHash = appHash

	// SaveBlockResponses commits the DB tx
	err = m.store.SaveBlockResponses(ctx, blockHeight, responses)
	if err != nil {
		return err
	}

	newState.DAHeight = atomic.LoadUint64(&m.daHeight)
	// After this call m.lastState is the NEW state returned from ApplyBlock
	// updateState also commits the DB tx
	err = m.updateState(ctx, newState)
	if err != nil {
		return err
	}
	m.recordMetrics(block)
	// Check for shut down event prior to sending the header and block to
	// their respective channels. The reason for checking for the shutdown
	// event separately is due to the inconsistent nature of the select
	// statement when multiple cases are satisfied.
	select {
	case <-ctx.Done():
		return pkgErrors.WithMessage(ctx.Err(), "unable to send header and block, context done")
	default:
	}

	// Publish header to channel so that header exchange service can broadcast
	m.HeaderCh <- &block.SignedHeader

	// Publish block to channel so that block exchange service can broadcast
	m.BlockCh <- block

	m.logger.Debug("successfully proposed block", "proposer", hex.EncodeToString(block.SignedHeader.ProposerAddress), "height", blockHeight)

	return nil
}

func (m *Manager) recordMetrics(block *types.Block) {
	m.metrics.NumTxs.Set(float64(len(block.Data.Txs)))
	m.metrics.TotalTxs.Add(float64(len(block.Data.Txs)))
	m.metrics.BlockSizeBytes.Set(float64(block.Size()))
	m.metrics.CommittedHeight.Set(float64(block.Height()))
}

func (m *Manager) submitBlocksToDA(ctx context.Context) error {
	submittedAllBlocks := false
	backoff := initialBackoff
	blocksToSubmit := m.pendingBlocks.getPendingBlocks()
	numTotalBlocks := len(blocksToSubmit)
	numSubmittedBlocks := 0
	attempt := 0
	maxBlobSize, err := m.dalc.DA.MaxBlobSize(ctx)
	if err != nil {
		return err
	}
	initialMaxBlobSize := maxBlobSize
	initialGasPrice := m.dalc.GasPrice
	gasPrice := m.dalc.GasPrice
	for ctx.Err() == nil && !submittedAllBlocks && attempt < maxSubmitAttempts {
		res := m.dalc.SubmitBlocks(ctx, blocksToSubmit, maxBlobSize, gasPrice)
		switch res.Code {
		case da.StatusSuccess:
			m.logger.Info("successfully submitted Rollkit blocks to DA layer", "daHeight", res.DAHeight, "count", res.SubmittedCount)
			if res.SubmittedCount == uint64(len(blocksToSubmit)) {
				submittedAllBlocks = true
			}
			submittedBlocks, notSubmittedBlocks := blocksToSubmit[:res.SubmittedCount], blocksToSubmit[res.SubmittedCount:]
			numSubmittedBlocks += len(submittedBlocks)
			for _, block := range submittedBlocks {
				m.blockCache.setDAIncluded(block.Hash().String())
			}
			m.pendingBlocks.removeSubmittedBlocks(submittedBlocks)
			blocksToSubmit = notSubmittedBlocks
			// reset submission options when successful
			backoff = initialBackoff
			maxBlobSize = initialMaxBlobSize
			gasPrice = initialGasPrice
			m.logger.Debug("resetting DA layer submission options", "backoff", backoff, "gasPrice", gasPrice, "maxBlobSize", maxBlobSize)
		case da.StatusNotIncludedInBlock, da.StatusAlreadyInMempool:
			m.logger.Error("DA layer submission failed", "error", res.Message, "attempt", attempt)
			backoff = m.conf.DABlockTime * time.Duration(m.conf.DAMempoolTTL)
			if m.dalc.GasMultiplier != -1 && gasPrice != -1 {
				gasPrice = gasPrice * m.dalc.GasMultiplier
			}
			m.logger.Info("retrying DA layer submission with", "backoff minutes: ", backoff.Minutes(), "gasPrice", gasPrice, "maxBlobSize", maxBlobSize)
			time.Sleep(backoff)
			backoff = m.exponentialBackoff(backoff)
		case da.StatusTooBig:
			m.logger.Error("DA layer submission failed", "error", res.Message, "attempt", attempt)
			maxBlobSize = maxBlobSize / 4
			time.Sleep(backoff)
			backoff = m.exponentialBackoff(backoff)
		default:
			m.logger.Error("DA layer submission failed", "error", res.Message, "attempt", attempt)
			time.Sleep(backoff)
			backoff = m.exponentialBackoff(backoff)
		}
		attempt += 1
	}

	if !submittedAllBlocks {
		return fmt.Errorf(
			"failed to submit all blocks to DA layer, submitted %d of %d blocks after %d attempts",
			numSubmittedBlocks,
			numTotalBlocks,
			maxSubmitAttempts,
		)
	}
	return nil
}

func (m *Manager) exponentialBackoff(backoff time.Duration) time.Duration {
	backoff *= 2
	if backoff > m.conf.DABlockTime {
		backoff = m.conf.DABlockTime
	}
	return backoff
}

// Updates the state stored in manager's store along the manager's lastState
func (m *Manager) updateState(ctx context.Context, s types.State) error {
	m.lastStateMtx.Lock()
	defer m.lastStateMtx.Unlock()
	err := m.store.UpdateState(ctx, s)
	if err != nil {
		return err
	}
	m.lastState = s
	m.metrics.Height.Set(float64(s.LastBlockHeight))
	return nil
}

func (m *Manager) getLastBlockTime() time.Time {
	m.lastStateMtx.RLock()
	defer m.lastStateMtx.RUnlock()
	return m.lastState.LastBlockTime
}

func (m *Manager) createBlock(height uint64, lastCommit *types.Commit, lastHeaderHash types.Hash) (*types.Block, error) {
	m.lastStateMtx.RLock()
	defer m.lastStateMtx.RUnlock()
	return m.executor.CreateBlock(height, lastCommit, lastHeaderHash, m.lastState)
}

func (m *Manager) applyBlock(ctx context.Context, block *types.Block) (types.State, *abci.ResponseFinalizeBlock, error) {
	m.lastStateMtx.RLock()
	defer m.lastStateMtx.RUnlock()
	return m.executor.ApplyBlock(ctx, m.lastState, block)
}

func updateState(s *types.State, res *abci.ResponseInitChain) {
	// If the app did not return an app hash, we keep the one set from the genesis doc in
	// the state. We don't set appHash since we don't want the genesis doc app hash
	// recorded in the genesis block. We should probably just remove GenesisDoc.AppHash.
	if len(res.AppHash) > 0 {
		s.AppHash = res.AppHash
	}

	if res.ConsensusParams != nil {
		params := res.ConsensusParams
		if params.Block != nil {
			s.ConsensusParams.Block.MaxBytes = params.Block.MaxBytes
			s.ConsensusParams.Block.MaxGas = params.Block.MaxGas
		}
		if params.Evidence != nil {
			s.ConsensusParams.Evidence.MaxAgeNumBlocks = params.Evidence.MaxAgeNumBlocks
			s.ConsensusParams.Evidence.MaxAgeDuration = params.Evidence.MaxAgeDuration
			s.ConsensusParams.Evidence.MaxBytes = params.Evidence.MaxBytes
		}
		if params.Validator != nil {
			// Copy params.Validator.PubkeyTypes, and set result's value to the copy.
			// This avoids having to initialize the slice to 0 values, and then write to it again.
			s.ConsensusParams.Validator.PubKeyTypes = append([]string{}, params.Validator.PubKeyTypes...)
		}
		if params.Version != nil {
			s.ConsensusParams.Version.App = params.Version.App
		}
		s.Version.Consensus.App = s.ConsensusParams.Version.App
	}
	// We update the last results hash with the empty hash, to conform with RFC-6962.
	s.LastResultsHash = merkle.HashFromByteSlices(nil)

}
