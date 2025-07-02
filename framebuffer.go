package framebuffer

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"
)

// --- Core Interfaces ---

// Frame represents the essential component of any item that can be processed by the buffer.
// It only requires a creation timestamp, which is used for all timing and ordering logic.
type Frame interface {
	CreatedTime() time.Time
}

// ManagedItem is an optional interface for frames that require manual resource management,
// such as reference counting for shared memory buffers. The buffer will automatically call
// these methods if the frame type implements them.
type ManagedItem[T any] interface {
	// Ref should return a new reference to the item. It is called when the buffer
	// needs to duplicate a frame to maintain the output frame rate.
	Ref() T
	// Cleanup should release any resources held by the frame. It is called when
	// a frame is dropped or when the buffer is closed.
	Cleanup()
}

// Interpolator is an optional interface for frames that can be synthetically generated
// to fill time gaps. This allows for smoother output than simply duplicating frames.
type Interpolator[T any] interface {
	// Interpolate should create a new frame that is a blend between the receiver (frame A)
	// and another frame (frame B). The factor (0.0 to 1.0) determines the blend.
	Interpolate(other T, factor float64) T
}

// --- Usability & Integration Types ---

// Logger defines a simple, structured logging interface. This allows users to inject
// their own logger (like slog, logrus, etc.) to get detailed diagnostics from the buffer.
type Logger interface {
	Debugf(format string, args ...any)
	Infof(format string, args ...any)
	Errorf(format string, args ...any)
}

// EventType defines the kind of notable event emitted by the buffer.
type EventType string

const (
	EventFramesDropped      EventType = "FramesDropped"
	EventInterpolationBegan EventType = "InterpolationBegan"
	EventDuplicationBegan   EventType = "DuplicationBegan"
	EventBufferingComplete  EventType = "BufferingComplete"
	EventFlushBegan         EventType = "FlushBegan"
	EventFlushComplete      EventType = "FlushComplete"
	EventDynamicDelay       EventType = "DynamicDelay"
	EventDrainComplete      EventType = "DrainComplete"
)

// Event is a notification sent by the buffer on state changes, allowing for
// programmatic reaction to the buffer's behavior.
type Event struct {
	Type      EventType
	Timestamp time.Time
	Metadata  map[string]any
}

// --- Metrics and Configuration ---

// Metrics holds performance counters for the Buffer, providing insight into its operation.
type Metrics struct {
	FramesIn           uint64 // Total frames received by the buffer.
	FramesOut          uint64 // Total frames sent from the buffer (real, duplicated, or interpolated).
	FramesDropped      uint64 // Total frames dropped due to rate limiting or being stale.
	FramesDuplicated   uint64 // Total frames duplicated to meet FPS.
	FramesInterpolated uint64 // Total frames synthetically created to meet FPS.
	CurrentBufferSize  int    // The number of frames currently held in the internal buffer.
}

// --- Buffer Implementation ---

// Buffer is a high-performance, robust frame buffer designed to smooth out, re-time,
// and manage a stream of frames for a consistent FPS output.
type Buffer[T Frame] struct {
	mu     sync.Mutex
	frames []T
	in     chan T
	out    chan T
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Configuration
	fps                 time.Duration
	bufferDelay         int
	useDynamicDelay     bool
	staleFrameTolerance time.Duration
	easingFunc          func(float64) float64
	logger              Logger
	eventCh             chan<- Event
	externalClock       <-chan time.Time

	// Dynamic State
	playbackSpeed          float64
	firstFrameOriginalTime time.Time
	firstFrameWallTime     time.Time
	isBuffering            bool
	isAnalyzing            bool
	lastSentFrame          T
	hasSentFirstFrame      bool
	nextKeepTime           time.Time
	metrics                Metrics
	jitterMonitor          *jitterMonitor
	setFPSCh               chan uint
	setSpeedCh             chan float64
	flushCh                chan chan struct{}
	drainCh                chan chan struct{}
}

// WithDelay sets a manual number of frames to buffer before output begins.
// This creates an initial delay to prevent stuttering on startup.
func WithDelay[T Frame](frames int) func(*Buffer[T]) {
	return func(b *Buffer[T]) {
		if frames > 0 {
			b.bufferDelay = frames
			b.isBuffering = true
		}
	}
}

// WithDynamicDelay enables the buffer to analyze stream jitter to automatically
// determine the optimal startup delay. It will begin playback once the stream is stable.
func WithDynamicDelay[T Frame]() func(*Buffer[T]) {
	return func(b *Buffer[T]) {
		if b.bufferDelay == 0 {
			b.useDynamicDelay = true
			b.isAnalyzing = true
		}
	}
}

// WithStaleFrameTolerance sets how long a frame can be past its target presentation time
// before being dropped to maintain low latency. A positive value is more lenient.
func WithStaleFrameTolerance[T Frame](tolerance time.Duration) func(*Buffer[T]) {
	return func(b *Buffer[T]) { b.staleFrameTolerance = tolerance }
}

// WithEasing provides a function to transform the interpolation factor (e.g., for
// non-linear animations) before it's passed to a frame's Interpolate method.
func WithEasing[T Frame](easingFunc func(float64) float64) func(*Buffer[T]) {
	return func(b *Buffer[T]) { b.easingFunc = easingFunc }
}

// WithLogger injects a logger for receiving internal diagnostic messages.
func WithLogger[T Frame](logger Logger) func(*Buffer[T]) {
	return func(b *Buffer[T]) { b.logger = logger }
}

// WithEventChannel provides a channel for the buffer to emit structured events
// for programmatic monitoring and reaction.
func WithEventChannel[T Frame](eventCh chan<- Event) func(*Buffer[T]) {
	return func(b *Buffer[T]) { b.eventCh = eventCh }
}

// WithExternalClock slaves the buffer's timing to an external ticker, which is
// crucial for scenarios like audio/video synchronization.
func WithExternalClock[T Frame](clock <-chan time.Time) func(*Buffer[T]) {
	return func(b *Buffer[T]) { b.externalClock = clock }
}

// NewBuffer creates a new frame buffer with the specified output FPS and optional configurations.
func NewBuffer[T Frame](ctx context.Context, fps uint, opts ...func(*Buffer[T])) *Buffer[T] {
	parentCtx, cancel := context.WithCancel(ctx)
	b := &Buffer[T]{
		frames:        make([]T, 0),
		in:            make(chan T, 256),
		out:           make(chan T, 256),
		ctx:           parentCtx,
		cancel:        cancel,
		fps:           time.Second / time.Duration(fps),
		playbackSpeed: 1.0,
		setFPSCh:      make(chan uint),
		setSpeedCh:    make(chan float64),
		flushCh:       make(chan chan struct{}),
		drainCh:       make(chan chan struct{}),
		logger:        &noopLogger{},
	}
	for _, opt := range opts {
		opt(b)
	}

	if b.useDynamicDelay {
		b.jitterMonitor = newJitterMonitor(fps)
	}

	b.wg.Add(1)
	go b.process()
	return b
}

// AddFrame adds a new frame to the buffer's input channel for processing.
func (b *Buffer[T]) AddFrame(frame T) {
	b.in <- frame
}

// Frames returns the read-only output channel. The consumer goroutine will receive
// a steady stream of frames from this channel at the target FPS.
func (b *Buffer[T]) Frames() <-chan T {
	return b.out
}

// Metrics returns a snapshot of the buffer's performance counters.
func (b *Buffer[T]) Metrics() Metrics {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := b.metrics
	m.CurrentBufferSize = len(b.frames)
	return m
}

// SetFPS changes the output frame rate on the fly.
func (b *Buffer[T]) SetFPS(fps uint) {
	b.setFPSCh <- fps
}

// SetSpeed changes the playback speed on the fly.
// Speed > 1.0 is fast-forward, Speed < 1.0 is slow-motion.
func (b *Buffer[T]) SetSpeed(speed float64) {
	if speed <= 0 {
		speed = 1.0 // Prevent invalid speeds
	}
	b.setSpeedCh <- speed
}

// Flush drains all currently buffered frames immediately, ignoring the FPS limiter.
// This is a blocking call that returns once the flush is complete.
func (b *Buffer[T]) Flush() {
	doneCh := make(chan struct{})
	b.flushCh <- doneCh
	<-doneCh
}

// Drain signals that the producer is finished and waits for the buffer to become empty
// of real frames. The buffer continues to run at its target FPS during this time.
// This is a blocking call.
func (b *Buffer[T]) Drain() {
	doneCh := make(chan struct{})
	b.drainCh <- doneCh
	<-doneCh
}

// Close stops the buffer's processing goroutine and cleans up any remaining resources.
func (b *Buffer[T]) Close() {
	b.cancel()
	b.wg.Wait()
}

// --- Core Processing Loop and Helpers ---

// process is the main goroutine for the buffer. It manages the state machine,
// handles incoming frames, and drives the output clock.
func (b *Buffer[T]) process() {
	defer b.wg.Done()
	defer close(b.out)

	defer func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		cleanupFrames(b.frames)
		if b.hasSentFirstFrame {
			cleanupFrame(b.lastSentFrame)
		}
	}()

	var lastSentPTS time.Time
	var presentationClock <-chan time.Time
	var internalTicker *time.Ticker
	clockStarted := false
	drainingDoneCh := (chan struct{})(nil)

	if b.externalClock != nil {
		presentationClock = b.externalClock
	}

	for {
		if !clockStarted && !b.isAnalyzing && !b.isBuffering {
			b.mu.Lock()
			if len(b.frames) > 0 {
				b.logger.Infof("Buffer ready, starting presentation clock.")
				if b.externalClock == nil {
					internalTicker = time.NewTicker(b.fps)
					presentationClock = internalTicker.C
				}
				lastSentPTS = b.getAdjustedTime(b.frames[0])
				clockStarted = true
			}
			b.mu.Unlock()
		}

		var tickChan <-chan time.Time
		if clockStarted {
			tickChan = presentationClock
		}

		select {
		case <-b.ctx.Done():
			if internalTicker != nil {
				internalTicker.Stop()
			}
			return
		case newFPS := <-b.setFPSCh:
			if internalTicker != nil {
				internalTicker.Stop()
			}
			b.fps = time.Second / time.Duration(newFPS)
			if b.externalClock == nil {
				internalTicker = time.NewTicker(b.fps)
				presentationClock = internalTicker.C
				b.logger.Infof("FPS changed to %d", newFPS)
			}
		case newSpeed := <-b.setSpeedCh:
			b.logger.Infof("Playback speed changed to %.2f", newSpeed)
			b.playbackSpeed = newSpeed
		case doneCh := <-b.flushCh:
			b.executeFlush(doneCh)
		case doneCh := <-b.drainCh:
			b.logger.Infof("Drain signal received; will complete when buffer is empty.")
			drainingDoneCh = doneCh
		case frame, ok := <-b.in:
			if ok {
				b.handleIncomingFrame(frame)
			}
		case <-tickChan:
			if b.isAnalyzing || b.isBuffering {
				continue
			}
			b.handlePresentationTick(lastSentPTS)
			lastSentPTS = lastSentPTS.Add(b.fps) // Correctly and steadily advance the clock

			if drainingDoneCh != nil {
				b.mu.Lock()
				// Drain is complete when all frames that have come in have also been sent out.
				if b.metrics.FramesOut >= b.metrics.FramesIn {
					b.logger.Infof("Drain complete.")
					b.emitEvent(EventDrainComplete, nil)
					close(drainingDoneCh)
					drainingDoneCh = nil // Reset state
				}
				b.mu.Unlock()
			}
		}
	}
}

// handleIncomingFrame manages adding a new frame to the internal buffer. It handles
// rate-limiting and sorting.
func (b *Buffer[T]) handleIncomingFrame(frame T) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.metrics.FramesIn++
	if b.useDynamicDelay && b.isAnalyzing {
		b.jitterMonitor.addFrame(frame)
		if b.jitterMonitor.isStable() {
			b.isAnalyzing = false
			b.logger.Infof("Dynamic delay analysis complete, stream is stable. Starting playback.")
			b.emitEvent(EventDynamicDelay, map[string]any{"status": "stable"})
		}
	}

	// Initialize the rate-limiter pacer on the first frame.
	if b.nextKeepTime.IsZero() {
		b.nextKeepTime = b.getAdjustedTime(frame)
		b.logger.Debugf("First frame arrived, initializing keep-time pacer.")
	}

	adjustedTime := b.getAdjustedTime(frame)
	// Timestamp-aware dropping for high-FPS input.
	if adjustedTime.Before(b.nextKeepTime) {
		cleanupFrame(frame)
		b.metrics.FramesDropped++
		b.emitEvent(EventFramesDropped, map[string]any{"reason": "rate_limit"})
		return
	}

	// This frame is kept, so advance the pacer for the next valid time.
	b.nextKeepTime = b.nextKeepTime.Add(b.fps)
	b.frames = append(b.frames, frame)
	sort.Slice(b.frames, func(i, j int) bool {
		return b.getAdjustedTime(b.frames[i]).Before(b.getAdjustedTime(b.frames[j]))
	})

	// Check if the manual buffering delay has been met.
	if b.isBuffering && len(b.frames) >= b.bufferDelay {
		b.isBuffering = false
		b.logger.Infof("Initial buffering complete (%d frames).", len(b.frames))
		b.emitEvent(EventBufferingComplete, nil)
	}
}

// handlePresentationTick is the main dispatcher for each tick of the output clock.
// It decides whether to send a real, interpolated, or duplicated frame.
func (b *Buffer[T]) handlePresentationTick(lastPTS time.Time) {
	targetPTS := lastPTS.Add(b.fps)
	if b.trySendRealFrame(targetPTS) {
		return
	}
	if b.tryInterpolateFrame(targetPTS) {
		return
	}
	b.sendDuplicateFrame()
}

// trySendRealFrame attempts to send a real frame from the buffer if one is ready.
// It also handles dropping stale frames to maintain low latency.
func (b *Buffer[T]) trySendRealFrame(targetPTS time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.frames) == 0 {
		return false
	}

	// Drop stale frames that are older than the tolerance window.
	earliestAcceptableTime := targetPTS.Add(-b.staleFrameTolerance)
	for len(b.frames) > 1 && b.getAdjustedTime(b.frames[0]).Before(earliestAcceptableTime) {
		cleanupFrame(b.frames[0])
		b.metrics.FramesDropped++
		b.emitEvent(EventFramesDropped, map[string]any{"reason": "stale"})
		b.frames = b.frames[1:]
	}

	// Send a frame if its timestamp is at or before the target presentation time.
	if !b.getAdjustedTime(b.frames[0]).After(targetPTS) {
		frameToSend := b.frames[0]
		b.frames = b.frames[1:]

		if b.hasSentFirstFrame {
			cleanupFrame(b.lastSentFrame)
		}
		b.lastSentFrame = refFrame(frameToSend)
		b.hasSentFirstFrame = true
		b.out <- frameToSend
		b.metrics.FramesOut++
		return true
	}

	return false
}

// tryInterpolateFrame attempts to create a synthetic frame if the frame type supports it.
func (b *Buffer[T]) tryInterpolateFrame(targetPTS time.Time) bool {
	b.mu.Lock()
	if !b.hasSentFirstFrame || len(b.frames) == 0 {
		b.mu.Unlock()
		return false
	}

	frameA := b.lastSentFrame
	frameB := b.frames[0]
	b.mu.Unlock()

	if interpolator, ok := any(frameA).(Interpolator[T]); ok {
		adjustedA := b.getAdjustedTime(frameA)
		adjustedB := b.getAdjustedTime(frameB)
		timeRange := adjustedB.Sub(adjustedA)

		if timeRange > 0 {
			timeProgress := targetPTS.Sub(adjustedA)
			factor := float64(timeProgress) / float64(timeRange)
			if factor >= 0 && factor < 1.0 { // Interpolate within the valid range.
				if b.easingFunc != nil {
					factor = b.easingFunc(factor)
				}
				b.emitEvent(EventInterpolationBegan, map[string]any{"factor": factor})
				syntheticFrame := interpolator.Interpolate(frameB, factor)
				b.metrics.FramesInterpolated++
				b.metrics.FramesOut++
				b.out <- syntheticFrame
				return true
			}
		}
	}
	return false
}

// sendDuplicateFrame is the fallback action, repeating the last successful frame.
func (b *Buffer[T]) sendDuplicateFrame() {
	if !b.hasSentFirstFrame {
		return
	}
	b.emitEvent(EventDuplicationBegan, nil)
	duplicateFrame := refFrame(b.lastSentFrame)
	b.metrics.FramesDuplicated++
	b.metrics.FramesOut++
	b.out <- duplicateFrame
}

// executeFlush sends all buffered frames immediately and blocks until complete.
func (b *Buffer[T]) executeFlush(doneCh chan struct{}) {
	defer close(doneCh)
	b.mu.Lock()
	b.logger.Infof("Flushing %d frames from buffer.", len(b.frames))
	b.emitEvent(EventFlushBegan, map[string]any{"count": len(b.frames)})

	framesToFlush := b.frames
	b.frames = make([]T, 0)
	b.mu.Unlock()

	for _, frame := range framesToFlush {
		select {
		case <-b.ctx.Done():
			b.logger.Infof("Flush interrupted by context cancellation.")
			cleanupFrame(frame)
			return
		case b.out <- frame:
			b.mu.Lock()
			if b.hasSentFirstFrame {
				cleanupFrame(b.lastSentFrame)
			}
			b.lastSentFrame = refFrame(frame)
			b.hasSentFirstFrame = true
			b.metrics.FramesOut++
			b.mu.Unlock()
		}
	}
	b.emitEvent(EventFlushComplete, nil)
}

// --- Utility and Helper Implementations ---

// getAdjustedTime calculates the effective timestamp of a frame based on the current playback speed.
func (b *Buffer[T]) getAdjustedTime(frame T) time.Time {
	if b.playbackSpeed == 1.0 {
		return frame.CreatedTime()
	}

	// Anchor the timeline to the very first frame received.
	if b.firstFrameOriginalTime.IsZero() {
		b.firstFrameOriginalTime = frame.CreatedTime()
		b.firstFrameWallTime = frame.CreatedTime() // Or time.Now() if you want to anchor to wall time
	}

	originalDelta := frame.CreatedTime().Sub(b.firstFrameOriginalTime)
	scaledDelta := time.Duration(float64(originalDelta) / b.playbackSpeed)
	return b.firstFrameWallTime.Add(scaledDelta)
}

func (b *Buffer[T]) emitEvent(eventType EventType, metadata map[string]any) {
	if b.eventCh == nil {
		return
	}
	event := Event{Type: eventType, Timestamp: time.Now(), Metadata: metadata}
	select {
	case b.eventCh <- event:
	default: // Drop event if the channel is full to prevent blocking.
	}
}

type noopLogger struct{}

func (l *noopLogger) Debugf(format string, args ...any) {}
func (l *noopLogger) Infof(format string, args ...any)  {}
func (l *noopLogger) Errorf(format string, args ...any) {}

// jitterMonitor analyzes the stability of the incoming frame stream.
type jitterMonitor struct {
	lastArrival time.Time
	deltas      []float64
	minSamples  int
	maxSamples  int
	threshold   float64
}

// newJitterMonitor creates a jitter monitor with thresholds relative to the target FPS.
func newJitterMonitor(fps uint) *jitterMonitor {
	if fps == 0 {
		fps = 30 // Fallback to a sensible default
	}
	frameIntervalMs := 1000.0 / float64(fps)
	return &jitterMonitor{
		minSamples: int(fps),               // Analyze at least 1 second of frames.
		maxSamples: int(fps * 4),           // Keep a rolling window of the last 4 seconds.
		threshold:  frameIntervalMs * 0.20, // Jitter threshold is 20% of the frame interval.
	}
}

// addFrame records the time delta from the previous frame.
func (jm *jitterMonitor) addFrame(frame Frame) {
	if !jm.lastArrival.IsZero() {
		delta := frame.CreatedTime().Sub(jm.lastArrival).Seconds() * 1000 // ms
		jm.deltas = append(jm.deltas, delta)
		if len(jm.deltas) > jm.maxSamples {
			jm.deltas = jm.deltas[1:]
		}
	}
	jm.lastArrival = frame.CreatedTime()
}

// isStable calculates the standard deviation of frame arrival deltas
// and returns true if it's below the stability threshold.
func (jm *jitterMonitor) isStable() bool {
	if len(jm.deltas) < jm.minSamples {
		return false
	}
	mean := 0.0
	for _, d := range jm.deltas {
		mean += d
	}
	mean /= float64(len(jm.deltas))

	variance := 0.0
	for _, d := range jm.deltas {
		variance += math.Pow(d-mean, 2)
	}
	variance /= float64(len(jm.deltas))

	stdDev := math.Sqrt(variance)
	return stdDev <= jm.threshold
}

// refFrame safely calls the Ref method if the frame implements ManagedItem.
func refFrame[T any](frame T) T {
	if item, ok := any(frame).(ManagedItem[T]); ok {
		return item.Ref()
	}
	return frame
}

// cleanupFrame safely calls the Cleanup method if the frame implements ManagedItem.
func cleanupFrame[T any](frame T) {
	if item, ok := any(frame).(ManagedItem[T]); ok {
		item.Cleanup()
	}
}

// cleanupFrames iterates over a slice and cleans up each frame.
func cleanupFrames[T any](frames []T) {
	for _, frame := range frames {
		cleanupFrame(frame)
	}
}
