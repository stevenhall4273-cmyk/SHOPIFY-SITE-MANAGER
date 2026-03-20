package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

// ChromePool manages a pool of persistent headless Chrome allocators.
// Each allocator is a separate Chrome process that can host multiple tabs.
type ChromePool struct {
	allocators []context.Context
	cancels    []context.CancelFunc
	mu         sync.Mutex
	busy       bool // true while a batch is running
}

// NewChromePool creates n headless Chrome processes.
func NewChromePool(n int) *ChromePool {
	pool := &ChromePool{
		allocators: make([]context.Context, n),
		cancels:    make([]context.CancelFunc, n),
	}

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

	for i := 0; i < n; i++ {
		allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
		pool.allocators[i] = allocCtx
		pool.cancels[i] = allocCancel

		// Warm up each allocator by creating and closing a tab
		tabCtx, tabCancel := chromedp.NewContext(allocCtx)
		chromedp.Run(tabCtx, chromedp.Navigate("about:blank"))
		tabCancel()

		fmt.Printf("Chrome process %d/%d ready\n", i+1, n)
	}

	return pool
}

// Close shuts down all Chrome processes.
func (p *ChromePool) Close() {
	for _, cancel := range p.cancels {
		cancel()
	}
}

// RunBatch runs all card entries in parallel across the pool.
// Cards are distributed round-robin across allocators.
// Each allocator runs its assigned cards concurrently (one tab per card).
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

	// Distribute cards round-robin across allocators
	numAlloc := len(p.allocators)
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

			// Per-tab timeout
			tabCtx, timeoutCancel := context.WithTimeout(tabCtx, 120*time.Second)
			defer timeoutCancel()

			results[idx] = runCheckout(tabCtx, e, buyer)
			fmt.Printf("[%d/%s] DONE — %s (%s) in %.1fs\n",
				e.Index, e.Card.Last4, results[idx].Status, results[idx].ErrorCode, results[idx].ElapsedSeconds)
		}(i, entry, p.allocators[allocIdx])
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
