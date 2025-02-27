package storage

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/NilFoundation/nil/nil/common"
	"github.com/NilFoundation/nil/nil/common/logging"
	"github.com/NilFoundation/nil/nil/internal/db"
	"github.com/NilFoundation/nil/nil/internal/types"
	"github.com/NilFoundation/nil/nil/services/rpc/jsonrpc"
	scTypes "github.com/NilFoundation/nil/nil/services/synccommittee/internal/types"
	"github.com/rs/zerolog"
)

const (
	// blocksTable stores blocks received from the RPC. Key: common.Hash, Value: blockEntry.
	blocksTable db.TableName = "blocks"
	// latestFetchedTable stores reference to the latest main shard block. Key: mainShardKey, Value: sctypes.MainBlockRef.
	latestFetchedTable db.TableName = "latest_fetched"
	// stateRootTable stores the latest ProvedStateRoot (single value). Key: mainShardKey, Value: common.Hash.
	stateRootTable db.TableName = "state_root"
	// nextToProposeTable stores parent's hash of the next block to propose (single value). Key: mainShardKey, Value: common.Hash.
	nextToProposeTable db.TableName = "next_to_propose_parent_hash"
)

var mainShardKey = makeShardKey(types.MainShardId)

type blockEntry struct {
	Block     jsonrpc.RPCBlock `json:"block"`
	IsProved  bool             `json:"isProved"`
	BatchId   scTypes.BatchId  `json:"batchId"`
	FetchedAt time.Time        `json:"fetchedAt"`
}

type BlockStorageMetrics interface {
	RecordMainBlockProved(ctx context.Context)
}

type BlockStorage struct {
	commonStorage
	timer   common.Timer
	metrics BlockStorageMetrics
}

func NewBlockStorage(
	database db.DB,
	timer common.Timer,
	metrics BlockStorageMetrics,
	logger zerolog.Logger,
) *BlockStorage {
	return &BlockStorage{
		commonStorage: makeCommonStorage(database, logger, common.DoNotRetryIf(scTypes.ErrBlockMismatch)),
		timer:         timer,
		metrics:       metrics,
	}
}

func (bs *BlockStorage) TryGetProvedStateRoot(ctx context.Context) (*common.Hash, error) {
	tx, err := bs.database.CreateRoTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	return bs.getProvedStateRoot(tx)
}

func (bs *BlockStorage) getProvedStateRoot(tx db.RoTx) (*common.Hash, error) {
	hashBytes, err := tx.Get(stateRootTable, mainShardKey)
	if errors.Is(err, db.ErrKeyNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	hash := common.BytesToHash(hashBytes)
	return &hash, nil
}

func (bs *BlockStorage) SetProvedStateRoot(ctx context.Context, stateRoot common.Hash) error {
	if stateRoot == common.EmptyHash {
		return errors.New("state root cannot be empty")
	}

	tx, err := bs.database.CreateRwTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	err = tx.Put(stateRootTable, mainShardKey, stateRoot.Bytes())
	if err != nil {
		return err
	}

	return bs.commit(tx)
}

func (bs *BlockStorage) TryGetLatestFetched(ctx context.Context) (*scTypes.MainBlockRef, error) {
	tx, err := bs.database.CreateRoTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	lastFetched, err := bs.getLatestFetchedMainTx(tx)
	if err != nil {
		return nil, err
	}

	return lastFetched, nil
}

func (bs *BlockStorage) TryGetBlock(ctx context.Context, id scTypes.BlockId) (*jsonrpc.RPCBlock, error) {
	tx, err := bs.database.CreateRoTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	entry, err := bs.getBlockEntry(tx, id)
	if err != nil || entry == nil {
		return nil, err
	}
	return &entry.Block, nil
}

func (bs *BlockStorage) SetBlockBatch(ctx context.Context, batch *scTypes.BlockBatch) error {
	if batch == nil {
		return errors.New("batch cannot be nil")
	}

	return bs.retryRunner.Do(ctx, func(ctx context.Context) error {
		return bs.setBlockBatchImpl(ctx, batch)
	})
}

func (bs *BlockStorage) setBlockBatchImpl(ctx context.Context, batch *scTypes.BlockBatch) error {
	tx, err := bs.database.CreateRwTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := bs.putBlockTx(tx, batch.Id, batch.MainShardBlock); err != nil {
		return err
	}

	for _, childBlock := range batch.ChildBlocks {
		if err := bs.putBlockTx(tx, batch.Id, childBlock); err != nil {
			return err
		}
	}

	if err := bs.setProposeParentHash(tx, batch.MainShardBlock); err != nil {
		return err
	}

	if err := bs.updateLatestFetched(tx, batch.MainShardBlock); err != nil {
		return err
	}

	return bs.commit(tx)
}

func (bs *BlockStorage) putBlockTx(tx db.RwTx, batchId scTypes.BatchId, block *jsonrpc.RPCBlock) error {
	currentTime := bs.timer.NowTime()
	entry := blockEntry{Block: *block, BatchId: batchId, FetchedAt: currentTime}
	value, err := marshallEntry(&entry)
	if err != nil {
		return err
	}

	blockId := scTypes.IdFromBlock(block)
	if err := tx.Put(blocksTable, blockId.Bytes(), value); err != nil {
		return fmt.Errorf("failed to put block %s: %w", blockId.String(), err)
	}

	return nil
}

func (bs *BlockStorage) updateLatestFetched(tx db.RwTx, block *jsonrpc.RPCBlock) error {
	if block.ShardId != types.MainShardId {
		return nil
	}

	latestFetched, err := bs.getLatestFetchedMainTx(tx)
	if err != nil {
		return err
	}

	if latestFetched.Equals(block) {
		return nil
	}

	if err := latestFetched.ValidateChild(block); err != nil {
		return fmt.Errorf("unable to update latest fetched block: %w", err)
	}

	newLatestFetched, err := scTypes.NewBlockRef(block)
	if err != nil {
		return err
	}

	return bs.putLatestFetchedBlockTx(tx, block.ShardId, *newLatestFetched)
}

func (bs *BlockStorage) setProposeParentHash(tx db.RwTx, block *jsonrpc.RPCBlock) error {
	if block.ShardId != types.MainShardId {
		return nil
	}
	parentHash, err := bs.getParentOfNextToPropose(tx)
	if err != nil {
		return err
	}
	if parentHash != nil {
		return nil
	}

	if block.Number > 0 && block.ParentHash.Empty() {
		return fmt.Errorf("block with hash=%s has empty parent hash", block.Hash.String())
	}

	bs.logger.Info().
		Stringer(logging.FieldBlockHash, block.Hash).
		Stringer("parentHash", block.ParentHash).
		Msg("block parent hash is not set, updating it")

	return bs.setParentOfNextToPropose(tx, block.ParentHash)
}

func (bs *BlockStorage) SetBlockAsProved(ctx context.Context, id scTypes.BlockId) error {
	wasSet, err := bs.setBlockAsProvedImpl(ctx, id)
	if err != nil {
		return err
	}
	if wasSet {
		bs.metrics.RecordMainBlockProved(ctx)
	}
	return nil
}

func (bs *BlockStorage) setBlockAsProvedImpl(ctx context.Context, id scTypes.BlockId) (wasSet bool, err error) {
	tx, err := bs.database.CreateRwTx(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	entry, err := bs.getBlockEntry(tx, id)
	if err != nil {
		return false, err
	}

	if entry == nil {
		return false, fmt.Errorf("block with id=%s is not found", id)
	}
	if entry.IsProved {
		bs.logger.Debug().Stringer("blockId", id).Msg("block is already marked as proved")
		return false, nil
	}

	entry.IsProved = true
	value, err := marshallEntry(entry)
	if err != nil {
		return false, err
	}

	if err := tx.Put(blocksTable, id.Bytes(), value); err != nil {
		return false, err
	}

	if err := bs.commit(tx); err != nil {
		return false, err
	}

	return true, nil
}

func (bs *BlockStorage) TryGetNextProposalData(ctx context.Context) (*scTypes.ProposalData, error) {
	tx, err := bs.database.CreateRoTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	currentProvedStateRoot, err := bs.getProvedStateRoot(tx)
	if err != nil {
		return nil, err
	}
	if currentProvedStateRoot == nil {
		return nil, errors.New("proved state root was not initialized")
	}

	parentHash, err := bs.getParentOfNextToPropose(tx)
	if err != nil {
		return nil, err
	}

	if parentHash == nil {
		bs.logger.Debug().Msg("block parent hash is not set")
		return nil, nil
	}

	var mainShardEntry *blockEntry
	err = iterateOverEntries(tx, func(entry *blockEntry) (bool, error) {
		if isValidProposalCandidate(entry, *parentHash) {
			mainShardEntry = entry
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	if mainShardEntry == nil {
		bs.logger.Debug().Stringer("parentHash", parentHash).Msg("no proved main shard block found")
		return nil, nil
	}

	transactions := scTypes.BlockTransactions(&mainShardEntry.Block)

	childIds, err := scTypes.ChildBlockIds(&mainShardEntry.Block)
	if err != nil {
		return nil, err
	}

	for _, childId := range childIds {
		childEntry, err := bs.getBlockEntry(tx, childId)
		if err != nil {
			return nil, fmt.Errorf("failed to get child block with id=%s: %w", childId, err)
		}
		if childEntry == nil {
			return nil, fmt.Errorf("child block with id=%s is not found", childId)
		}

		blockTransactions := scTypes.BlockTransactions(&childEntry.Block)
		transactions = append(transactions, blockTransactions...)
	}

	return &scTypes.ProposalData{
		MainShardBlockHash: mainShardEntry.Block.Hash,
		Transactions:       transactions,
		OldProvedStateRoot: *currentProvedStateRoot,
		NewProvedStateRoot: mainShardEntry.Block.ChildBlocksRootHash,
		MainBlockFetchedAt: mainShardEntry.FetchedAt,
	}, nil
}

func (bs *BlockStorage) SetBlockAsProposed(ctx context.Context, id scTypes.BlockId) error {
	return bs.retryRunner.Do(ctx, func(ctx context.Context) error {
		return bs.setBlockAsProposedImpl(ctx, id)
	})
}

func (bs *BlockStorage) setBlockAsProposedImpl(ctx context.Context, id scTypes.BlockId) error {
	tx, err := bs.database.CreateRwTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	mainShardEntry, err := bs.getBlockEntry(tx, id)
	if err != nil {
		return err
	}

	if err := bs.validateMainShardEntry(tx, id, mainShardEntry); err != nil {
		return err
	}

	childIds, err := scTypes.ChildBlockIds(&mainShardEntry.Block)
	if err != nil {
		return err
	}

	for _, childId := range childIds {
		if err := tx.Delete(blocksTable, childId.Bytes()); err != nil {
			return fmt.Errorf("failed to delete child block with id=%s: %w", childId, err)
		}
	}

	mainBlockId := scTypes.IdFromBlock(&mainShardEntry.Block)
	if err := tx.Delete(blocksTable, mainBlockId.Bytes()); err != nil {
		return fmt.Errorf("failed to delete main shard block with id=%s: %w", mainBlockId, err)
	}

	if err := tx.Put(stateRootTable, mainShardKey, mainShardEntry.Block.ChildBlocksRootHash.Bytes()); err != nil {
		return fmt.Errorf("failed to put state root: %w", err)
	}

	if err := bs.setParentOfNextToPropose(tx, mainShardEntry.Block.Hash); err != nil {
		return err
	}

	return bs.commit(tx)
}

func isValidProposalCandidate(entry *blockEntry, parentHash common.Hash) bool {
	return entry.Block.ShardId == types.MainShardId &&
		entry.IsProved &&
		entry.Block.ParentHash == parentHash
}

// getParentOfNextToPropose retrieves parent's hash of the next block to propose
func (bs *BlockStorage) getParentOfNextToPropose(tx db.RoTx) (*common.Hash, error) {
	hashBytes, err := tx.Get(nextToProposeTable, mainShardKey)

	if errors.Is(err, db.ErrKeyNotFound) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("failed to get next to propose parent hash: %w", err)
	}

	hash := common.BytesToHash(hashBytes)
	return &hash, nil
}

// setParentOfNextToPropose sets parent's hash of the next block to propose
func (bs *BlockStorage) setParentOfNextToPropose(tx db.RwTx, hash common.Hash) error {
	err := tx.Put(nextToProposeTable, mainShardKey, hash.Bytes())
	if err != nil {
		return fmt.Errorf("failed to put next to propose parent hash: %w", err)
	}
	return nil
}

func (bs *BlockStorage) validateMainShardEntry(tx db.RoTx, id scTypes.BlockId, entry *blockEntry) error {
	if entry == nil {
		return fmt.Errorf("block with id=%s is not found", id.String())
	}

	if entry.Block.ShardId != types.MainShardId {
		return fmt.Errorf("block with id=%s is not from main shard", id.String())
	}

	if !entry.IsProved {
		return fmt.Errorf("block with id=%s is not proved", id.String())
	}

	parentHash, err := bs.getParentOfNextToPropose(tx)
	if err != nil {
		return err
	}
	if parentHash == nil {
		return errors.New("next to propose parent hash is not set")
	}

	if *parentHash != entry.Block.ParentHash {
		return fmt.Errorf(
			"parent's block hash=%s is not equal to the stored value=%s",
			entry.Block.ParentHash.String(),
			parentHash.String(),
		)
	}
	return nil
}

func (bs *BlockStorage) getLatestFetchedMainTx(tx db.RoTx) (*scTypes.MainBlockRef, error) {
	value, err := tx.Get(latestFetchedTable, mainShardKey)
	if errors.Is(err, db.ErrKeyNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var blockRef *scTypes.MainBlockRef
	err = json.Unmarshal(value, &blockRef)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSerializationFailed, err)
	}
	return blockRef, nil
}

func (bs *BlockStorage) putLatestFetchedBlockTx(tx db.RwTx, shardId types.ShardId, block scTypes.MainBlockRef) error {
	bytes, err := json.Marshal(block)
	if err != nil {
		return fmt.Errorf(
			"%w: failed to encode block ref with hash=%s: %w", ErrSerializationFailed, block.Hash.String(), err,
		)
	}
	err = tx.Put(latestFetchedTable, makeShardKey(shardId), bytes)
	if err != nil {
		return fmt.Errorf("failed to put block ref with hash=%s: %w", block.Hash.String(), err)
	}
	return nil
}

func makeShardKey(shardId types.ShardId) []byte {
	key := make([]byte, 4)
	binary.LittleEndian.PutUint32(key, uint32(shardId))
	return key
}

func (bs *BlockStorage) getBlockEntry(tx db.RoTx, id scTypes.BlockId) (*blockEntry, error) {
	idBytes := id.Bytes()
	value, err := tx.Get(blocksTable, idBytes)
	if err != nil {
		if errors.Is(err, db.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, err
	}

	entry, err := unmarshallEntry(idBytes, value)
	if err != nil {
		return nil, err
	}

	return entry, nil
}

func iterateOverEntries(tx db.RoTx, action func(entry *blockEntry) (shouldContinue bool, err error)) error {
	iter, err := tx.Range(blocksTable, nil, nil)
	if err != nil {
		return err
	}
	defer iter.Close()

	for iter.HasNext() {
		key, val, err := iter.Next()
		if err != nil {
			return err
		}
		entry, err := unmarshallEntry(key, val)
		if err != nil {
			return err
		}
		shouldContinue, err := action(entry)
		if err != nil {
			return err
		}
		if !shouldContinue {
			return nil
		}
	}

	return nil
}

func marshallEntry(entry *blockEntry) ([]byte, error) {
	bytes, err := json.Marshal(entry)
	if err != nil {
		return nil, fmt.Errorf(
			"%w: failed to encode block with hash %s: %w", ErrSerializationFailed, entry.Block.Hash, err,
		)
	}
	return bytes, nil
}

func unmarshallEntry(key []byte, val []byte) (*blockEntry, error) {
	entry := &blockEntry{}
	if err := json.Unmarshal(val, entry); err != nil {
		return nil, fmt.Errorf(
			"%w: failed to unmarshall block entry with id=%s: %w", ErrSerializationFailed, hex.EncodeToString(key), err,
		)
	}

	return entry, nil
}
