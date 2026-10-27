//go:build ignore

package main

import (
	"fmt"
	"log"

	"github.com/magnolia/backend/internal/db/postgres"
)

func main() {
	db, err := postgres.NewDB("host=localhost user=magnolia_user password=magnolia_password dbname=magnolia_db port=5432 sslmode=disable")
	if err != nil {
		log.Fatalf("DB error: %v", err)
	}

	user := &postgres.User{
		Email:        "admin@magnolia.test",
		PasswordHash: "password123", // Dummy plaintext match for the prototype
	}

	if err := db.CreateUser(user); err != nil {
		fmt.Printf("User creation skipped/failed: %v\n", err)
	} else {
		db.SaveUserSettings(&postgres.UserSettings{
			UserID: user.ID,
		})
		fmt.Println("Admin user created successfully!")
	}
}
