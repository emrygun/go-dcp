package stream

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Trendyol/go-dcp/stream/offset"

	"github.com/Trendyol/go-dcp/tracing"

	"github.com/Trendyol/go-dcp/membership"

	"github.com/couchbase/gocbcore/v10"

	"github.com/Trendyol/go-dcp/wrapper"

	"github.com/Trendyol/go-dcp/config"

	"github.com/Trendyol/go-dcp/metadata"

	"github.com/Trendyol/go-dcp/couchbase"
	"github.com/Trendyol/go-dcp/models"

	"github.com/Trendyol/go-dcp/logger"

	"github.com/Trendyol/go-dcp/helpers"
)

type Stream interface {
	Open()
	Rebalance()
	Save()
	Close(bool)
	GetOffsets() (*wrapper.ConcurrentSwissMap[uint16, *models.Offset], *wrapper.ConcurrentSwissMap[uint16, bool], bool)
	GetObservers() *wrapper.ConcurrentSwissMap[uint16, couchbase.Observer]
	GetMetric() (*Metric, int32)
	UnmarkDirtyOffsets()
	GetCheckpointMetric() *CheckpointMetric
	IsOpen() bool
}

type Metric struct {
	ProcessLatency int64
	DcpLatency     int64
	Rebalance      int
}

type stream struct {
	client                       couchbase.Client
	metadata                     metadata.Metadata
	checkpoint                   Checkpoint
	rollbackMitigation           couchbase.RollbackMitigation
	vBucketDiscovery             VBucketDiscovery
	eventHandler                 models.EventHandler
	config                       *config.Dcp
	metric                       *Metric
	rebalanceTimer               *time.Timer
	vbIDRange                    *models.VbIDRange
	dirtyOffsets                 *wrapper.ConcurrentSwissMap[uint16, bool]
	stopCh                       chan struct{}
	consumer                     models.Consumer
	bucketInfo                   *couchbase.BucketInfo
	finishStreamWithEndEventCh   chan struct{}
	finishStreamWithCloseCh      chan struct{}
	offsets                      *wrapper.ConcurrentSwissMap[uint16, *models.Offset]
	observers                    *wrapper.ConcurrentSwissMap[uint16, couchbase.Observer]
	collectionIDs                map[uint32]string
	streamEndNotSupportedData    *streamEndNotSupportedData
	tracerComponent              *tracing.TracerComponent
	rebalanceLock                sync.Mutex
	activeStreams                atomic.Int32
	streamFinishedWithCloseCh    bool
	streamFinishedWithEndEventCh bool
	anyDirtyOffset               bool
	balancing                    bool
	closeWithCancel              bool
	open                         bool
}

type streamEndNotSupportedData struct {
	queue  chan struct{}
	ending bool
}

func (s *stream) setOffset(vbID uint16, offset *models.Offset, dirty bool) {
	if s.vbIDRange.In(vbID) {
		if current, ok := s.offsets.Load(vbID); ok && current.SeqNo > offset.SeqNo {
			return
		}
		s.offsets.Store(vbID, offset)
		s.consumer.TrackOffset(vbID, offset)
		if !dirty {
			return
		}

		s.dirtyOffsets.StoreIf(vbID, func(p bool, f bool) (v bool, s bool) {
			if !f || (f && !p) {
				return true, true
			}

			return p, false
		})
	} else {
		logger.Log.Warn("vbID: %v not belong our vbID range", vbID)
	}
}

func (s *stream) waitAndForward(
	payload interface{},
	spanCtx tracing.RequestSpanContext,
	offset *models.Offset,
	vbID uint16,
	eventTime time.Time,
) {
	if helpers.IsMetadata(payload) {
		s.setOffset(vbID, offset, false)
		return
	}

	s.metric.DcpLatency = time.Since(eventTime).Milliseconds()

	ctx := &models.ListenerContext{
		Commit: s.checkpoint.Save,
		Event:  payload,
		Ack: func() {
			s.setOffset(vbID, offset, true)
			s.anyDirtyOffset = true
		},
		ListenerTracerComponent: s.tracerComponent.NewListenerTracerComponent(spanCtx),
	}

	start := time.Now()

	s.consumer.ConsumeEvent(ctx)

	s.metric.ProcessLatency = time.Since(start).Milliseconds()
}

func (s *stream) listen(args models.ListenerArgs) {
	switch v := args.Event.(type) {
	case models.DcpMutation:
		s.waitAndForward(v, args.TraceContext, v.Offset, v.VbID, v.EventTime)
	case models.DcpDeletion:
		s.waitAndForward(v, args.TraceContext, v.Offset, v.VbID, v.EventTime)
	case models.DcpExpiration:
		s.waitAndForward(v, args.TraceContext, v.Offset, v.VbID, v.EventTime)
	case models.DcpSeqNoAdvanced:
		s.setOffset(v.VbID, v.Offset, true)
	case models.DcpCollectionCreation:
		s.setOffset(v.VbID, v.Offset, true)
	case models.DcpCollectionDeletion:
		s.setOffset(v.VbID, v.Offset, true)
	case models.DcpCollectionFlush:
		s.setOffset(v.VbID, v.Offset, true)
	case models.DcpScopeCreation:
		s.setOffset(v.VbID, v.Offset, true)
	case models.DcpScopeDeletion:
		s.setOffset(v.VbID, v.Offset, true)
	case models.DcpCollectionModification:
		s.setOffset(v.VbID, v.Offset, true)
	default:
	}
}

func (s *stream) reopenStream(vbID uint16) {
	retry := 5

	for {
		err := s.openStream(vbID)
		if err == nil {
			logger.Log.Info("re-open stream, vbID: %d", vbID)
			break
		} else {
			logger.Log.Warn("cannot re-open stream, vbID: %d, err: %v", vbID, err)
		}

		retry--
		if retry == 0 {
			logger.Log.Error("error while re-open stream, vbID: %d, err: give up after few retry", vbID)
			panic(err)
		}

		time.Sleep(time.Second)
	}
}

func (s *stream) listenEnd(endContext models.DcpStreamEndContext) {
	if s.streamEndNotSupportedData != nil && s.streamEndNotSupportedData.ending {
		<-s.streamEndNotSupportedData.queue
	}

	if !s.closeWithCancel && endContext.Err != nil {
		if !errors.Is(endContext.Err, gocbcore.ErrDCPStreamClosed) {
			logger.Log.Warn("end stream vbID: %v got error: %v", endContext.Event.VbID, endContext.Err)
		} else {
			logger.Log.Debug("end stream vbID: %v got error: %v", endContext.Event.VbID, endContext.Err)
		}
	}

	if endContext.Err == nil {
		logger.Log.Debug("end stream vbID: %v", endContext.Event.VbID)
	}

	if !s.closeWithCancel && endContext.Err != nil &&
		(errors.Is(endContext.Err, gocbcore.ErrSocketClosed) ||
			errors.Is(endContext.Err, gocbcore.ErrDCPBackfillFailed) ||
			errors.Is(endContext.Err, gocbcore.ErrDCPStreamStateChanged) ||
			errors.Is(endContext.Err, gocbcore.ErrDCPStreamTooSlow) ||
			errors.Is(endContext.Err, gocbcore.ErrDCPStreamDisconnected)) {
		go s.reopenStream(endContext.Event.VbID)
	} else {
		activeStreams := s.activeStreams.Add(-1)
		if activeStreams == 0 && !s.streamFinishedWithCloseCh {
			s.finishStreamWithEndEventCh <- struct{}{}
		}
	}
}

func (s *stream) Open() {
	s.streamFinishedWithCloseCh = false
	s.streamFinishedWithEndEventCh = false

	s.eventHandler.BeforeStreamStart()

	vbIDs := s.vBucketDiscovery.Get()
	s.vbIDRange = &models.VbIDRange{
		Start: vbIDs[0],
		End:   vbIDs[len(vbIDs)-1],
	}

	if !s.config.RollbackMitigation.Disabled {
		if s.bucketInfo.IsEphemeral() {
			logger.Log.Info("rollback mitigation is disabled for ephemeral bucket")
			s.config.RollbackMitigation.Disabled = true
		} else {
			s.rollbackMitigation = couchbase.NewRollbackMitigation(s.client, s.config, vbIDs, s.dispatchPersistSeqNo)
			s.rollbackMitigation.Start()
		}
	}

	s.activeStreams.Swap(int32(len(vbIDs)))

	latestSeqNoInitializer := offset.NewOffsetLatestSeqNoInit(s.config)

	s.checkpoint = NewCheckpoint(s, vbIDs, s.client, s.metadata, s.config, latestSeqNoInitializer)
	s.offsets, s.dirtyOffsets, s.anyDirtyOffset = s.checkpoint.Load()

	s.observers = wrapper.CreateConcurrentSwissMap[uint16, couchbase.Observer](1024)
	s.offsets.Range(func(vbID uint16, offset *models.Offset) bool {
		s.observers.Store(
			vbID,
			couchbase.NewObserver(s.config,
				vbID, offset.LatestSeqNo, s.listen, s.listenEnd, s.collectionIDs, s.tracerComponent,
			),
		)

		return true
	})

	s.openAllStreams(vbIDs)

	logger.Log.Info("stream started")
	s.eventHandler.AfterStreamStart()

	s.checkpoint.StartSchedule()

	go s.wait()
	s.open = true
}

func (s *stream) IsOpen() bool {
	return s.open
}

func (s *stream) Rebalance() {
	if s.balancing && s.rebalanceTimer != nil {
		// Is rebalance timer triggered already
		if s.rebalanceTimer.Stop() {
			s.rebalanceTimer.Reset(s.config.Dcp.Group.Membership.RebalanceDelay)
			logger.Log.Info("latest rebalance time is resetted")
		} else {
			s.rebalanceTimer = time.AfterFunc(s.config.Dcp.Group.Membership.RebalanceDelay, s.Rebalance)
			logger.Log.Info("latest rebalance time is reassigned")
		}
		return
	}
	logger.Log.Info("rebalance starting")
	s.rebalanceLock.Lock()

	s.eventHandler.BeforeRebalanceStart()

	if !s.balancing {
		s.balancing = true
		s.Close(false)
	}

	s.eventHandler.AfterRebalanceStart()

	if s.config.Dcp.Group.Membership.Type == membership.DynamicMembershipType {
		s.rebalanceTimer = time.AfterFunc(0, s.rebalance)
		logger.Log.Info("rebalance delay is disabled on dynamic membership")
	} else {
		s.rebalanceTimer = time.AfterFunc(s.config.Dcp.Group.Membership.RebalanceDelay, s.rebalance)
		logger.Log.Info("rebalance will start after %v", s.config.Dcp.Group.Membership.RebalanceDelay)
	}
}

func (s *stream) rebalance() {
	logger.Log.Info("reassigning vbuckets and opening stream is starting")

	defer s.rebalanceLock.Unlock()

	s.eventHandler.BeforeRebalanceEnd()
	s.Open()
	s.metric.Rebalance++

	logger.Log.Info("rebalance is finished")
	s.balancing = false
	s.eventHandler.AfterRebalanceEnd()
}

func (s *stream) Save() {
	s.checkpoint.Save()
}

func (s *stream) dispatchPersistSeqNo(persistSeqNo *models.PersistSeqNo) {
	if s.observers != nil {
		if observer, ok := s.observers.Load(persistSeqNo.VbID); ok {
			observer.SetPersistSeqNo(persistSeqNo.SeqNo)
		}
	}
}

func (s *stream) openStream(vbID uint16) error {
	offset, exist := s.offsets.Load(vbID)
	if !exist {
		err := fmt.Errorf("vbID: %d not found on offset map", vbID)
		logger.Log.Error("error while opening stream, err: %v", err)
		return err
	}
	observer, _ := s.observers.Load(vbID)
	return s.client.OpenStream(vbID, s.collectionIDs, offset, observer)
}

func (s *stream) openAllStreams(vbIDs []uint16) {
	openWg := &sync.WaitGroup{}
	openWg.Add(len(vbIDs))

	for _, vbID := range vbIDs {
		go func(innerVbId uint16) {
			err := s.openStream(innerVbId)
			if err != nil {
				logger.Log.Error("error while open stream, vbID: %d, err: %v", innerVbId, err)
				panic(err)
			}
			openWg.Done()
		}(vbID)
	}

	openWg.Wait()
}

func (s *stream) closeAllStreams() {
	// We need to do this without async when couchbase version below v5.5.0.
	// Because "gocbcore - memdopmap.go - FindOpenStream" is not thread safe.
	// BTW We cannot use ConcurrentSwissMap either. You know it's concurrent :/
	if s.streamEndNotSupportedData != nil {
		s.streamEndNotSupportedData.ending = true
		for vbID := s.vbIDRange.Start; vbID <= s.vbIDRange.End; vbID++ {
			s.streamEndNotSupportedData.queue <- struct{}{}
			if err := s.client.CloseStream(vbID); err != nil {
				logger.Log.Error(
					"cannot close stream on (stream end not supporting) mode, vbID: %d, err: %v",
					vbID, err,
				)
			}
		}
		s.streamEndNotSupportedData.ending = false
	} else {
		var wg sync.WaitGroup
		wg.Add(s.offsets.Count())
		s.offsets.Range(func(vbID uint16, _ *models.Offset) bool {
			go func(vbID uint16) {
				if err := s.client.CloseStream(vbID); err != nil {
					logger.Log.Error("cannot close stream, vbID: %d, err: %v", vbID, err)
				}

				wg.Done()
			}(vbID)

			return true
		})

		wg.Wait()
	}
}

func (s *stream) wait() {
	select {
	case <-s.finishStreamWithCloseCh:
		s.streamFinishedWithCloseCh = true
	case <-s.finishStreamWithEndEventCh:
		s.streamFinishedWithEndEventCh = true
	}

	if !s.balancing {
		close(s.stopCh)
	}
}

func (s *stream) Close(closeWithCancel bool) {
	s.closeWithCancel = closeWithCancel

	s.eventHandler.BeforeStreamStop()

	if !s.config.RollbackMitigation.Disabled {
		s.rollbackMitigation.Stop()
	}

	s.observers.Range(func(_ uint16, observer couchbase.Observer) bool {
		observer.Close()
		return true
	})

	if s.checkpoint != nil {
		s.checkpoint.StopSchedule()
	}

	s.closeAllStreams()

	s.observers.Range(func(_ uint16, observer couchbase.Observer) bool {
		observer.CloseEnd()
		return true
	})
	s.observers = nil

	s.offsets = wrapper.CreateConcurrentSwissMap[uint16, *models.Offset](1024)
	s.dirtyOffsets = wrapper.CreateConcurrentSwissMap[uint16, bool](1024)

	logger.Log.Info("stream stopped")
	s.eventHandler.AfterStreamStop()
	s.open = false

	if !s.streamFinishedWithEndEventCh {
		s.finishStreamWithCloseCh <- struct{}{}
	}
}

func (s *stream) GetOffsets() (*wrapper.ConcurrentSwissMap[uint16, *models.Offset], *wrapper.ConcurrentSwissMap[uint16, bool], bool) {
	return s.offsets, s.dirtyOffsets, s.anyDirtyOffset
}

func (s *stream) GetObservers() *wrapper.ConcurrentSwissMap[uint16, couchbase.Observer] {
	return s.observers
}

func (s *stream) GetMetric() (*Metric, int32) {
	return s.metric, s.activeStreams.Load()
}

func (s *stream) GetCheckpointMetric() *CheckpointMetric {
	return s.checkpoint.GetMetric()
}

func (s *stream) UnmarkDirtyOffsets() {
	s.anyDirtyOffset = false
	s.dirtyOffsets = wrapper.CreateConcurrentSwissMap[uint16, bool](1024)
}

func NewStream(client couchbase.Client,
	metadata metadata.Metadata,
	config *config.Dcp,
	version *couchbase.Version,
	bucketInfo *couchbase.BucketInfo,
	vBucketDiscovery VBucketDiscovery,
	consumer models.Consumer,
	collectionIDs map[uint32]string,
	stopCh chan struct{},
	eventHandler models.EventHandler,
	tc *tracing.TracerComponent,
) Stream {
	stream := &stream{
		client:                     client,
		metadata:                   metadata,
		consumer:                   consumer,
		config:                     config,
		bucketInfo:                 bucketInfo,
		vBucketDiscovery:           vBucketDiscovery,
		collectionIDs:              collectionIDs,
		finishStreamWithCloseCh:    make(chan struct{}, 1),
		finishStreamWithEndEventCh: make(chan struct{}, 1),
		stopCh:                     stopCh,
		eventHandler:               eventHandler,
		metric:                     &Metric{},
		tracerComponent:            tc,
	}

	if version.Lower(couchbase.SrvVer550) {
		stream.streamEndNotSupportedData = &streamEndNotSupportedData{
			ending: false,
			queue:  make(chan struct{}, 1),
		}
	}

	return stream
}
