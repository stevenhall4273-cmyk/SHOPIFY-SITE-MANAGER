package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

// ChromePool manages on-demand headless Chrome allocators.
// Chrome processes are created per-batch and closed after to prevent zombie buildup.
type ChromePool struct {
	size int
	opts []chromedp.ExecAllocatorOption
	mu   sync.Mutex
	busy bool // true while a batch is running
}

// NewChromePool creates a pool config for n concurrent Chrome processes.
// Chrome processes are created on demand per batch, not prewarmed.
func NewChromePool(n int) *ChromePool {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-images", true),     // save RAM
		chromedp.Flag("disable-extensions", true), // save RAM
		chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("metrics-recording-only", true),
		chromedp.Flag("no-first-run", true),
		chromedp.WindowSize(1366, 768),
	)

	fmt.Printf("Chrome pool configured for %d concurrent processes (on-demand)\n", n)
	return &ChromePool{size: n, opts: opts}
}

// Close is a no-op since Chrome processes are now created/destroyed per batch.
func (p *ChromePool) Close() {}

// RunBatch creates fresh Chrome process(es), runs all entries, then cleans up.
// Returns results in the same order as the input.
func (p *ChromePool) RunBatch(entries []CardEntry, buyer BuyerInfo, perStoreLimit int) ([]CheckResult, error) {
	p.mu.Lock()
	if p.busy {
		p.mu.Unlock()
		return nil, fmt.Errorf("batch already in progress")
	}
	p.busy = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.busy = false
		p.mu.Unlock()
	}()

	n := len(entries)
	results := make([]CheckResult, n)

	// Per-store concurrency limiter
	storeSem := &storeSemaphore{
		limit: perStoreLimit,
		sems:  make(map[string]chan struct{}),
	}

	// Create fresh Chrome allocators for this batch
	numAlloc := p.size
	if numAlloc > n {
		numAlloc = n
	}
	allocators := make([]context.Context, numAlloc)
	cancels := make([]context.CancelFunc, numAlloc)
	for i := 0; i < numAlloc; i++ {
		allocators[i], cancels[i] = chromedp.NewExecAllocator(context.Background(), p.opts...)
	}
	// Always clean up Chrome processes after batch
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()

	var wg sync.WaitGroup

	for i, entry := range entries {
		wg.Add(1)
		allocIdx := i % numAlloc
		entry.Card.FullName = buyer.FirstName + " " + buyer.LastName

		go func(idx int, e CardEntry, allocCtx context.Context) {
			defer wg.Done()

			// Acquire per-store semaphore
			storeSem.Acquire(e.Store)
			defer storeSem.Release(e.Store)

			// Create a new browser tab in this allocator
			tabCtx, tabCancel := chromedp.NewContext(allocCtx)
			defer tabCancel()

			// Per-tab timeout (keep short — dead sites waste time)
			tabCtx, timeoutCancel := context.WithTimeout(tabCtx, 45*time.Second)
			defer timeoutCancel()

			results[idx] = runCheckout(tabCtx, e, buyer)
			fmt.Printf("[%d/%s] DONE — %s (%s) in %.1fs\n",
				e.Index, e.Card.Last4, results[idx].Status, results[idx].ErrorCode, results[idx].ElapsedSeconds)
		}(i, entry, allocators[allocIdx])
	}

	wg.Wait()
	return results, nil
}

// storeSemaphore limits concurrent access per store domain.
type storeSemaphore struct {
	mu    sync.Mutex
	limit int
	sems  map[string]chan struct{}
}

func (s *storeSemaphore) getSem(store string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.sems[store]; ok {
		return ch
	}
	ch := make(chan struct{}, s.limit)
	s.sems[store] = ch
	return ch
}

func (s *storeSemaphore) Acquire(store string) {
	s.getSem(store) <- struct{}{}
}

func (s *storeSemaphore) Release(store string) {
	<-s.getSem(store)
}
