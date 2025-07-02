/*
Package framebuffer provides a high-performance, concurrent-safe buffer for managing
and re-timing streams of frames to a constant FPS output.

It is designed for real-time video processing, streaming, or any application that
needs to convert a variable-rate input stream into a smooth, fixed-rate output.

# Key Features

  - FPS Management: Consumes frames from a producer and delivers them to a
    consumer at a steady, configurable frame rate.
  - Playback Speed Control: The playback speed can be adjusted on the fly to create
    slow-motion or fast-forward effects using the SetSpeed method.
  - Timestamp-Aware Dropping: Intelligently drops excess frames from high-FPS
    sources based on their CreatedTime to ensure the smoothest possible output.
  - Frame Duplication & Interpolation: Automatically fills gaps in the stream by
    duplicating the last valid frame or, if the frame type supports it, by
    synthetically generating interpolated frames.
  - Resource Management: Optional support for reference-counted frames via the
    ManagedItem interface, ensuring resources are cleaned up correctly when
    frames are dropped or the buffer is closed.
  - Dynamic Configuration: The output FPS can be changed on the fly.
  - Observability: Exposes detailed metrics and a structured event stream for
    monitoring and reacting to the buffer's internal state.
  - Flexible Startup: Offers both manual and dynamic startup delays to prevent
    stuttering when a stream begins.

# Basic Usage

A producer goroutine adds frames to the buffer, and a consumer goroutine receives
them from the output channel.

	// A simple frame type for the example.
	type MyFrame struct {
		Timestamp time.Time
	}

	func (f *MyFrame) CreatedTime() time.Time {
		return f.Timestamp
	}

	func main() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Create a buffer to output at 30 FPS.
		// Use WithDynamicDelay to let the buffer decide when to start playback.
		b := framebuffer.NewBuffer[*MyFrame](ctx, 30, framebuffer.WithDynamicDelay[*MyFrame]())
		defer b.Close()

		// Producer goroutine
		go func() {
			for {
				// In a real application, you would get a frame from a source.
				frame := &MyFrame{Timestamp: time.Now()}
				b.AddFrame(frame)
				time.Sleep(16 * time.Millisecond) // Simulate ~60 FPS input
			}
		}()

		// Consumer goroutine
		for outFrame := range b.Frames() {
			fmt.Printf("Processing frame created at: %v\n", outFrame.CreatedTime())
			// In a real application, you would encode or display the frame here.
		}
	}
*/
package framebuffer
