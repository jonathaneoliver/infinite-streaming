package main

import "github.com/jonathaneoliver/infinite-streaming/analytics/go-forwarder/internal/plays"

// playsBackend constructs the minimal ClickHouse handle the typed
// domain layer (internal/plays) needs from this binary's config.
// Lifted out so every caller — HTTP handlers, the classifier queue
// drain, the chat backend (#497) — composes the same value.
func playsBackend(cfg config) plays.Backend {
	return plays.Backend{
		ClickHouseURL: cfg.clickhouseURL,
		Database:      cfg.chDatabase,
		EventsTable:   cfg.chTable,
		User:          cfg.chUser,
		Password:      cfg.chPassword,
	}
}
