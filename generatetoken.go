package main

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// In production, keep this secret safe in an environment variable!
var mySigningKey = []byte("super-secret-gateway-key")

func main() {
	// 1. Generate a Premium User Token
	premiumClaims := jwt.MapClaims{
		"sub":  "user_premium_99",
		"tier": "premium",
		"exp":  time.Now().Add(time.Hour * 24).Unix(),
	}
	premiumToken := jwt.NewWithClaims(jwt.SigningMethodHS256, premiumClaims)
	premiumStr, _ := premiumToken.SignedString(mySigningKey)

	// 2. Generate a Free User Token
	freeClaims := jwt.MapClaims{
		"sub":  "user_free_11",
		"tier": "free",
		"exp":  time.Now().Add(time.Hour * 24).Unix(),
	}
	freeToken := jwt.NewWithClaims(jwt.SigningMethodHS256, freeClaims)
	freeStr, _ := freeToken.SignedString(mySigningKey)

	fmt.Println("🎟️ PREMIUM TOKEN:")
	fmt.Printf("Authorization: Bearer %s\n\n", premiumStr)
	fmt.Println("🎟️ FREE TOKEN:")
	fmt.Printf("Authorization: Bearer %s\n", freeStr)
}