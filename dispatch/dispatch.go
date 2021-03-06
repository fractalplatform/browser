package dispatch

import (
	"time"

	"github.com/browser/client"
	"github.com/browser/config"
	"github.com/browser/db"
	. "github.com/browser/log"
	"github.com/browser/rlp"
	"github.com/browser/task"
	"github.com/browser/types"
	"go.uber.org/zap"
)

var (
	syncDuration = 100 * time.Millisecond
)

func NewDispatch() *Dispatch {
	blockDataChan := make(chan *types.BlockAndResult, config.BlockDataChanBufferSize)
	taskStatusMap, startHeight := getTaskStatus()
	taskCount := len(config.Tasks)
	taskDataChan := make([]chan *task.TaskChanData, taskCount)
	taskRollbackDataChan := make([]chan *task.TaskChanData, taskCount)
	taskResultChan := make(chan bool, taskCount)
	for i := 0; i < taskCount; i++ {
		taskDataChan[i] = make(chan *task.TaskChanData, 1)
		taskRollbackDataChan[i] = make(chan *task.TaskChanData, 1)
	}
	isRollbackChan := make(chan bool)
	return &Dispatch{
		blockDataChan:        blockDataChan,
		taskStatusMap:        taskStatusMap,
		startHeight:          startHeight,
		taskDataChan:         taskDataChan,
		taskResultChan:       taskResultChan,
		taskCount:            taskCount,
		taskRollbackDataChan: taskRollbackDataChan,
		isRollbackChan:       isRollbackChan,
	}
}

type Dispatch struct {
	blockDataChan        chan *types.BlockAndResult
	taskStatusMap        map[string]*db.TaskStatus
	startHeight          uint64
	batchTo              uint64
	taskDataChan         []chan *task.TaskChanData
	taskRollbackDataChan []chan *task.TaskChanData
	taskResultChan       chan bool
	taskCount            int
	isRollbackChan       chan bool
	currentBlock         *types.BlockAndResult
}

func (d *Dispatch) Start() {
	//async start task
	d.startTasks()
	//async start send block data task
	d.sendBlockToTask()

	//batch pull block
	d.batchPullIrreversibleBlock()

	//delete roolback backup data
	go deleteRollback()

	//start a single block pull task
	isRollback := false
	startHeight := d.batchTo
	for {
		select {
		case isRollback = <-d.isRollbackChan:
			if !isRollback {
				//clear block
				startHeight = d.startHeight
			} else {
				time.Sleep(time.Duration(1) * time.Second)
			}
		default:
		}
		if !isRollback {
			block, err := client.GetBlockAndResult(int64(startHeight))
			if err == client.ErrNull {
				time.Sleep(syncDuration)
				continue
			}
			if err != nil {
				ZapLog.Error("sync getBlockByNumber", zap.Error(err))
				time.Sleep(syncDuration)
				continue
			}
			if block == nil {
				time.Sleep(syncDuration)
				continue
			}
			d.blockDataChan <- block
			startHeight++
		}
	}
}

func getTaskStatus() (map[string]*db.TaskStatus, uint64) {
	taskStatusMap := db.Mysql.GetTaskStatus(config.Tasks)
	startHeight := uint64(0)
	if len(taskStatusMap) != len(config.Tasks) {
		for _, taskType := range config.Tasks {
			if _, ok := taskStatusMap[taskType]; !ok {
				db.Mysql.InitTaskStatus(taskType)
			}
		}
		taskStatusMap = db.Mysql.GetTaskStatus(config.Tasks)
	}

	startHeightInit := true
	for _, taskStatus := range taskStatusMap {
		if startHeightInit {
			startHeight = taskStatus.Height
			startHeightInit = false
		} else {
			if taskStatus.Height < startHeight {
				startHeight = taskStatus.Height
			}
		}

		if startHeight == 0 {
			break
		}
	}
	return taskStatusMap, startHeight
}

func (d *Dispatch) batchPullIrreversibleBlock() {
	irreversible, err := client.GetDposIrreversible()
	d.batchTo = irreversible.BftIrreversible
	if err != nil {
		ZapLog.Panic("get dpos irreversible error", zap.Error(err))
	}

	if d.startHeight < d.batchTo {
		scanning(int64(d.startHeight), int64(d.batchTo), d.blockDataChan)
	} else {
		d.batchTo = d.startHeight
	}
}

func (d *Dispatch) startTasks() {
	for i, taskType := range config.Tasks {
		if taskFunc, ok := task.TaskFunc[taskType]; ok {
			go taskFunc.Start(d.taskDataChan[i], d.taskRollbackDataChan[i], d.taskResultChan, d.taskStatusMap[taskType].Height)
		} else {
			ZapLog.Panic("task type or func not existing")
		}
	}
}

func (d *Dispatch) sendBlockToTask() {
	go func() {
		for {
			block := <-d.blockDataChan

			if block.Block.Number.Uint64() > d.batchTo { //the inverse calculation block
				if d.currentBlock == nil {
					d.currentBlock = block
				} else {
					if d.currentBlock.Block.Hash.String() != block.Block.ParentHash.String() {
						d.currentBlock = block
						d.rollback()
						ZapLog.Info("rollback success", zap.Uint64("height", block.Block.Number.Uint64()-1))
						continue
					}
					d.currentBlock = block
				}
			}

			taskData := &task.TaskChanData{
				Block: block,
				Tx:    nil,
			}
			d.cacheBlock(taskData) //cache reversible block
			for _, taskDataChan := range d.taskDataChan {
				taskDataChan <- taskData
			}
			d.checkTaskResult()
			if block.Block.Number.Int64()%config.Log.SyncBlockShowNumber == 0 {
				ZapLog.Info("commit success", zap.Uint64("height", block.Block.Number.Uint64()))
			}
		}
	}()
}

func (d *Dispatch) checkTaskResult() {
	returnCount := 0
	for {
		select {
		case <-d.taskResultChan:
			returnCount += 1
		default:

		}
		if returnCount == d.taskCount {
			return
		}
	}
}

func (d *Dispatch) rollback() {
	d.isRollbackChan <- true
	for {
		isClear := false
		select {
		case <-d.blockDataChan:
			time.Sleep(time.Duration(100) * time.Millisecond)
		default:
			isClear = true
		}
		if isClear {
			break
		}
	}
	endHeight := d.currentBlock.Block.Number.Uint64() - 1

	for ; ; endHeight-- {
		dbBlock := db.Mysql.GetBlockOriginalByHeight(endHeight)
		chainBlock, err := client.GetBlockAndResult(int64(endHeight))
		if err != nil {
			ZapLog.Panic("rpc get block error", zap.Error(err))
		}
		if dbBlock.BlockHash != chainBlock.Block.Hash.String() {
			rollbackData := &task.TaskChanData{
				Block: BlobToBlock(dbBlock),
				Tx:    nil,
			}
			for _, rollbackChan := range d.taskRollbackDataChan {
				rollbackChan <- rollbackData
			}
			d.checkTaskResult()
			db.DeleteOverOriginalBlock(dbBlock.Height)
		} else {
			d.currentBlock = chainBlock
			d.startHeight = endHeight + 1
			break
		}
	}

	d.isRollbackChan <- false
}

func (d *Dispatch) cacheBlock(blockData *task.TaskChanData) {
	height := blockData.Block.Block.Number.Uint64()
	if height > d.batchTo {
		irreversible, err := client.GetDposIrreversible()
		if err != nil {
			ZapLog.Panic("cache block data error", zap.Error(err))
		}
		if blockData.Block.Block.Number.Uint64() > irreversible.BftIrreversible {
			db.AddReversibleBlockCache(BlockToBlob(blockData.Block))
		}
	}
}

func BlobToBlock(block *db.BlockOriginal) *types.BlockAndResult {
	blockAndResult := &types.BlockAndResult{}
	err := rlp.DecodeBytes(block.BlockData, blockAndResult)
	if err != nil {
		ZapLog.Panic("decode block byte data error", zap.Error(err))
	}
	return blockAndResult
}

func BlockToBlob(block *types.BlockAndResult) *db.BlockOriginal {
	data, err := rlp.EncodeToBytes(block)
	if err != nil {
		ZapLog.Panic("encode block error", zap.Error(err))
	}
	result := &db.BlockOriginal{
		BlockData:  data,
		Height:     block.Block.Number.Uint64(),
		BlockHash:  block.Block.Hash.String(),
		ParentHash: block.Block.ParentHash.String(),
	}
	return result
}

func deleteRollback() {
	lastDeteteTime := time.Now().Add(time.Hour)
	for {
		if time.Now().Unix() > lastDeteteTime.Unix() {
			lastDeteteTime = lastDeteteTime.Add(time.Hour)
			irreversible, err := client.GetDposIrreversible()
			if err != nil {
				ZapLog.Panic("cache block data error", zap.Error(err))
			}
			db.DeleteRollbackAccountByHeight(irreversible.BftIrreversible)
			db.DeleteIrreversibleCache(irreversible.BftIrreversible)
			db.DeleteTokenBackupByHeight(irreversible.BftIrreversible)
		}
		time.Sleep(time.Second * 60)
	}
}
