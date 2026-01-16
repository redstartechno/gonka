package tx_manager

import (
	"decentralized-api/internal/nats/server"
	"decentralized-api/logging"
	"sync"
	"time"

	"github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/nats-io/nats.go"
	"github.com/productscience/inference/x/inference/types"
)

const (
	batchStartConsumer        = "batch-start-consumer"
	batchFinishConsumer       = "batch-finish-consumer"
	batchPocV2Consumer        = "batch-poc-v2-consumer"
	batchValidationV2Consumer = "batch-validation-v2-consumer"
	batchAckWait              = time.Minute // must exceed FlushTimeout to prevent redelivery
)

type BatchConfig struct {
	FlushSize    int
	FlushTimeout time.Duration
}

type pendingMsg struct {
	msg     sdk.Msg
	natsMsg *nats.Msg
}

type BatchConsumer struct {
	js        nats.JetStreamContext
	codec     codec.Codec
	txManager TxManager
	config    BatchConfig

	startBatch        []pendingMsg
	finishBatch       []pendingMsg
	pocV2Batch        []pendingMsg
	validationV2Batch []pendingMsg

	startMu        sync.Mutex
	finishMu       sync.Mutex
	pocV2Mu        sync.Mutex
	validationV2Mu sync.Mutex

	startCreatedAt        time.Time
	finishCreatedAt       time.Time
	pocV2CreatedAt        time.Time
	validationV2CreatedAt time.Time
}

func NewBatchConsumer(
	js nats.JetStreamContext,
	cdc codec.Codec,
	txManager TxManager,
	config BatchConfig,
) *BatchConsumer {
	return &BatchConsumer{
		js:                js,
		codec:             cdc,
		txManager:         txManager,
		config:            config,
		startBatch:        make([]pendingMsg, 0, config.FlushSize),
		finishBatch:       make([]pendingMsg, 0, config.FlushSize),
		pocV2Batch:        make([]pendingMsg, 0, config.FlushSize),
		validationV2Batch: make([]pendingMsg, 0, config.FlushSize),
	}
}

func (c *BatchConsumer) Start() error {
	if err := c.subscribeStream(server.TxsBatchStartStream, batchStartConsumer, c.handleStartMsg); err != nil {
		return err
	}
	if err := c.subscribeStream(server.TxsBatchFinishStream, batchFinishConsumer, c.handleFinishMsg); err != nil {
		return err
	}
	if err := c.subscribeStream(server.TxsBatchPocV2Stream, batchPocV2Consumer, c.handlePocV2Msg); err != nil {
		return err
	}
	if err := c.subscribeStream(server.TxsBatchValidationV2Stream, batchValidationV2Consumer, c.handleValidationV2Msg); err != nil {
		return err
	}

	go c.flushLoop()
	logging.Info("Batch consumer started", types.Messages,
		"flushSize", c.config.FlushSize,
		"flushTimeout", c.config.FlushTimeout)
	return nil
}

func (c *BatchConsumer) subscribeStream(stream, consumer string, handler func(*nats.Msg)) error {
	_, err := c.js.Subscribe(stream, handler,
		nats.Durable(consumer),
		nats.ManualAck(),
		nats.AckWait(batchAckWait),
	)
	return err
}

func (c *BatchConsumer) handleStartMsg(msg *nats.Msg) {
	if err := msg.InProgress(); err != nil {
		logging.Error("Failed to mark start msg in progress", types.Messages, "error", err)
	}
	sdkMsg, err := c.unmarshalMsg(msg.Data)
	if err != nil {
		logging.Error("Failed to unmarshal start msg", types.Messages, "error", err)
		msg.Term()
		return
	}

	var shouldFlush bool
	c.startMu.Lock()
	if len(c.startBatch) == 0 {
		c.startCreatedAt = time.Now()
	}
	c.startBatch = append(c.startBatch, pendingMsg{msg: sdkMsg, natsMsg: msg})
	shouldFlush = len(c.startBatch) >= c.config.FlushSize
	c.startMu.Unlock()

	if shouldFlush {
		c.flushStart()
	}
}

func (c *BatchConsumer) handleFinishMsg(msg *nats.Msg) {
	if err := msg.InProgress(); err != nil {
		logging.Error("Failed to mark finish msg in progress", types.Messages, "error", err)
	}
	sdkMsg, err := c.unmarshalMsg(msg.Data)
	if err != nil {
		logging.Error("Failed to unmarshal finish msg", types.Messages, "error", err)
		msg.Term()
		return
	}

	var shouldFlush bool
	c.finishMu.Lock()
	if len(c.finishBatch) == 0 {
		c.finishCreatedAt = time.Now()
	}
	c.finishBatch = append(c.finishBatch, pendingMsg{msg: sdkMsg, natsMsg: msg})
	shouldFlush = len(c.finishBatch) >= c.config.FlushSize
	c.finishMu.Unlock()

	if shouldFlush {
		c.flushFinish()
	}
}

func (c *BatchConsumer) handlePocV2Msg(msg *nats.Msg) {
	if err := msg.InProgress(); err != nil {
		logging.Error("Failed to mark poc v2 msg in progress", types.Messages, "error", err)
	}
	sdkMsg, err := c.unmarshalMsg(msg.Data)
	if err != nil {
		logging.Error("Failed to unmarshal poc v2 msg", types.Messages, "error", err)
		msg.Term()
		return
	}

	var shouldFlush bool
	c.pocV2Mu.Lock()
	if len(c.pocV2Batch) == 0 {
		c.pocV2CreatedAt = time.Now()
	}
	c.pocV2Batch = append(c.pocV2Batch, pendingMsg{msg: sdkMsg, natsMsg: msg})
	shouldFlush = len(c.pocV2Batch) >= c.config.FlushSize
	c.pocV2Mu.Unlock()

	if shouldFlush {
		c.flushPocV2()
	}
}

func (c *BatchConsumer) handleValidationV2Msg(msg *nats.Msg) {
	if err := msg.InProgress(); err != nil {
		logging.Error("Failed to mark validation v2 msg in progress", types.Messages, "error", err)
	}
	sdkMsg, err := c.unmarshalMsg(msg.Data)
	if err != nil {
		logging.Error("Failed to unmarshal validation v2 msg", types.Messages, "error", err)
		msg.Term()
		return
	}

	var shouldFlush bool
	c.validationV2Mu.Lock()
	if len(c.validationV2Batch) == 0 {
		c.validationV2CreatedAt = time.Now()
	}
	c.validationV2Batch = append(c.validationV2Batch, pendingMsg{msg: sdkMsg, natsMsg: msg})
	shouldFlush = len(c.validationV2Batch) >= c.config.FlushSize
	c.validationV2Mu.Unlock()

	if shouldFlush {
		c.flushValidationV2()
	}
}

func (c *BatchConsumer) flushLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for range ticker.C {
		c.extendAckDeadlines()
		c.checkAndFlushStart()
		c.checkAndFlushFinish()
		c.checkAndFlushPocV2()
		c.checkAndFlushValidationV2()
	}
}

func (c *BatchConsumer) extendAckDeadlines() {
	c.startMu.Lock()
	for _, p := range c.startBatch {
		_ = p.natsMsg.InProgress()
	}
	c.startMu.Unlock()

	c.finishMu.Lock()
	for _, p := range c.finishBatch {
		_ = p.natsMsg.InProgress()
	}
	c.finishMu.Unlock()

	c.pocV2Mu.Lock()
	for _, p := range c.pocV2Batch {
		_ = p.natsMsg.InProgress()
	}
	c.pocV2Mu.Unlock()

	c.validationV2Mu.Lock()
	for _, p := range c.validationV2Batch {
		_ = p.natsMsg.InProgress()
	}
	c.validationV2Mu.Unlock()
}

func (c *BatchConsumer) checkAndFlushStart() {
	c.startMu.Lock()
	shouldFlush := len(c.startBatch) > 0 && time.Since(c.startCreatedAt) >= c.config.FlushTimeout
	c.startMu.Unlock()

	if shouldFlush {
		c.flushStart()
	}
}

func (c *BatchConsumer) checkAndFlushFinish() {
	c.finishMu.Lock()
	shouldFlush := len(c.finishBatch) > 0 && time.Since(c.finishCreatedAt) >= c.config.FlushTimeout
	c.finishMu.Unlock()

	if shouldFlush {
		c.flushFinish()
	}
}

func (c *BatchConsumer) checkAndFlushPocV2() {
	c.pocV2Mu.Lock()
	shouldFlush := len(c.pocV2Batch) > 0 && time.Since(c.pocV2CreatedAt) >= c.config.FlushTimeout
	c.pocV2Mu.Unlock()

	if shouldFlush {
		c.flushPocV2()
	}
}

func (c *BatchConsumer) checkAndFlushValidationV2() {
	c.validationV2Mu.Lock()
	shouldFlush := len(c.validationV2Batch) > 0 && time.Since(c.validationV2CreatedAt) >= c.config.FlushTimeout
	c.validationV2Mu.Unlock()

	if shouldFlush {
		c.flushValidationV2()
	}
}

func (c *BatchConsumer) flushStart() {
	c.startMu.Lock()
	batch := c.startBatch
	if len(batch) == 0 {
		c.startMu.Unlock()
		return
	}
	c.startBatch = make([]pendingMsg, 0, c.config.FlushSize)
	c.startCreatedAt = time.Time{} // reset timer
	c.startMu.Unlock()

	c.broadcastBatch("start", batch)
}

func (c *BatchConsumer) flushFinish() {
	c.finishMu.Lock()
	batch := c.finishBatch
	if len(batch) == 0 {
		c.finishMu.Unlock()
		return
	}
	c.finishBatch = make([]pendingMsg, 0, c.config.FlushSize)
	c.finishCreatedAt = time.Time{} // reset timer
	c.finishMu.Unlock()

	c.broadcastBatch("finish", batch)
}

func (c *BatchConsumer) flushPocV2() {
	c.pocV2Mu.Lock()
	batch := c.pocV2Batch
	if len(batch) == 0 {
		c.pocV2Mu.Unlock()
		return
	}
	c.pocV2Batch = make([]pendingMsg, 0, c.config.FlushSize)
	c.pocV2CreatedAt = time.Time{} // reset timer
	c.pocV2Mu.Unlock()

	c.broadcastBatch("poc_v2", batch)
}

func (c *BatchConsumer) flushValidationV2() {
	c.validationV2Mu.Lock()
	batch := c.validationV2Batch
	if len(batch) == 0 {
		c.validationV2Mu.Unlock()
		return
	}
	c.validationV2Batch = make([]pendingMsg, 0, c.config.FlushSize)
	c.validationV2CreatedAt = time.Time{} // reset timer
	c.validationV2Mu.Unlock()

	c.broadcastBatch("validation_v2", batch)
}

func (c *BatchConsumer) broadcastBatch(batchType string, batch []pendingMsg) {
	msgs := make([]sdk.Msg, len(batch))
	for i, p := range batch {
		msgs[i] = p.msg
	}

	logging.Info("Broadcasting batch", types.Messages, "type", batchType, "count", len(msgs))

	if err := c.txManager.SendBatchAsyncWithRetry(msgs); err != nil {
		logging.Error("Failed to hand off batch to TxManager", types.Messages, "type", batchType, "error", err)
	}

	for _, p := range batch {
		p.natsMsg.Ack()
	}
}

func (c *BatchConsumer) unmarshalMsg(data []byte) (sdk.Msg, error) {
	var msg sdk.Msg
	if err := c.codec.UnmarshalInterfaceJSON(data, &msg); err != nil {
		return nil, err
	}
	return msg, nil
}

func (c *BatchConsumer) PublishStartInference(msg sdk.Msg) error {
	return c.publishMsg(server.TxsBatchStartStream, msg)
}

func (c *BatchConsumer) PublishFinishInference(msg sdk.Msg) error {
	return c.publishMsg(server.TxsBatchFinishStream, msg)
}

func (c *BatchConsumer) PublishPocBatchV2(msg sdk.Msg) error {
	return c.publishMsg(server.TxsBatchPocV2Stream, msg)
}

func (c *BatchConsumer) PublishPocValidationV2(msg sdk.Msg) error {
	return c.publishMsg(server.TxsBatchValidationV2Stream, msg)
}

func (c *BatchConsumer) publishMsg(stream string, msg sdk.Msg) error {
	data, err := c.codec.MarshalInterfaceJSON(msg)
	if err != nil {
		return err
	}
	_, err = c.js.Publish(stream, data)
	return err
}
