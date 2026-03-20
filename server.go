package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// maxCardsPerRequest is the hard limit on cards in a single POST /check.
const maxCardsPerRequest = 100

// perStoreDefaultLimit caps concurrent checks against a single Shopify store.
const perStoreDefaultLimit = 5

// StartServer starts the HTTP API server on the given address.
// db may be nil if DATABASE_URL is not configured (site management disabled).
func StartServer(addr string, pool *ChromePool, db *DB) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/check", func(w http.ResponseWriter, r *http.Request) {
		handleCheck(w, r, pool)
	})

	// Register site management routes if DB is available
	if db != nil {
		RegisterSiteRoutes(mux, db)
		log.Println("Site management API enabled")
	}

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 300 * time.Second, // long — batch can take minutes
		IdleTimeout:  120 * time.Second,
	}

	log.Printf("Server listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func handleCheck(w http.ResponseWriter, r *http.Request, pool *ChromePool) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req CheckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"invalid JSON: %s"}`, err.Error()), http.StatusBadRequest)
		return
	}

	if len(req.Cards) == 0 {
		http.Error(w, `{"error":"cards array is empty"}`, http.StatusBadRequest)
		return
	}
	if len(req.Cards) > maxCardsPerRequest {
		http.Error(w, fmt.Sprintf(`{"error":"max %d cards per request"}`, maxCardsPerRequest), http.StatusBadRequest)
		return
	}

	// Use provided buyer or defaults
	buyer := DefaultBuyer()
	if req.Buyer != nil {
		if req.Buyer.Email != "" {
			buyer.Email = req.Buyer.Email
		}
		if req.Buyer.FirstName != "" {
			buyer.FirstName = req.Buyer.FirstName
		}
		if req.Buyer.LastName != "" {
			buyer.LastName = req.Buyer.LastName
		}
		if req.Buyer.Address1 != "" {
			buyer.Address1 = req.Buyer.Address1
		}
		if req.Buyer.Address2 != "" {
			buyer.Address2 = req.Buyer.Address2
		}
		if req.Buyer.City != "" {
			buyer.City = req.Buyer.City
		}
		if req.Buyer.State != "" {
			buyer.State = req.Buyer.State
		}
		if req.Buyer.StateName != "" {
			buyer.StateName = req.Buyer.StateName
		}
		if req.Buyer.Zip != "" {
			buyer.Zip = req.Buyer.Zip
		}
		if req.Buyer.Country != "" {
			buyer.Country = req.Buyer.Country
		}
		if req.Buyer.CountryName != "" {
			buyer.CountryName = req.Buyer.CountryName
		}
		if req.Buyer.Phone != "" {
			buyer.Phone = req.Buyer.Phone
		}
	}

	// Parse card entries
	entries := make([]CardEntry, 0, len(req.Cards))
	for i, raw := range req.Cards {
		entry, err := ParseCardEntry(raw, i)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"card %d: %s"}`, i, err.Error()), http.StatusBadRequest)
			return
		}
		entry.Card.FullName = buyer.FirstName + " " + buyer.LastName
		entries = append(entries, entry)
	}

	start := time.Now()
	log.Printf("Starting batch: %d cards", len(entries))

	results, err := pool.RunBatch(entries, buyer, perStoreDefaultLimit)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusTooManyRequests)
		return
	}

	elapsed := time.Since(start).Seconds()

	completed := 0
	for _, r := range results {
		if r.Status != "" {
			completed++
		}
	}

	resp := CheckResponse{
		Total:          len(entries),
		Completed:      completed,
		ElapsedSeconds: elapsed,
		Results:        results,
	}

	log.Printf("Batch complete: %d/%d in %.1fs", completed, len(entries), elapsed)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
