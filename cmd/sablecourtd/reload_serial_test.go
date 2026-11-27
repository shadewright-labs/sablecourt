package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Reloads run one at a time: a second caller waits until the first has
// applied its bundle and policy to every consumer.
func TestSerializedReloadsNeverOverlap(t *testing.T) {
	var running, overlaps, calls atomic.Int32
	reload := serialized(func() error {
		if running.Add(1) > 1 {
			overlaps.Add(1)
		}
		calls.Add(1)
		time.Sleep(5 * time.Millisecond)
		running.Add(-1)
		return nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = reload()
		}()
	}
	wg.Wait()
	if n := overlaps.Load(); n != 0 {
		t.Fatalf("%d reloads overlapped another", n)
	}
	if n := calls.Load(); n != 8 {
		t.Fatalf("%d reloads ran, want 8", n)
	}
}
