package v2

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tendermint/go-amino"

	"github.com/tendermint/tendermint/behaviour"
	"github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/p2p"
	"github.com/tendermint/tendermint/state"
	"github.com/tendermint/tendermint/types"
)

const (
	// chBufferSize is the buffer size of all event channels.
	chBufferSize int = 1000
)

//-------------------------------------

type bcBlockRequestMessage struct {
	Height int64
}

// ValidateBasic performs basic validation.
func (m *bcBlockRequestMessage) ValidateBasic() error {
	if m.Height < 0 {
		return errors.New("negative Height")
	}
	return nil
}

func (m *bcBlockRequestMessage) String() string {
	return fmt.Sprintf("[bcBlockRequestMessage %v]", m.Height)
}

type bcNoBlockResponseMessage struct {
	Height int64
}

// ValidateBasic performs basic validation.
func (m *bcNoBlockResponseMessage) ValidateBasic() error {
	if m.Height < 0 {
		return errors.New("negative Height")
	}
	return nil
}

func (m *bcNoBlockResponseMessage) String() string {
	return fmt.Sprintf("[bcNoBlockResponseMessage %d]", m.Height)
}

//-------------------------------------

type bcBlockResponseMessage struct {
	Block *types.Block
}

// ValidateBasic performs basic validation.
func (m *bcBlockResponseMessage) ValidateBasic() error {
	if m.Block == nil {
		return errors.New("block response message has nil block")
	}

	return m.Block.ValidateBasic()
}

func (m *bcBlockResponseMessage) String() string {
	return fmt.Sprintf("[bcBlockResponseMessage %v]", m.Block.Height)
}

//-------------------------------------

type bcStatusRequestMessage struct {
	Height int64
	Base   int64
}

// ValidateBasic performs basic validation.
func (m *bcStatusRequestMessage) ValidateBasic() error {
	if m.Base < 0 {
		return errors.New("negative Base")
	}
	if m.Height < 0 {
		return errors.New("negative Height")
	}
	if m.Base > m.Height {
		return fmt.Errorf("base %v cannot be greater than height %v", m.Base, m.Height)
	}
	return nil
}

func (m *bcStatusRequestMessage) String() string {
	return fmt.Sprintf("[bcStatusRequestMessage %v:%v]", m.Base, m.Height)
}

//-------------------------------------

type bcStatusResponseMessage struct {
	Height int64
	Base   int64
}

// ValidateBasic performs basic validation.
func (m *bcStatusResponseMessage) ValidateBasic() error {
	if m.Base < 0 {
		return errors.New("negative Base")
	}
	if m.Height < 0 {
		return errors.New("negative Height")
	}
	if m.Base > m.Height {
		return fmt.Errorf("base %v cannot be greater than height %v", m.Base, m.Height)
	}
	return nil
}

func (m *bcStatusResponseMessage) String() string {
	return fmt.Sprintf("[bcStatusResponseMessage %v:%v]", m.Base, m.Height)
}

type blockStore interface {
	LoadBlock(height int64) *types.Block
	SaveBlock(*types.Block, *types.PartSet, *types.Commit)
	Base() int64
	Height() int64
}

// BlockchainReactor handles fast sync protocol.
type BlockchainReactor struct {
	p2p.BaseReactor

	fastSync    bool // if true, enable fast sync on start
	stateSynced bool // set to true when SwitchToFastSync is called by state sync
	scheduler   *Routine
	processor   *Routine
	logger      log.Logger

	mtx           sync.RWMutex
	maxPeerHeight int64
	syncHeight    int64
	events        chan Event // non-nil during a fast sync

	reporter behaviour.Reporter
	io       iIO
	store    blockStore
}

//nolint:unused,deadcode
type blockVerifier interface {
	VerifyCommit(chainID string, blockID types.BlockID, height int64, commit *types.Commit) error
}

type blockApplier interface {
	ApplyBlock(state state.State, blockID types.BlockID, block *types.Block) (state.State, int64, error)
}

// XXX: unify naming in this package around tmState
func newReactor(state state.State, store blockStore, reporter behaviour.Reporter,
	blockApplier blockApplier, fastSync bool) *BlockchainReactor {
	scheduler := newScheduler(state.LastBlockHeight, time.Now())
	pContext := newProcessorContext(store, blockApplier, state)
	// TODO: Fix naming to just newProcesssor
	// newPcState requires a processorContext
	processor := newPcState(pContext)

	return &BlockchainReactor{
		scheduler: newRoutine("scheduler", scheduler.handle, chBufferSize),
		processor: newRoutine("processor", processor.handle, chBufferSize),
		store:     store,
		reporter:  reporter,
		logger:    log.NewNopLogger(),
		fastSync:  fastSync,
	}
}

// NewBlockchainReactor creates a new reactor instance.
func NewBlockchainReactor(
	state state.State,
	blockApplier blockApplier,
	store blockStore,
	fastSync bool) *BlockchainReactor {
	reporter := behaviour.NewMockReporter()
	return newReactor(state, store, reporter, blockApplier, fastSync)
}

// SetSwitch implements Reactor interface.
func (r *BlockchainReactor) SetSwitch(sw *p2p.Switch) {
	r.Switch = sw
	if sw != nil {
		r.io = newSwitchIo(sw)
	} else {
		r.io = nil
	}
}

func (r *BlockchainReactor) setMaxPeerHeight(height int64) {
	r.mtx.Lock()
	defer r.mtx.Unlock()
	if height > r.maxPeerHeight {
		r.maxPeerHeight = height
	}
}

func (r *BlockchainReactor) setSyncHeight(height int64) {
	r.mtx.Lock()
	defer r.mtx.Unlock()
	r.syncHeight = height
}

// SyncHeight returns the height to which the BlockchainReactor has synced.
func (r *BlockchainReactor) SyncHeight() int64 {
	r.mtx.RLock()
	defer r.mtx.RUnlock()
	return r.syncHeight
}

// SetLogger sets the logger of the reactor.
func (r *BlockchainReactor) SetLogger(logger log.Logger) {
	r.logger = logger
	r.scheduler.setLogger(logger)
	r.processor.setLogger(logger)
}

// Start implements cmn.Service interface
func (r *BlockchainReactor) Start() error {
	r.reporter = behaviour.NewSwitchReporter(r.BaseReactor.Switch)
	if r.fastSync {
		err := r.startSync(nil)
		if err != nil {
			return fmt.Errorf("failed to start fast sync: %w", err)
		}
	}
	return nil
}

// startSync begins a fast sync, signalled by r.events being non-nil. If state is non-nil,
// the scheduler and processor is updated with this state on startup.
func (r *BlockchainReactor) startSync(state *state.State) error {
	r.mtx.Lock()
	defer r.mtx.Unlock()
	if r.events != nil {
		return errors.New("fast sync already in progress")
	}
	r.events = make(chan Event, chBufferSize)
	go r.scheduler.start()
	go r.processor.start()
	if state != nil {
		<-r.scheduler.ready()
		<-r.processor.ready()
		r.scheduler.send(bcResetState{state: *state})
		r.processor.send(bcResetState{state: *state})
	}
	go r.demux(r.events)
	return nil
}

// endSync ends a fast sync
func (r *BlockchainReactor) endSync() {
	r.mtx.Lock()
	defer r.mtx.Unlock()
	if r.events != nil {
		close(r.events)
	}
	r.events = nil
	r.scheduler.stop()
	r.processor.stop()
}

// SwitchToFastSync is called by the state sync reactor when switching to fast sync.
func (r *BlockchainReactor) SwitchToFastSync(state state.State) error {
	r.stateSynced = true
	state = state.Copy()
	return r.startSync(&state)
}

// reactor generated ticker events:
// ticker for cleaning peers
type rTryPrunePeer struct {
	priorityHigh
	time time.Time
}

func (e rTryPrunePeer) String() string {
	return fmt.Sprintf(": %v", e.time)
}

// ticker event for scheduling block requests
type rTrySchedule struct {
	priorityHigh
	time time.Time
}

func (e rTrySchedule) String() string {
	return fmt.Sprintf(": %v", e.time)
}

// ticker for block processing
type rProcessBlock struct {
	priorityNormal
}

// reactor generated events based on blockchain related messages from peers:
// blockResponse message received from a peer
type bcBlockResponse struct {
	priorityNormal
	time   time.Time
	peerID p2p.ID
	size   int64
	block  *types.Block
}

// blockNoResponse message received from a peer
type bcNoBlockResponse struct {
	priorityNormal
	time   time.Time
	peerID p2p.ID
	height int64
}

// statusResponse message received from a peer
type bcStatusResponse struct {
	priorityNormal
	time   time.Time
	peerID p2p.ID
	base   int64
	height int64
}

// new peer is connected
type bcAddNewPeer struct {
	priorityNormal
	peerID p2p.ID
}

// existing peer is removed
type bcRemovePeer struct {
	priorityHigh
	peerID p2p.ID
	reason interface{}
}

// signals event loop and routines to begin scheduling block events, optionally resetting state
type bcResetState struct {
	priorityHigh
	state state.State
}

// Takes the channel as a parameter to avoid race conditions on r.events.
func (r *BlockchainReactor) demux(events <-chan Event) {
	var lastRate = 0.0
	var lastHundred = time.Now()

	var (
		processBlockFreq = 20 * time.Millisecond
		doProcessBlockCh = make(chan struct{}, 1)
		doProcessBlockTk = time.NewTicker(processBlockFreq)
	)
	defer doProcessBlockTk.Stop()

	var (
		prunePeerFreq = 1 * time.Second
		doPrunePeerCh = make(chan struct{}, 1)
		doPrunePeerTk = time.NewTicker(prunePeerFreq)
	)
	defer doPrunePeerTk.Stop()

	var (
		scheduleFreq = 20 * time.Millisecond
		doScheduleCh = make(chan struct{}, 1)
		doScheduleTk = time.NewTicker(scheduleFreq)
	)
	defer doScheduleTk.Stop()

	var (
		statusFreq = 10 * time.Second
		doStatusCh = make(chan struct{}, 1)
		doStatusTk = time.NewTicker(statusFreq)
	)
	defer doStatusTk.Stop()
	doStatusCh <- struct{}{} // immediately broadcast to get status of existing peers

	// XXX: Extract timers to make testing atemporal
	for {
		select {
		// Pacers: send at most per frequency but don't saturate
		case <-doProcessBlockTk.C:
			select {
			case doProcessBlockCh <- struct{}{}:
			default:
			}
		case <-doPrunePeerTk.C:
			select {
			case doPrunePeerCh <- struct{}{}:
			default:
			}
		case <-doScheduleTk.C:
			select {
			case doScheduleCh <- struct{}{}:
			default:
			}
		case <-doStatusTk.C:
			select {
			case doStatusCh <- struct{}{}:
			default:
			}

		// Tickers: perform tasks periodically
		case <-doScheduleCh:
			r.scheduler.send(rTrySchedule{time: time.Now()})
		case <-doPrunePeerCh:
			r.scheduler.send(rTryPrunePeer{time: time.Now()})
		case <-doProcessBlockCh:
			r.processor.send(rProcessBlock{})
		case <-doStatusCh:
			r.io.broadcastStatusRequest(r.store.Base(), r.SyncHeight())

		// Events from peers. Closing the channel signals event loop termination.
		case event, ok := <-events:
			if !ok {
				r.logger.Info("Stopping event processing")
				return
			}
			switch event := event.(type) {
			case bcStatusResponse:
				r.setMaxPeerHeight(event.height)
				r.scheduler.send(event)
			case bcAddNewPeer, bcRemovePeer, bcBlockResponse, bcNoBlockResponse:
				r.scheduler.send(event)
			default:
				r.logger.Error("Received unexpected event", "event", fmt.Sprintf("%T", event))
			}

		// Incremental events from scheduler
		case event := <-r.scheduler.next():
			switch event := event.(type) {
			case scBlockReceived:
				r.processor.send(event)
			case scPeerError:
				r.processor.send(event)
				r.reporter.Report(behaviour.BadMessage(event.peerID, "scPeerError"))
			case scBlockRequest:
				r.io.sendBlockRequest(event.peerID, event.height)
			case scFinishedEv:
				r.processor.send(event)
				r.scheduler.stop()
			case scSchedulerFail:
				r.logger.Error("Scheduler failure", "err", event.reason.Error())
			case scPeersPruned:
				r.logger.Debug("Pruned peers", "count", len(event.peers))
			case noOpEvent:
			default:
				r.logger.Error("Received unexpected scheduler event", "event", fmt.Sprintf("%T", event))
			}

		// Incremental events from processor
		case event := <-r.processor.next():
			switch event := event.(type) {
			case pcBlockProcessed:
				r.setSyncHeight(event.height)
				if r.syncHeight%100 == 0 {
					lastRate = 0.9*lastRate + 0.1*(100/time.Since(lastHundred).Seconds())
					r.logger.Info("Fast Sync Rate", "height", r.syncHeight,
						"max_peer_height", r.maxPeerHeight, "blocks/s", lastRate)
					lastHundred = time.Now()
				}
				r.scheduler.send(event)
			case pcBlockVerificationFailure:
				r.scheduler.send(event)
			case pcFinished:
				r.logger.Info("Fast sync complete, switching to consensus")
				if !r.io.trySwitchToConsensus(event.tmState, event.blocksSynced > 0 || r.stateSynced) {
					r.logger.Error("Failed to switch to consensus reactor")
				}
				r.endSync()
				return
			case noOpEvent:
			default:
				r.logger.Error("Received unexpected processor event", "event", fmt.Sprintf("%T", event))
			}

		// Terminal event from scheduler
		case err := <-r.scheduler.final():
			switch err {
			case nil:
				r.logger.Info("Scheduler stopped")
			default:
				r.logger.Error("Scheduler aborted with error", "err", err)
			}

		// Terminal event from processor
		case err := <-r.processor.final():
			switch err {
			case nil:
				r.logger.Info("Processor stopped")
			default:
				r.logger.Error("Processor aborted with error", "err", err)
			}
		}
	}
}

// Stop implements cmn.Service interface.
func (r *BlockchainReactor) Stop() error {
	r.logger.Info("reactor stopping")
	r.endSync()
	r.logger.Info("reactor stopped")
	return nil
}

const (
	// NOTE: keep up to date with bcBlockResponseMessage
	bcBlockResponseMessagePrefixSize   = 4
	bcBlockResponseMessageFieldKeySize = 1
	maxMsgSize                         = types.MaxBlockSizeBytes +
		bcBlockResponseMessagePrefixSize +
		bcBlockResponseMessageFieldKeySize
)

// BlockchainMessage is a generic message for this reactor.
type BlockchainMessage interface {
	ValidateBasic() error
}

// RegisterBlockchainMessages registers the fast sync messages for amino encoding.
func RegisterBlockchainMessages(cdc *amino.Codec) {
	cdc.RegisterInterface((*BlockchainMessage)(nil), nil)
	cdc.RegisterConcrete(&bcBlockRequestMessage{}, "tendermint/blockchain/BlockRequest", nil)
	cdc.RegisterConcrete(&bcBlockResponseMessage{}, "tendermint/blockchain/BlockResponse", nil)
	cdc.RegisterConcrete(&bcNoBlockResponseMessage{}, "tendermint/blockchain/NoBlockResponse", nil)
	cdc.RegisterConcrete(&bcStatusResponseMessage{}, "tendermint/blockchain/StatusResponse", nil)
	cdc.RegisterConcrete(&bcStatusRequestMessage{}, "tendermint/blockchain/StatusRequest", nil)
}

func decodeMsg(bz []byte) (msg BlockchainMessage, err error) {
	err = cdc.UnmarshalBinaryBare(bz, &msg)
	return
}

// Receive implements Reactor by handling different message types.
func (r *BlockchainReactor) Receive(chID byte, src p2p.Peer, msgBytes []byte) {
	msg, err := decodeMsg(msgBytes)
	if err != nil {
		r.logger.Error("error decoding message",
			"src", src.ID(), "chId", chID, "msg", msg, "err", err, "bytes", msgBytes)
		_ = r.reporter.Report(behaviour.BadMessage(src.ID(), err.Error()))
		return
	}

	if err = msg.ValidateBasic(); err != nil {
		r.logger.Error("peer sent us invalid msg", "peer", src, "msg", msg, "err", err)
		_ = r.reporter.Report(behaviour.BadMessage(src.ID(), err.Error()))
		return
	}

	r.logger.Debug("Receive", "src", src.ID(), "chID", chID, "msg", msg)

	switch msg := msg.(type) {
	case *bcStatusRequestMessage:
		if err := r.io.sendStatusResponse(r.store.Height(), src.ID()); err != nil {
			r.logger.Error("Could not send status message to peer", "src", src)
		}

	case *bcBlockRequestMessage:
		block := r.store.LoadBlock(msg.Height)
		if block != nil {
			if err = r.io.sendBlockToPeer(block, src.ID()); err != nil {
				r.logger.Error("Could not send block message to peer: ", err)
			}
		} else {
			r.logger.Info("peer asking for a block we don't have", "src", src, "height", msg.Height)
			peerID := src.ID()
			if err = r.io.sendBlockNotFound(msg.Height, peerID); err != nil {
				r.logger.Error("Couldn't send block not found: ", err)
			}
		}

	case *bcStatusResponseMessage:
		r.mtx.RLock()
		if r.events != nil {
			r.events <- bcStatusResponse{peerID: src.ID(), base: msg.Base, height: msg.Height}
		}
		r.mtx.RUnlock()

	case *bcBlockResponseMessage:
		r.mtx.RLock()
		if r.events != nil {
			r.events <- bcBlockResponse{
				peerID: src.ID(),
				block:  msg.Block,
				size:   int64(len(msgBytes)),
				time:   time.Now(),
			}
		}
		r.mtx.RUnlock()

	case *bcNoBlockResponseMessage:
		r.mtx.RLock()
		if r.events != nil {
			r.events <- bcNoBlockResponse{peerID: src.ID(), height: msg.Height, time: time.Now()}
		}
		r.mtx.RUnlock()
	}
}

// AddPeer implements Reactor interface
func (r *BlockchainReactor) AddPeer(peer p2p.Peer) {
	err := r.io.sendStatusResponse(r.store.Height(), peer.ID())
	if err != nil {
		r.logger.Error("Could not send status message to peer new", "src", peer.ID, "height", r.SyncHeight())
	}
	r.mtx.RLock()
	defer r.mtx.RUnlock()
	if r.events != nil {
		r.events <- bcAddNewPeer{peerID: peer.ID()}
	}
}

// RemovePeer implements Reactor interface.
func (r *BlockchainReactor) RemovePeer(peer p2p.Peer, reason interface{}) {
	r.mtx.RLock()
	defer r.mtx.RUnlock()
	if r.events != nil {
		r.events <- bcRemovePeer{
			peerID: peer.ID(),
			reason: reason,
		}
	}
}

// GetChannels implements Reactor
func (r *BlockchainReactor) GetChannels() []*p2p.ChannelDescriptor {
	return []*p2p.ChannelDescriptor{
		{
			ID:                  BlockchainChannel,
			Priority:            10,
			SendQueueCapacity:   2000,
			RecvBufferCapacity:  50 * 4096,
			RecvMessageCapacity: maxMsgSize,
		},
	}
}
