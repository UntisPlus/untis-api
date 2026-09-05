package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"untis-proxy/internal/store"
)

func TestDeliverChangeFansOutToMatchingHooksOnly(t *testing.T) {
	p, st := newTestProxy(t)

	var mu sync.Mutex
	hits := map[string]int{}
	sigs := map[string]string{}
	summaryByURL := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		sigs[r.URL.Path] = r.Header.Get("X-Untis-Signature")
		summaryByURL[r.URL.Path] = r.Header.Get("X-Untis-Summary")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// school-wide hook (always fires) with secret -> signature expected
	_, _ = st.AddWebhook(&store.Webhook{School: "schuldorf", ClassID: 0, URL: srv.URL + "/all", Secret: "topsecret", Enabled: true})
	// matching per-class hook
	_, _ = st.AddWebhook(&store.Webhook{School: "schuldorf", ClassID: 4419, URL: srv.URL + "/c4419", Enabled: true})
	// non-matching per-class hook
	_, _ = st.AddWebhook(&store.Webhook{School: "schuldorf", ClassID: 9999, URL: srv.URL + "/c9999", Enabled: true})
	// disabled hook
	_, _ = st.AddWebhook(&store.Webhook{School: "schuldorf", ClassID: 0, URL: srv.URL + "/disabled", Enabled: false})
	// other school's hook must not fire for this school
	_, _ = st.AddWebhook(&store.Webhook{School: "otherschool", ClassID: 0, URL: srv.URL + "/other", Enabled: true})

	p.deliverChange("schuldorf", 4419, 7, []store.PeriodRow{
		{PeriodID: 1, Kind: "ADDED", Subject: "M", Start: "2026-09-07 08:00"},
	})

	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if hits["/all"] != 1 {
		t.Fatalf("/all hits = %d, want 1", hits["/all"])
	}
	if hits["/c4419"] != 1 {
		t.Fatalf("/c4419 hits = %d, want 1", hits["/c4419"])
	}
	if hits["/c9999"] != 0 {
		t.Errorf("/c9999 hits = %d, want 0 (wrong class)", hits["/c9999"])
	}
	if hits["/disabled"] != 0 {
		t.Errorf("/disabled hits = %d, want 0 (disabled hook)", hits["/disabled"])
	}
	if hits["/other"] != 0 {
		t.Errorf("/other hits = %d, want 0 (other school)", hits["/other"])
	}
	if summaryByURL["/all"] == "" {
		t.Errorf("X-Untis-Summary header not set on /all")
	}
	if len(sigs["/all"]) != 64+len("sha256=") {
		t.Errorf("X-Untis-Signature header missing or wrong length on /all: %q", sigs["/all"])
	}
	if sigs["/c4419"] != "" {
		t.Errorf("/c4419 has no secret, should have no signature, got %q", sigs["/c4419"])
	}
}

func TestPublishNtfyPostsMessage(t *testing.T) {
	var mu sync.Mutex
	received := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		defer r.Body.Close()
		var m map[string]any
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			t.Errorf("bad body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		received = m
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	old := ntfyBase
	ntfyBase = srv.URL
	defer func() { ntfyBase = old }()

	p, _ := newTestProxy(t)
	p.publishNtfy(&store.NtfyTopic{Topic: "myclass"}, "schuldorf", 4419, "2 added")

	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if received["topic"] != "myclass" {
		t.Fatalf("topic = %#v, want myclass", received["topic"])
	}
	if received["message"] != "2 added" {
		t.Fatalf("message = %#v, want summary", received["message"])
	}
	if received["title"] == "" {
		t.Errorf("title not set")
	}
}
