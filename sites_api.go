package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// apiKey is a simple shared secret for the scraper to authenticate.
var apiKey string

func init() {
	apiKey = os.Getenv("API_KEY")
}

// RegisterSiteRoutes adds site management endpoints to the mux.
func RegisterSiteRoutes(mux *http.ServeMux, db *DB) {
	mux.HandleFunc("/sites/add", func(w http.ResponseWriter, r *http.Request) {
		handleAddSites(w, r, db)
	})
	mux.HandleFunc("/sites/working", func(w http.ResponseWriter, r *http.Request) {
		handleWorkingSites(w, r, db)
	})
	mux.HandleFunc("/sites/stats", func(w http.ResponseWriter, r *http.Request) {
		handleStats(w, r, db)
	})
	mux.HandleFunc("/sites/export", func(w http.ResponseWriter, r *http.Request) {
		handleExport(w, r, db)
	})
}

func checkAuth(r *http.Request) bool {
	if apiKey == "" {
		return true // no key configured = open
	}
	auth := r.Header.Get("Authorization")
	return auth == "Bearer "+apiKey
}

// POST /sites/add — scraper submits discovered sites
// Body: {"urls": ["https://store1.myshopify.com", ...]}
func handleAddSites(w http.ResponseWriter, r *http.Request, db *DB) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if !checkAuth(r) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var req struct {
		URLs []string `json:"urls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"invalid JSON: %s"}`, err.Error()), http.StatusBadRequest)
		return
	}

	// Validate and clean URLs
	clean := make([]string, 0, len(req.URLs))
	for _, u := range req.URLs {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if !strings.Contains(u, "myshopify.com") {
			continue
		}
		// Normalize to https
		if !strings.HasPrefix(u, "http") {
			u = "https://" + u
		}
		clean = append(clean, u)
	}

	if len(clean) == 0 {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"added":    0,
			"received": 0,
		})
		return
	}

	added, err := db.AddSites(clean)
	if err != nil {
		log.Printf("Error adding sites: %v", err)
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	log.Printf("[api] Added %d/%d new sites", added, len(clean))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"added":    added,
		"received": len(clean),
	})
}

// GET /sites/working?limit=100&offset=0
func handleWorkingSites(w http.ResponseWriter, r *http.Request, db *DB) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	limit := 100
	offset := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	sites, total, err := db.GetWorkingSites(limit, offset)
	if err != nil {
		log.Printf("Error getting working sites: %v", err)
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total":  total,
		"limit":  limit,
		"offset": offset,
		"sites":  sites,
	})
}

// GET /sites/stats
func handleStats(w http.ResponseWriter, r *http.Request, db *DB) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	stats, err := db.GetStats()
	if err != nil {
		log.Printf("Error getting stats: %v", err)
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	total := 0
	for _, v := range stats {
		total += v
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total":     total,
		"by_status": stats,
	})
}

// GET /sites/export — download all working sites as plain text (one per line)
func handleExport(w http.ResponseWriter, r *http.Request, db *DB) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	sites, _, err := db.GetWorkingSites(10000, 0)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Content-Disposition", "attachment; filename=working_sites.txt")
	for _, s := range sites {
		fmt.Fprintf(w, "%s | $%.2f\n", s.URL, s.CheckoutPrice)
	}
}
