package proxy

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"untis-proxy/internal/store"
)

// ntfyBase is the publish base URL for push notifications; overridden in tests
// to point at a local httptest server.
var ntfyBase = "https://ntfy.sh"

// notifyHub fans out timetable-change events to SSE subscribers, keyed by
// school|classID.
type notifyHub struct {
	mu   sync.RWMutex
	subs map[string]map[chan notifyMsg]struct{}
}

type notifyMsg struct {
	School  string            `json:"school"`
	ClassID int64             `json:"classId"`
	Version int64             `json:"version"`
	Changes []store.PeriodRow `json:"changes"`
}

func newNotifyHub() *notifyHub {
	return &notifyHub{subs: map[string]map[chan notifyMsg]struct{}{}}
}

func hubKey(school string, classID int64) string {
	return fmt.Sprintf("%s|%d", school, classID)
}

func (h *notifyHub) subscribe(school string, classID int64) (chan notifyMsg, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan notifyMsg, 8)
	k := hubKey(school, classID)
	if h.subs[k] == nil {
		h.subs[k] = map[chan notifyMsg]struct{}{}
	}
	h.subs[k][ch] = struct{}{}
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if m, ok := h.subs[k]; ok {
			delete(m, ch)
			close(ch)
			if len(m) == 0 {
				delete(h.subs, k)
			}
		}
	}
}

func (h *notifyHub) publish(school string, classID int64, msg notifyMsg) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.subs[hubKey(school, classID)] {
		select {
		case ch <- msg:
		default:
		}
	}
}

// periodRow converts a fetched period into a change snapshot row.
func periodRow(pd map[string]any, md *masterDataCache) store.PeriodRow {
	st, _ := pd["startDateTime"].(string)
	en, _ := pd["endDateTime"].(string)
	pid, _ := pd["id"].(float64)
	var subject, room string
	for _, el := range periodElementIDs(pd) {
		switch el.Type {
		case "SUBJECT":
			if subject == "" {
				subject = elementName(md, "SUBJECT", el.ID)
			}
		case "ROOM":
			if room == "" {
				room = elementName(md, "ROOM", el.ID)
			}
		}
	}
	if subject == "" {
		subject = textField(pd, "subject")
	}
	if subject == "" {
		subject = textField(pd, "lesson")
	}
	desc := ""
	if t := textField(pd, "substitution"); t != "" {
		desc = "Substitution: " + t
	}
	if t := textField(pd, "info"); t != "" {
		if desc != "" {
			desc += "\n"
		}
		desc += "Info: " + t
	}
	return store.PeriodRow{
		PeriodID:    int64(pid),
		Start:       st,
		End:         en,
		Subject:     subject,
		Room:        room,
		Description: desc,
	}
}

// checkClass polls one class for timetable changes, persisting any diff. It
// returns true if a change was detected.
func (p *Proxy) checkClass(school string, classID int64) bool {
	now := time.Now()
	start := now.AddDate(0, 0, -1).Format("2006-01-02")
	end := now.AddDate(0, 0, 4).Format("2006-01-02")
	periods, err := p.classPeriodsFresh(school, classID, start, end)
	if err != nil {
		log.Printf("[notify] poll class %d: %v", classID, err)
		return false
	}
	md := p.masterData(school)
	next := make([]store.PeriodRow, 0, len(periods))
	for _, pd := range periods {
		if row := periodRow(pd, md); row.Start != "" {
			next = append(next, row)
		}
	}
	newVer := p.store.ClassVersion(school, classID) + 1
	dropBefore := time.Now().Format("2006-01-02")
	changed, err := p.store.ReplaceClassSnapshot(school, classID, next, newVer, dropBefore)
	if err != nil {
		log.Printf("[notify] store class %d: %v", classID, err)
		return false
	}
	if changed == 0 {
		return false
	}
	rows, cur, err := p.store.PendingChanges(school, classID, newVer-1)
	if err != nil {
		return true
	}
	p.hub.publish(school, classID, notifyMsg{
		School:  school,
		ClassID: classID,
		Version: cur,
		Changes: rows,
	})
	go p.deliverChange(school, classID, cur, rows)
	log.Printf("[notify] class %d changed (%d updates) -> version %d", classID, changed, cur)
	return true
}

// deliverChange fans a timetable change out to configured webhooks and ntfy
// topics (per-class and school-wide). Runs detached so a slow receiver never
// blocks the poll loop.
func (p *Proxy) deliverChange(school string, classID int64, version int64, rows []store.PeriodRow) {
	payload := map[string]any{
		"event":   "change",
		"school":  school,
		"classId": classID,
		"version": version,
		"changes": rows,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	summary := summarize(rows)

	hooks, err := p.store.ListWebhooks(school)
	if err != nil {
		log.Printf("[deliver] webhooks: %v", err)
	}
	for _, h := range hooks {
		if !h.Enabled {
			continue
		}
		if h.ClassID != 0 && h.ClassID != classID {
			continue
		}
		go p.postWebhook(h, body, summary)
	}

	topics, err := p.store.ListNtfyTopics(school)
	if err != nil {
		log.Printf("[deliver] ntfy: %v", err)
	}
	for _, t := range topics {
		if !t.Enabled {
			continue
		}
		if t.ClassID != 0 && t.ClassID != classID {
			continue
		}
		go p.publishNtfy(t, school, classID, summary)
	}
}

// postWebhook delivers a change payload to one webhook with a short retry.
// The shared secret (if set) is sent as an HMAC-SHA256 signature header so
// receivers can verify the request really came from this proxy.
func (p *Proxy) postWebhook(h *store.Webhook, body []byte, summary string) {
	sig := ""
	if h.Secret != "" {
		mac := hmac.New(sha256.New, []byte(h.Secret))
		mac.Write(body)
		sig = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequest(http.MethodPost, h.URL, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "untis-proxy/1.0")
		req.Header.Set("X-Untis-Event", "timetable-change")
		req.Header.Set("X-Untis-Summary", summary)
		if sig != "" {
			req.Header.Set("X-Untis-Signature", sig)
		}
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return
			}
		}
		if attempt < 2 {
			time.Sleep(time.Duration(attempt+1) * time.Second)
		}
	}
	log.Printf("[deliver] webhook %s failed after retries", h.URL)
}

// publishNtfy posts a change notification to an ntfy.sh topic (or any ntfy
// server, since the topic is a full publish URL path).
func (p *Proxy) publishNtfy(t *store.NtfyTopic, school string, classID int64, summary string) {
	title := fmt.Sprintf("Timetable change — class %d", classID)
	body, _ := json.Marshal(map[string]any{
		"topic":   t.Topic,
		"title":   title,
		"message": summary,
		"tags":    []string{"calendar"},
		"click":   fmt.Sprintf("https://api-untis.deelabs.tech/api/timetable/changes?school=%s&classId=%d", school, classID),
	})
	url := ntfyBase + "/" + t.Topic
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[deliver] ntfy %s: %v", t.Topic, err)
		return
	}
	resp.Body.Close()
}

// summarize flattens a set of timetable changes into a short human-readable
// notification line (e.g. "3 lessons changed · 1 new exam").
func summarize(rows []store.PeriodRow) string {
	var added, removed, exams int
	for _, r := range rows {
		switch r.Kind {
		case "ADDED":
			added++
		case "REMOVED":
			removed++
		default:
			exams++
		}
	}
	parts := []string{}
	if added > 0 {
		parts = append(parts, fmt.Sprintf("%d added", added))
	}
	if removed > 0 {
		parts = append(parts, fmt.Sprintf("%d removed", removed))
	}
	if exams > 0 {
		parts = append(parts, fmt.Sprintf("%d changed", exams))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("%d timetable change(s)", len(rows))
	}
	return fmt.Sprintf("%d change(s): %s", len(rows), strings.Join(parts, " · "))
}

// StartPollLoop runs the change-detection poller until done. It performs an
// immediate first pass, then polls every interval. The default school is always
// polled; any school that auto-registers later is picked up on the next tick.
func (p *Proxy) StartPollLoop(school string, interval time.Duration, done <-chan struct{}) {
	if interval <= 0 {
		interval = time.Minute
	}
	p.pollOnce(school)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			p.pollOnce(school)
			if schools, err := p.store.ListSchools(); err == nil {
				for _, sc := range schools {
					if sc.Name != school {
						p.pollOnce(sc.Name)
					}
				}
			}
		}
	}
}

func (p *Proxy) pollOnce(school string) {
	classes, err := p.store.Pool(school)
	if err != nil {
		return
	}
	for _, c := range classes {
		p.checkClass(school, c.ID)
	}
}

// handleTimetableChanges is the pollable diff API. It requires a session whose
// class matches classId; returns pending changes since `since`.
func (p *Proxy) handleTimetableChanges(w http.ResponseWriter, r *http.Request) {
	u := p.sessionUser(r)
	if u == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	school := r.URL.Query().Get("school")
	if school == "" {
		school = p.opts.School
	}
	classID, err := strconv.ParseInt(r.URL.Query().Get("classId"), 10, 64)
	if err != nil || classID <= 0 {
		classID = u.ClassID
	}
	if classID != u.ClassID {
		p.forbidden(w)
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	rows, cur, err := p.store.PendingChanges(school, classID, since)
	if err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	if len(rows) == 0 && cur == since {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	p.writeJSON(w, map[string]any{
		"school":  school,
		"classId": classID,
		"since":   since,
		"current": cur,
		"changes": rows,
	})
}

// handleTimetableStream is an SSE endpoint: it emits a change event whenever
// the requester's own class timetable changes. Heartbeats every 30s.
func (p *Proxy) handleTimetableStream(w http.ResponseWriter, r *http.Request) {
	u := p.sessionUser(r)
	if u == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	school := p.opts.School
	if sc := r.URL.Query().Get("school"); sc != "" {
		school = sc
	}
	classID := u.ClassID

	flusher, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch, unsub := p.hub.subscribe(school, classID)
	defer unsub()

	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	// initial snapshot so the client syncs state on connect
	rows, cur, _ := p.store.PendingChanges(school, classID, 0)
	if data, err := json.Marshal(map[string]any{
		"event": "snapshot", "school": school, "classId": classID,
		"current": cur, "changes": rows,
	}); err == nil {
		fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", data)
		flusher.Flush()
	}

	notifyClosed := r.Context().Done()
	for {
		select {
		case <-notifyClosed:
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case msg := <-ch:
			data, err := json.Marshal(map[string]any{
				"event": "change", "school": msg.School,
				"classId": msg.ClassID, "version": msg.Version, "changes": msg.Changes,
			})
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: change\ndata: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
