package framebuffer

import (
	"context"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- Test Frame Implementations ---

// TestFrame is a basic implementation of the Frame interface for testing.
type TestFrame struct {
	id          int
	createdTime time.Time
}

func (f *TestFrame) CreatedTime() time.Time {
	return f.createdTime
}

// TestManagedFrame is a full implementation for testing all features.
type TestManagedFrame struct {
	id          int
	createdTime time.Time
	refCount    int32
}

func (f *TestManagedFrame) CreatedTime() time.Time {
	return f.createdTime
}

func (f *TestManagedFrame) Ref() *TestManagedFrame {
	atomic.AddInt32(&f.refCount, 1)
	return f
}

func (f *TestManagedFrame) Cleanup() {
	atomic.AddInt32(&f.refCount, -1)
}

func (f *TestManagedFrame) Interpolate(other *TestManagedFrame, factor float64) *TestManagedFrame {
	return &TestManagedFrame{
		id:          -1, // Indicates an interpolated frame
		createdTime: f.createdTime.Add(time.Duration(float64(other.createdTime.Sub(f.createdTime)) * factor)),
		refCount:    1,
	}
}

// --- Test Logger ---

type testLogger struct {
	t *testing.T
}

func (l *testLogger) Debugf(format string, args ...any) { l.t.Logf("DEBUG: "+format, args...) }
func (l *testLogger) Infof(format string, args ...any)  { l.t.Logf("INFO: "+format, args...) }
func (l *testLogger) Errorf(format string, args ...any) { l.t.Errorf("ERROR: "+format, args...) }

// --- Test Suite ---

func TestNewBuffer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := NewBuffer[*TestFrame](ctx, 30)
	if b == nil {
		t.Fatal("NewBuffer returned nil")
	}
	b.Close()
}

func TestAddAndReceiveFrames(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	b := NewBuffer[*TestFrame](ctx, 10)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			b.AddFrame(&TestFrame{id: i, createdTime: time.Now()})
			time.Sleep(50 * time.Millisecond)
		}
	}()

	receivedCount := 0
	consumeDone := make(chan struct{})
	go func() {
		for range b.Frames() {
			receivedCount++
			if receivedCount == 5 {
				break
			}
		}
		close(consumeDone)
	}()

	wg.Wait()
	<-consumeDone

	b.Close()

	if receivedCount != 5 {
		t.Errorf("Expected to receive 5 frames, got %d", receivedCount)
	}
}

func TestTimestampAwareDropping(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	b := NewBuffer[*TestManagedFrame](ctx, 10)

	var producerWg sync.WaitGroup
	producerWg.Add(1)
	go func() {
		defer producerWg.Done()
		startTime := time.Now()
		for i := 0; i < 50; i++ {
			select {
			case <-ctx.Done():
				return
			default:
				b.AddFrame(&TestManagedFrame{id: i, createdTime: startTime.Add(time.Duration(i*10) * time.Millisecond), refCount: 1})
			}
		}
	}()

	var consumerWg sync.WaitGroup
	consumerWg.Add(1)
	var receivedCount atomic.Int32
	go func() {
		defer consumerWg.Done()
		for range b.Frames() {
			receivedCount.Add(1)
		}
	}()

	producerWg.Wait()
	time.Sleep(1100 * time.Millisecond)

	b.Close()
	consumerWg.Wait()

	rc := receivedCount.Load()
	if rc < 9 || rc > 13 {
		t.Errorf("Expected around 11 frames (1.1s @ 10fps), but got %d", rc)
	}
	if b.Metrics().FramesDropped == 0 {
		t.Error("Expected frames to be dropped")
	}
}

func TestFrameDuplication(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	b := NewBuffer[*TestManagedFrame](ctx, 30)

	var producerWg sync.WaitGroup
	producerWg.Add(1)
	go func() {
		defer producerWg.Done()
		b.AddFrame(&TestManagedFrame{id: 1, createdTime: time.Now(), refCount: 1})
		time.Sleep(200 * time.Millisecond)
		b.AddFrame(&TestManagedFrame{id: 2, createdTime: time.Now(), refCount: 1})
	}()

	var consumerWg sync.WaitGroup
	consumerWg.Add(1)
	var receivedCount atomic.Int32
	go func() {
		defer consumerWg.Done()
		for range b.Frames() {
			receivedCount.Add(1)
		}
	}()

	producerWg.Wait()
	time.Sleep(300 * time.Millisecond)

	b.Close()
	consumerWg.Wait()

	rc := receivedCount.Load()
	if rc < 13 || rc > 17 {
		t.Errorf("Expected around 15 frames (500ms @ 30fps), got %d", rc)
	}
	if b.Metrics().FramesDuplicated == 0 {
		t.Error("Expected frames to be duplicated")
	}
}

func TestFrameInterpolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	b := NewBuffer[*TestManagedFrame](ctx, 30)

	var consumerWg sync.WaitGroup
	consumerWg.Add(1)
	var interpolatedCount atomic.Int32
	var firstFrameReceived sync.WaitGroup
	firstFrameReceived.Add(1)

	go func() {
		defer consumerWg.Done()
		for f := range b.Frames() {
			if f.id == 1 {
				firstFrameReceived.Done()
			}
			if f.id == -1 {
				interpolatedCount.Add(1)
			}
		}
	}()

	startTime := time.Now()
	b.AddFrame(&TestManagedFrame{id: 1, createdTime: startTime, refCount: 1})
	firstFrameReceived.Wait() // Ensure the first frame is processed
	b.AddFrame(&TestManagedFrame{id: 2, createdTime: startTime.Add(200 * time.Millisecond), refCount: 1})

	time.Sleep(250 * time.Millisecond)

	b.Close()
	consumerWg.Wait()

	ic := interpolatedCount.Load()
	if ic == 0 {
		t.Fatal("Expected frames to be interpolated, but none were.")
	}
}

func TestSetFPS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	b := NewBuffer[*TestFrame](ctx, 10)

	var producerWg sync.WaitGroup
	producerWg.Add(1)
	go func() {
		defer producerWg.Done()
		for i := 0; i < 40; i++ {
			b.AddFrame(&TestFrame{id: i, createdTime: time.Now()})
			time.Sleep(20 * time.Millisecond)
		}
	}()

	var consumerWg sync.WaitGroup
	consumerWg.Add(1)
	var receivedCount1, receivedCount2 atomic.Int32
	var switched atomic.Bool

	go func() {
		defer consumerWg.Done()
		for range b.Frames() {
			if !switched.Load() {
				receivedCount1.Add(1)
			} else {
				receivedCount2.Add(1)
			}
		}
	}()

	time.Sleep(550 * time.Millisecond)
	switched.Store(true)
	b.SetFPS(50)
	time.Sleep(550 * time.Millisecond)

	producerWg.Wait()
	b.Close()
	consumerWg.Wait()

	rc1 := receivedCount1.Load()
	if rc1 < 4 || rc1 > 7 {
		t.Errorf("Expected ~5 frames at 10 FPS, got %d", rc1)
	}

	rc2 := receivedCount2.Load()
	if rc2 < 23 || rc2 > 28 {
		t.Errorf("Expected ~25 frames at 50 FPS, got %d", rc2)
	}
}

func TestFlush(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	b := NewBuffer[*TestFrame](ctx, 60)

	var producerWg sync.WaitGroup
	producerWg.Add(1)
	go func() {
		defer producerWg.Done()
		startTime := time.Now()
		for i := 0; i < 100; i++ {
			// Create frames with correctly paced timestamps so they aren't dropped.
			frameTime := startTime.Add(time.Duration(i) * b.fps)
			b.AddFrame(&TestFrame{id: i, createdTime: frameTime})
		}
	}()
	producerWg.Wait()

	// Poll until the buffer has ingested all frames.
	for {
		if b.Metrics().FramesIn == 100 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("Test timed out waiting for frames to be ingested. Ingested: %d", b.Metrics().FramesIn)
		case <-time.After(10 * time.Millisecond):
		}
	}

	var receivedCount atomic.Int32
	var consumerWg sync.WaitGroup
	consumerWg.Add(1)
	go func() {
		defer consumerWg.Done()
		for range b.Frames() {
			receivedCount.Add(1)
		}
	}()

	b.Flush()
	b.Close()
	consumerWg.Wait()

	rc := receivedCount.Load()
	if rc != 100 {
		t.Errorf("Expected exactly 100 frames after flush, got %d", rc)
	}
}

func TestDrain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	b := NewBuffer[*TestFrame](ctx, 30)

	var producerWg sync.WaitGroup
	producerWg.Add(1)
	go func() {
		defer producerWg.Done()
		startTime := time.Now()
		for i := 0; i < 10; i++ {
			b.AddFrame(&TestFrame{id: i, createdTime: startTime.Add(time.Duration(i) * b.fps)})
		}
	}()
	producerWg.Wait()

	var receivedCount atomic.Int32
	var consumerWg sync.WaitGroup
	consumerWg.Add(1)
	go func() {
		defer consumerWg.Done()
		for range b.Frames() {
			receivedCount.Add(1)
		}
	}()

	b.Drain() // This will block until all 10 frames are sent

	if b.Metrics().CurrentBufferSize != 0 {
		t.Errorf("Expected buffer to be empty after drain, but size is %d", b.Metrics().CurrentBufferSize)
	}

	b.Close()
	consumerWg.Wait()

	rc := receivedCount.Load()
	if rc < 10 {
		t.Errorf("Expected at least 10 frames after drain, got %d", rc)
	}
}

func TestDynamicDelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	eventCh := make(chan Event, 10)
	logger := testLogger{t: t}
	b := NewBuffer[*TestFrame](ctx, 30, WithDynamicDelay[*TestFrame](), WithEventChannel[*TestFrame](eventCh), WithLogger[*TestFrame](&logger))
	defer b.Close()

	go func() {
		startTime := time.Now()
		for i := 0; i < 60; i++ {
			var jitter time.Duration
			if i < 20 {
				jitter = time.Duration(i%5*5) * time.Millisecond
			}
			b.AddFrame(&TestFrame{id: i, createdTime: startTime.Add(time.Duration(i*33)*time.Millisecond + jitter)})
			select {
			case <-time.After(33 * time.Millisecond):
			case <-ctx.Done():
				return
			}
		}
	}()

	timeout := time.After(2 * time.Second)
	var stabilityEvent Event
loop:
	for {
		select {
		case e := <-eventCh:
			if e.Type == EventDynamicDelay {
				stabilityEvent = e
				break loop
			}
		case <-timeout:
			t.Fatal("Timed out waiting for dynamic delay stability event")
		}
	}

	if stabilityEvent.Metadata["status"] != "stable" {
		t.Errorf("Expected stability event, got %+v", stabilityEvent)
	}
}

func TestStaleFrameDropping(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	b := NewBuffer[*TestFrame](ctx, 30, WithStaleFrameTolerance[*TestFrame](50*time.Millisecond))

	var framesReceived []*TestFrame
	var consumerWg sync.WaitGroup
	consumerWg.Add(1)
	go func() {
		defer consumerWg.Done()
		for f := range b.Frames() {
			framesReceived = append(framesReceived, f)
		}
	}()

	startTime := time.Now()
	for i := 0; i < 15; i++ {
		b.AddFrame(&TestFrame{id: i, createdTime: startTime.Add(time.Duration(i) * b.fps)})
	}
	time.Sleep(300 * time.Millisecond)
	b.AddFrame(&TestFrame{id: 99, createdTime: startTime.Add(300 * time.Millisecond)})
	time.Sleep(100 * time.Millisecond)

	b.Close()
	consumerWg.Wait()

	if len(framesReceived) == 0 {
		t.Fatal("Did not receive any frames")
	}
	if b.Metrics().FramesDropped == 0 {
		t.Error("Expected stale frames to be dropped, but none were.")
	}
}

func TestWithDelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	delayAmount := 10
	eventCh := make(chan Event, 1)
	b := NewBuffer[*TestFrame](ctx, 30, WithDelay[*TestFrame](delayAmount), WithEventChannel[*TestFrame](eventCh))
	defer b.Close()

	var receivedCount atomic.Int32
	var consumerWg sync.WaitGroup
	consumerWg.Add(1)
	go func() {
		defer consumerWg.Done()
		for range b.Frames() {
			receivedCount.Add(1)
		}
	}()

	// Send frames to meet the delay, with correctly paced timestamps
	startTime := time.Now()
	for i := 0; i < delayAmount; i++ {
		frameTime := startTime.Add(time.Duration(i) * b.fps)
		b.AddFrame(&TestFrame{id: i, createdTime: frameTime})
	}

	// Wait for the buffering complete event
	select {
	case e := <-eventCh:
		if e.Type != EventBufferingComplete {
			t.Fatalf("Expected EventBufferingComplete, got %s", e.Type)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Timed out waiting for buffering complete event")
	}

	// At this point, no frames should have been sent out yet.
	if rc := receivedCount.Load(); rc != 0 {
		t.Errorf("Expected 0 frames received during delay period, but got %d", rc)
	}

	// Send one more frame to trigger output
	b.AddFrame(&TestFrame{id: delayAmount, createdTime: time.Now()})

	// Poll until we receive at least one frame
	for {
		if receivedCount.Load() > 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("Test timed out waiting for frames to be sent after delay")
		case <-time.After(10 * time.Millisecond):
		}
	}

	b.Close()
	consumerWg.Wait()
}

func TestWithEasing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	eventCh := make(chan Event, 20)
	clock := make(chan time.Time)
	easingFunc := func(f float64) float64 { return 0.99 } // Return a constant for easy testing

	b := NewBuffer[*TestManagedFrame](ctx, 30,
		WithEasing[*TestManagedFrame](easingFunc),
		WithEventChannel[*TestManagedFrame](eventCh),
		WithExternalClock[*TestManagedFrame](clock),
	)
	defer b.Close()

	var consumerWg sync.WaitGroup
	consumerWg.Add(1)
	go func() {
		defer consumerWg.Done()
		// Drain the output channel to prevent blocking
		for range b.Frames() {
		}
	}()

	startTime := time.Now()
	// Add first frame and send it
	b.AddFrame(&TestManagedFrame{id: 1, createdTime: startTime, refCount: 1})
	clock <- time.Now()

	// Add second frame, creating the gap for interpolation
	b.AddFrame(&TestManagedFrame{id: 2, createdTime: startTime.Add(200 * time.Millisecond), refCount: 1})

	// Tick the clock to trigger interpolation
	clock <- time.Now()

	b.Close()
	consumerWg.Wait()

	// Check the events for the eased factor
	close(eventCh)
	foundEasedEvent := false
	for e := range eventCh {
		if e.Type == EventInterpolationBegan {
			if factor, ok := e.Metadata["factor"].(float64); ok {
				if factor == 0.99 {
					foundEasedEvent = true
					break
				}
			}
		}
	}

	if !foundEasedEvent {
		t.Error("Expected to find an interpolation event with the eased factor, but none was found")
	}
}

func TestWithExternalClock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	clock := make(chan time.Time)
	b := NewBuffer[*TestFrame](ctx, 30, WithExternalClock[*TestFrame](clock))
	defer b.Close()

	var receivedCount atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range b.Frames() {
			receivedCount.Add(1)
		}
	}()

	b.AddFrame(&TestFrame{id: 1, createdTime: time.Now()})

	// No frames should be received yet
	if rc := receivedCount.Load(); rc != 0 {
		t.Fatalf("Received frame before clock tick, expected 0 got %d", rc)
	}

	// Send a tick
	clock <- time.Now()

	// Poll until the frame is received
	for {
		if receivedCount.Load() == 1 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("Test timed out waiting for frame after clock tick")
		case <-time.After(10 * time.Millisecond):
		}
	}

	b.Close()
	wg.Wait()
}

func TestSetSpeed_SlowMotion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	b := NewBuffer[*TestManagedFrame](ctx, 30)
	defer b.Close()

	b.SetSpeed(0.5) // Half speed

	var consumerWg sync.WaitGroup
	consumerWg.Add(1)
	go func() {
		defer consumerWg.Done()
		// Drain output
		for range b.Frames() {
		}
	}()

	startTime := time.Now()
	b.AddFrame(&TestManagedFrame{id: 1, createdTime: startTime, refCount: 1})
	b.AddFrame(&TestManagedFrame{id: 2, createdTime: startTime.Add(100 * time.Millisecond), refCount: 1})

	// Wait for the buffer to process the stretched timeline
	time.Sleep(300 * time.Millisecond)
	b.Close()
	consumerWg.Wait()

	metrics := b.Metrics()
	// The 100ms gap becomes 200ms. At 30fps (~33ms interval), this requires duplication/interpolation.
	if metrics.FramesDuplicated == 0 && metrics.FramesInterpolated == 0 {
		t.Error("Expected frames to be duplicated or interpolated for slow motion, but none were")
	}
}

func TestSetSpeed_FastForward(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	b := NewBuffer[*TestFrame](ctx, 30)
	defer b.Close()

	b.SetSpeed(2.0) // Double speed

	var consumerWg sync.WaitGroup
	consumerWg.Add(1)
	go func() {
		defer consumerWg.Done()
		for range b.Frames() {
		}
	}()

	startTime := time.Now()
	// Send frames that would normally be kept, but are now too close together
	for i := 0; i < 10; i++ {
		b.AddFrame(&TestFrame{id: i, createdTime: startTime.Add(time.Duration(i*33) * time.Millisecond)})
	}

	time.Sleep(200 * time.Millisecond)
	b.Close()
	consumerWg.Wait()

	metrics := b.Metrics()
	if metrics.FramesDropped == 0 {
		t.Error("Expected frames to be dropped for fast-forward, but none were")
	}
}

func TestPresentationClockAdvancement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	clock := make(chan time.Time)
	// Use a low FPS to make the time gaps obvious
	b := NewBuffer[*TestManagedFrame](ctx, 10, WithExternalClock[*TestManagedFrame](clock))
	defer b.Close()

	var receivedFrames []*TestManagedFrame
	var consumerWg sync.WaitGroup
	consumerWg.Add(1)
	go func() {
		defer consumerWg.Done()
		for f := range b.Frames() {
			receivedFrames = append(receivedFrames, f)
		}
	}()

	startTime := time.Now()

	// 1. Add two frames to the buffer.
	b.AddFrame(&TestManagedFrame{id: 1, createdTime: startTime, refCount: 1})
	b.AddFrame(&TestManagedFrame{id: 2, createdTime: startTime.Add(500 * time.Millisecond), refCount: 1})

	// 2. Tick once to send the first frame.
	clock <- time.Now() // Tick 1

	// 3. Tick again. Now frame 1 is lastSent, and frame 2 is in the queue.
	// This is the condition where interpolation should happen.
	clock <- time.Now() // Tick 2

	// 4. Tick several more times to eventually process the second real frame.
	for i := 0; i < 5; i++ {
		clock <- time.Now()
	}

	b.Close()
	consumerWg.Wait()

	if len(receivedFrames) < 3 {
		t.Fatalf("Expected at least 3 frames, got %d", len(receivedFrames))
	}

	// Check the sequence
	// Frame 1 should be real
	if receivedFrames[0].id != 1 {
		t.Errorf("Expected first frame to be real (id 1), got id %d", receivedFrames[0].id)
	}
	// Frame 2 should be synthetic (interpolated)
	if receivedFrames[1].id != -1 {
		t.Errorf("Expected second frame to be synthetic (id -1), got id %d", receivedFrames[1].id)
	}
	// One of the later frames should be the second real frame
	foundSecondRealFrame := false
	for _, f := range receivedFrames {
		if f.id == 2 {
			foundSecondRealFrame = true
			break
		}
	}
	if !foundSecondRealFrame {
		t.Error("Expected to receive the second real frame (id 2), but it was not found")
	}
}

func TestInternalPresentationClockAdvancement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	b := NewBuffer[*TestManagedFrame](ctx, 10) // 100ms interval
	defer b.Close()

	var receivedFrames []*TestManagedFrame
	var consumerWg sync.WaitGroup
	var receivedFramesMutex sync.Mutex
	var firstFrameReceived, secondFrameReceived sync.WaitGroup
	firstFrameReceived.Add(1)
	secondFrameReceived.Add(1)

	consumerWg.Add(1)
	go func() {
		defer consumerWg.Done()
		for f := range b.Frames() {
			receivedFramesMutex.Lock()
			receivedFrames = append(receivedFrames, f)
			receivedFramesMutex.Unlock()
			if f.id == 1 {
				firstFrameReceived.Done()
			}
			if f.id == 2 {
				secondFrameReceived.Done()
			}
		}
	}()

	startTime := time.Now()
	// Add first frame
	b.AddFrame(&TestManagedFrame{id: 1, createdTime: startTime, refCount: 1})
	firstFrameReceived.Wait() // Wait until the first frame is processed

	// Add a second real frame far in the future
	b.AddFrame(&TestManagedFrame{id: 2, createdTime: startTime.Add(500 * time.Millisecond), refCount: 1})
	secondFrameReceived.Wait() // Wait until the second real frame is processed

	b.Close()
	consumerWg.Wait()

	receivedFramesMutex.Lock()
	defer receivedFramesMutex.Unlock()

	if len(receivedFrames) < 3 {
		t.Fatalf("Expected at least 3 frames, got %d", len(receivedFrames))
	}
	if receivedFrames[0].id != 1 {
		t.Errorf("Expected first frame to be real (id 1), got id %d", receivedFrames[0].id)
	}
	// The second frame should be synthetic because of the gap
	if receivedFrames[1].id != -1 {
		t.Errorf("Expected second frame to be synthetic (id -1), got id %d", receivedFrames[1].id)
	}
	foundSecondRealFrame := false
	for _, f := range receivedFrames {
		if f.id == 2 {
			foundSecondRealFrame = true
			break
		}
	}
	if !foundSecondRealFrame {
		t.Error("Expected to receive the second real frame (id 2), but it was not found")
	}
}

func TestResourceCleanupOnClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var frame *TestManagedFrame

	func() {
		b := NewBuffer[*TestManagedFrame](ctx, 30)
		frame = &TestManagedFrame{id: 1, createdTime: time.Now(), refCount: 1}
		b.AddFrame(frame)
		// Poll metrics to ensure the frame was ingested before closing
		for {
			if b.Metrics().FramesIn > 0 {
				break
			}
			time.Sleep(1 * time.Millisecond)
		}
		b.Close()
	}()

	if atomic.LoadInt32(&frame.refCount) != 0 {
		t.Errorf("Expected frame refCount to be 0 after close, but it was %d", frame.refCount)
	}
}

func ExampleWithLogger() {
	l := log.New(os.Stdout, "", 0)
	logger := &consoleLogger{l}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	b := NewBuffer[*TestFrame](ctx, 10, WithLogger[*TestFrame](logger))
	defer b.Close()

	b.AddFrame(&TestFrame{id: 1, createdTime: time.Now()})
	<-b.Frames()

	// The INFO log appears once the buffer has frames and starts its clock.
	// Corrected Output:
	// DEBUG: First frame arrived, initializing keep-time pacer.
	// INFO: Buffer ready, starting presentation clock.
}

type consoleLogger struct{ l *log.Logger }

func (l *consoleLogger) Debugf(format string, args ...any) { l.l.Printf("DEBUG: "+format, args...) }
func (l *consoleLogger) Infof(format string, args ...any)  { l.l.Printf("INFO: "+format, args...) }
func (l *consoleLogger) Errorf(format string, args ...any) { l.l.Printf("ERROR: "+format, args...) }
