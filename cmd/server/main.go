package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
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
	yearStart := flag.String("year-start", "", "school year start override (default: auto-derived)")
	yearEnd := flag.String("year-end", "", "school year end override (default: auto-derived)")
	env := flag.String("env", "", "deployment mode (dev|beta|prod); defaults to UNTIS_ENV, else dev")
	version := flag.String("version", "dev", "reported build version")
	poll := flag.Duration("poll-interval", 60*time.Second, "timetable change-detection poll interval")
	flag.Parse()

	if *env != "" {
		os.Setenv("UNTIS_ENV", *env)
	}
	if *version != "" {
		os.Setenv("UNTIS_VERSION", *version)
	}

	ys, ye := schoolYear(time.Now())
	if *yearStart != "" {
		ys = *yearStart
	}
	if *yearEnd != "" {
		ye = *yearEnd
	}

	st, err := store.Open(*db)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	uc := untis.New(untis.Config{Server: *server, School: *school})
	sm := session.NewManager(24 * time.Hour)

	p := proxy.New(st, uc, sm, proxy.Options{School: *school, TTL: *ttl})
	p.LoadRecon()
	p.StartRecon(*school, ys, ye)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pollDone := make(chan struct{})
	go p.StartPollLoop(*school, *poll, pollDone)

	srv := &http.Server{Addr: *addr, Handler: p.Handler()}
	log.Printf("listening on %s (upstream %s, school %s)", *addr, *server, *school)

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		log.Printf("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}

	close(pollDone)
	p.PersistRecon()
	if err := st.Close(); err != nil {
		log.Printf("close store: %v", err)
	}
}

// schoolYear returns the bounds of the current WebUntis school year so the
// recon scan needs no periodic manual maintenance. German school years start
// in August: for any date in Aug..Dec the year runs from 1 Aug of the current
// year to 31 Jul of the next; from Jan..Jul it runs from 1 Aug of the previous
// year to 31 Jul of the current year.
func schoolYear(now time.Time) (string, string) {
	start := time.Date(now.Year(), time.August, 1, 0, 0, 0, 0, now.Location())
	if now.Month() < time.August {
		start = start.AddDate(-1, 0, 0)
	}
	end := start.AddDate(1, 0, 0).AddDate(0, 0, -1)
	return start.Format("2006-01-02"), end.Format("2006-01-02")
}
