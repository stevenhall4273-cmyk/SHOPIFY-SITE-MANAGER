package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
)

func main() {
	// Number of Chrome processes (each handles ~10 tabs)
	numChrome := 3
	if v := os.Getenv("CHROME_POOL_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			numChrome = n
		}
	}

	// HTTP port — Railway sets PORT env var
	port := "8080"
	if v := os.Getenv("PORT"); v != "" {
		port = v
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	// Connect to PostgreSQL (optional — if DATABASE_URL not set, site management is disabled)
	var db *DB
	if os.Getenv("DATABASE_URL") != "" {
		var err error
		db, err = NewDB()
		if err != nil {
			log.Fatalf("Failed to connect to database: %v", err)
		}
		defer db.Close()
		fmt.Println("Database connected, site management enabled")
	} else {
		fmt.Println("DATABASE_URL not set — site management disabled (card checking still works)")
	}

	fmt.Printf("Starting with %d Chrome processes...\n", numChrome)
	pool := NewChromePool(numChrome)
	defer pool.Close()
	fmt.Printf("All %d Chrome processes ready\n", numChrome)

	// Start background site check worker if DB is available
	stopWorker := make(chan struct{})
	if db != nil {
		worker := NewSiteCheckWorker(db, pool, numChrome)
		go worker.Run(stopWorker)
		fmt.Println("Site check worker started")
	}

	// Graceful shutdown
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		fmt.Println("\nShutting down...")
		close(stopWorker)
		pool.Close()
		if db != nil {
			db.Close()
		}
		os.Exit(0)
	}()

	StartServer(":"+port, pool, db)
}
