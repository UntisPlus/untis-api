package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"untis-proxy/internal/store"
)

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
	log.Printf("[notify] class %d changed (%d updates) -> version %d", classID, changed, cur)
	return true
}

// StartPollLoop runs the change-detection poller until done. It performs an
// immediate first pass, then polls every interval.
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
		}
	}
}

func (p *Proxy) pollOnce(school string) {
	classes, err := p.store.Pool()
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
