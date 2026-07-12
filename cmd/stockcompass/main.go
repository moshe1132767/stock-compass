// מצפן המניות — שרת המלצות מניות לפי אינדיקטורים טכניים.
package main

import (
	"log"

	"stockcompass/internal/config"
	"stockcompass/internal/server"
)

func main() {
	cfg := config.Load()
	s := server.New(cfg)
	if err := s.Start(); err != nil {
		log.Fatalf("שגיאת שרת: %v", err)
	}
}
