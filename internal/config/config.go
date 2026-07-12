// Package config טוען הגדרות ממשתני סביבה.
package config

import "os"

type Config struct {
	Port          string
	TwelveDataKey string
	FinnhubKey    string
	WebDir        string
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func Load() Config {
	return Config{
		Port:          getEnv("PORT", "3003"),
		TwelveDataKey: getEnv("TWELVEDATA_API_KEY", ""),
		FinnhubKey:    getEnv("FINNHUB_API_KEY", ""),
		WebDir:        getEnv("WEB_DIR", "web"),
	}
}
