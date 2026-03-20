package main

import (
	"fmt"
	"log"
	"time"
)

// SiteCheckWorker continuously pulls pending sites from the DB,
// checks them using the Chrome pool, and writes results back.
type SiteCheckWorker struct {
	db        *DB
	pool      *ChromePool
	batchSize int
}

// NewSiteCheckWorker creates a background worker that checks sites.
// batchSize controls how many sites to check per tick (should match pool size).
func NewSiteCheckWorker(db *DB, pool *ChromePool, batchSize int) *SiteCheckWorker {
	if batchSize <= 0 {
		batchSize = 3
	}
	return &SiteCheckWorker{db: db, pool: pool, batchSize: batchSize}
}

// Run starts the worker loop. Call in a goroutine.
func (w *SiteCheckWorker) Run(stop <-chan struct{}) {
	log.Println("[worker] Site check worker started")

	// Reset any sites stuck in "checking" from previous crashes
	if n, err := w.db.ResetStuckChecking(); err == nil && n > 0 {
		log.Printf("[worker] Reset %d stuck sites back to pending", n)
	}

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			log.Println("[worker] Shutting down")
			return
		case <-ticker.C:
			w.processBatch()
		}
	}
}

func (w *SiteCheckWorker) processBatch() {
	// Reset stuck sites periodically
	if n, err := w.db.ResetStuckChecking(); err == nil && n > 0 {
		log.Printf("[worker] Reset %d stuck sites", n)
	}

	sites, err := w.db.ClaimPendingSites(w.batchSize)
	if err != nil {
		log.Printf("[worker] Error claiming sites: %v", err)
		return
	}
	if len(sites) == 0 {
		return
	}

	log.Printf("[worker] Checking %d sites...", len(sites))

	// Use a test card that will always fail with INCORRECT_NUMBER on working sites.
	// This is the whole point — INCORRECT_NUMBER means the site's checkout is alive.
	testCard := "4111111111111111|12|28|123"

	buyer := DefaultBuyer()

	// Build card entries — one per site
	entries := make([]CardEntry, len(sites))
	for i, site := range sites {
		raw := fmt.Sprintf("%s|%s", testCard, site.URL)
		entry, err := ParseCardEntry(raw, i)
		if err != nil {
			log.Printf("[worker] Error parsing entry for %s: %v", site.URL, err)
			w.db.UpdateSiteResult(site.ID, StatusError, "PARSE_ERROR", err.Error(), 0)
			continue
		}
		entry.Card.FullName = buyer.FirstName + " " + buyer.LastName
		entries[i] = entry
	}

	// Run batch through the Chrome pool
	results, err := w.pool.RunBatch(entries, buyer, perStoreDefaultLimit)
	if err != nil {
		log.Printf("[worker] Batch error: %v", err)
		// Put sites back to pending
		for _, site := range sites {
			w.db.UpdateSiteResult(site.ID, StatusError, "BATCH_ERROR", err.Error(), 0)
		}
		return
	}

	// Process results
	for i, r := range results {
		site := sites[i]

		switch {
		case r.ErrorCode == "INCORRECT_NUMBER":
			// WORKING — the checkout is live, it tried to charge the card
			log.Printf("[worker] ✅ WORKING: %s ($%.2f)", site.URL, r.CheckoutPrice)
			w.db.UpdateSiteResult(site.ID, StatusWorking, r.ErrorCode, r.ErrorMessage, r.CheckoutPrice)

		case r.ErrorCode == "CHECKOUT_FAILED" || r.ErrorCode == "PAYMENT_FILL_FAILED":
			// Site is dead or broken
			log.Printf("[worker] ❌ DEAD: %s (%s)", site.URL, r.ErrorCode)
			w.db.UpdateSiteResult(site.ID, StatusDead, r.ErrorCode, r.ErrorMessage, r.CheckoutPrice)

		case r.Status == "error" && site.CheckCount < 2:
			// Transient error — retry later
			log.Printf("[worker] ⚠️ ERROR (will retry): %s (%s)", site.URL, r.ErrorCode)
			w.db.UpdateSiteResult(site.ID, StatusError, r.ErrorCode, r.ErrorMessage, r.CheckoutPrice)

		case r.Status == "declined" && r.ErrorCode != "INCORRECT_NUMBER":
			// Some other decline — checkout works but different error
			// Still means the site is alive enough to reach payment
			log.Printf("[worker] ✅ WORKING (declined): %s ($%.2f) (%s)", site.URL, r.CheckoutPrice, r.ErrorCode)
			w.db.UpdateSiteResult(site.ID, StatusWorking, r.ErrorCode, r.ErrorMessage, r.CheckoutPrice)

		default:
			// Any other outcome — mark dead after retries exhausted
			if site.CheckCount >= 2 {
				log.Printf("[worker] ❌ DEAD (max retries): %s (%s)", site.URL, r.ErrorCode)
				w.db.UpdateSiteResult(site.ID, StatusDead, r.ErrorCode, r.ErrorMessage, r.CheckoutPrice)
			} else {
				log.Printf("[worker] ⚠️ UNKNOWN (retrying): %s (%s: %s)", site.URL, r.ErrorCode, r.ErrorMessage)
				w.db.UpdateSiteResult(site.ID, StatusError, r.ErrorCode, r.ErrorMessage, r.CheckoutPrice)
			}
		}
	}
}
