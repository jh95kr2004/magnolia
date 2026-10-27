/*
Package main (test_db)
This file is an independent test program used to verify PostgreSQL connection, AutoMigrate operations, AES-GCM API key encryption/decryption, and the Crash Recovery state management (Save/Load).
*/
package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/magnolia/backend/internal/db/postgres"
)

func main() {
	// Set the 32-byte master key for AES-GCM for this test
	os.Setenv("ENCRYPTION_MASTER_KEY", "12345678901234567890123456789012")

	// 1. Connect and Migrate DB (Using local SQLite for simplicity in testing, or Postgres if running natively)
	// For this test, we assume a local Postgres is not guaranteed to be running, so we'll just demonstrate 
	// the data structures if Postgres is up, otherwise we connect to a local postgres Docker instance or print.
	
	// Assuming a local postgres database named "magnolia_test" on user/pass postgres/postgres
	dsn := "host=localhost user=postgres password=postgres dbname=magnolia_test port=5432 sslmode=disable TimeZone=Asia/Seoul"
	db, err := postgres.NewDB(dsn)
	if err != nil {
		fmt.Printf("[SKIP] Could not connect to local Postgres (%v). Ensure Postgres is running to test end-to-end.\n", err)
		
		// We can still test Encryption locally without DB
		fmt.Println("--- Testing AES-GCM Encryption Module ---")
		testCrypto()
		return
	}

	fmt.Println("[SUCCESS] Connected to test Postgres DB and completed AutoMigrate.")

	// Test Encryption & API Key Save
	fmt.Println("\n--- Testing AES-GCM Encryption & API Keys ---")
	plaintextSecret := "my-super-secret-coinbase-key-12345"
	encrypted, err := postgres.Encrypt([]byte(plaintextSecret))
	if err != nil {
		log.Fatalf("Encryption failed: %v", err)
	}

	apiKey := &postgres.UserAPIKey{
		UserID:                 1,
		Exchange:               "COINBASE",
		AccessKey:              "my-access-key",
		SecretKey:              string(encrypted),
		DefaultWithdrawalLimit: 250000.0,
	}

	if err := db.SaveAPIKey(apiKey); err != nil {
		log.Printf("Failed to save API key: %v", err)
	} else {
		fmt.Printf("Saved API Key (Encrypted Secret: %x) successfully.\n", encrypted)
	}

	fetchedKey, err := db.GetAPIKey(1, "COINBASE")
	if err == nil {
		decrypted, _ := postgres.Decrypt([]byte(fetchedKey.SecretKey))
		fmt.Printf("Retrieved and Decrypted API Key: %s\n", string(decrypted))
	}

	// Test Crash Recovery / Position Management
	fmt.Println("\n--- Testing ActivePosition Crash Recovery ---")
	pos := &postgres.ActivePosition{
		PositionID:         "POS-TEST-001",
		MonitorNodeID:      "node-01",
		Status:             "OPEN",
		Type:               "BORROW_TRANSFER",
		BaseExchange:       "UPBIT",
		ForeignExchange:    "COINBASE",
		BaseSymbol:         "BTC-KRW",
		ForeignSymbol:      "BTC-USD",
		InvestmentUSD:      500.0,
		EntryBasePrice:     90000.0,
		MaxUnrealizedROI:   0.05,
		OpenedAt:           time.Now(),
	}

	if err := db.SavePosition(pos); err != nil {
		log.Printf("Failed to save position: %v", err)
	} else {
		fmt.Println("Saved ACTIVE position simulating an executor entering a trade.")
	}

	// Simulate a crash/restart... engine boots up and calls GetActivePositions
	activePositions, err := db.GetActivePositions("node-01")
	if err != nil {
		log.Fatalf("Failed to fetch active positions: %v", err)
	}
	
	fmt.Printf("\n[BOOT] Recovered %d active position(s) from database.\n", len(activePositions))
	for _, p := range activePositions {
		fmt.Printf("   -> Resume Tracking: %s (%s) | %s to %s | Investment: $%.2f | Peak ROI: %.2f%%\n",
			p.PositionID, p.Status, p.BaseExchange, p.ForeignExchange, p.InvestmentUSD, p.MaxUnrealizedROI*100)
	}

	// Clean up for next run
	db.UpdatePositionStatus("POS-TEST-001", "CLOSED")
	fmt.Println("Updated Position Status to CLOSED.")
}

func testCrypto() {
	plaintext := []byte("secret-key")
	enc, err := postgres.Encrypt(plaintext)
	if err != nil {
		fmt.Println("Encrypt Error:", err)
		return
	}
	fmt.Printf("Encrypted (Hex): %x\n", enc)

	dec, err := postgres.Decrypt(enc)
	if err != nil {
		fmt.Println("Decrypt Error:", err)
		return
	}
	fmt.Printf("Decrypted: %s\n", string(dec))
}
