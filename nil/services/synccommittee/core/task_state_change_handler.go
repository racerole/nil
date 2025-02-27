package core

import (
	"context"
	"fmt"

	"github.com/NilFoundation/nil/nil/common/logging"
	"github.com/NilFoundation/nil/nil/services/synccommittee/internal/api"
	"github.com/NilFoundation/nil/nil/services/synccommittee/internal/log"
	"github.com/NilFoundation/nil/nil/services/synccommittee/internal/types"
	"github.com/rs/zerolog"
)

type ProvedBlockSetter interface {
	SetBlockAsProved(ctx context.Context, blockId types.BlockId) error
}

type taskStateChangeHandler struct {
	blockSetter ProvedBlockSetter
	logger      zerolog.Logger
}

func newTaskStateChangeHandler(
	blockSetter ProvedBlockSetter,
	logger zerolog.Logger,
) api.TaskStateChangeHandler {
	return &taskStateChangeHandler{
		blockSetter: blockSetter,
		logger:      logger,
	}
}

func (h taskStateChangeHandler) OnTaskTerminated(ctx context.Context, task *types.Task, result *types.TaskResult) error {
	if task.TaskType != types.AggregateProofs {
		log.NewTaskEvent(h.logger, zerolog.DebugLevel, task).
			Msgf("task has type %s, just update pending dependency", task.TaskType)
		return nil
	}

	if !result.IsSuccess() {
		// TODO: handle critical errors here

		log.NewTaskResultEvent(h.logger, zerolog.WarnLevel, result).
			Msg("block proof task has failed, data won't be sent to the L1")
		return nil
	}

	log.NewTaskResultEvent(h.logger, zerolog.InfoLevel, result).
		Stringer(logging.FieldBatchId, task.BatchId).
		Msg("Proof batch completed")

	blockId := types.NewBlockId(task.ShardId, task.BlockHash)

	if err := h.blockSetter.SetBlockAsProved(ctx, blockId); err != nil {
		return fmt.Errorf("failed to set block with id=%s as proved: %w", blockId, err)
	}

	return nil
}
