package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"untis-proxy/internal/proxy"
	"untis-proxy/internal/session"
	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

func main() {
	addr := flag.String("addr", ":8787", "listen address")
	server := flag.String("server", "schuldorf.webuntis.com", "upstream Untis server")
	school := flag.String("school", "schuldorf", "upstream school")
	db := flag.String("db", "untis.db", "sqlite database path")
	ttl := flag.Duration("ttl", 5*time.Minute, "timetable cache TTL")
	yearStart := flag.String("year-start", "2026-08-10", "current school year start (recon scan)")
	yearEnd := flag.String("year-end", "2027-06-27", "current school year end (recon scan)")
	flag.Parse()

	st, err := store.Open(*db)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	uc := untis.New(untis.Config{Server: *server, School: *school})
	sm := session.NewManager(24 * time.Hour)

	p := proxy.New(st, uc, sm, proxy.Options{School: *school, Server: *server, TTL: *ttl})
	p.StartRecon(*school, *yearStart, *yearEnd)
	log.Printf("listening on %s (upstream %s, school %s)", *addr, *server, *school)
	log.Fatal(http.ListenAndServe(*addr, p.Handler()))
}
