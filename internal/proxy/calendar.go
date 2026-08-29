package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"untis-proxy/internal/store"
)

// icsEscape escapes a string for an iCalendar TEXT value.
func icsEscape(s string) string {
	r := strings.NewReplacer("\\", "\\\\", ",", "\\,", ";", "\\;", "\n", "\\n")
	return r.Replace(s)
}

// newCalendarToken generates a cryptographically random opaque token.
func newCalendarToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// handleCalendarToken creates (or returns the existing) calendar subscription
// token for a school+class. It requires a valid session; the class must be in
// the pool. The response carries the token and the full subscription URL.
func (p *Proxy) handleCalendarToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	u := p.sessionUser(r)
	if u == nil {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	var req struct {
		School  string `json:"school"`
		ClassID int64  `json:"classId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	school := req.School
	if school == "" {
		school = p.opts.School
	}
	if req.ClassID <= 0 {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"classId required"}`))
		return
	}
	ok, err := p.store.PoolContains(req.ClassID)
	if err != nil || !ok {
		p.forbidden(w)
		return
	}
	tok, err := p.store.ClassTokenForClass(school, req.ClassID)
	if err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	if tok == nil {
		now := time.Now().Unix()
		tok = &store.ClassToken{
			Token: newCalendarToken(), School: school,
			ClassID: req.ClassID, CreatedAt: now, LastAccess: now,
		}
		if err := p.store.CreateClassToken(tok); err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
	}
	p.writeJSON(w, map[string]any{
		"school":   tok.School,
		"classId":  tok.ClassID,
		"token":    tok.Token,
		"url":      p.calendarURL(r, tok.Token),
		"created":  tok.CreatedAt,
		"lastUsed": tok.LastAccess,
	})
}

// handleCalendarICS serves the .ics subscription feed for a token. It is public
// by design (anyone with the opaque token can read the class timetable), so the
// token must be treated as a password.
func (p *Proxy) handleCalendarICS(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSuffix(r.PathValue("token"), ".ics")
	tok, err := p.store.ClassTokenByToken(token)
	if err != nil || tok == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_ = p.store.TouchClassToken(token, time.Now().Unix())

	md := p.masterData(tok.School)
	className := elementName(md, "CLASS", tok.ClassID)
	if className == "" {
		className = fmt.Sprintf("class-%d", tok.ClassID)
	}

	start := time.Now()
	end := start.AddDate(0, 0, 30)
	periods, err := p.classPeriods(tok.School, tok.ClassID,
		start.Format("2006-01-02"), end.Format("2006-01-02"))
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"fetch failed"}`))
		return
	}

	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\n")
	b.WriteString("VERSION:2.0\r\n")
	b.WriteString("PRODID:-//untis-api//Calendar//EN\r\n")
	b.WriteString("CALSCALE:GREGORIAN\r\n")
	b.WriteString("X-WR-CALNAME:" + icsEscape("Untis "+className) + "\r\n")
	b.WriteString("X-WR-CALDESC:" + icsEscape("Untis timetable for "+className) + "\r\n")
	for _, pd := range periods {
		b.WriteString(p.buildICSVEVENT(pd, md))
	}
	b.WriteString("END:VCALENDAR\r\n")

	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="untis-%d.ics"`, tok.ClassID))
	_, _ = w.Write([]byte(b.String()))
}

// buildICSVEVENT renders one period as a VEVENT.
func (p *Proxy) buildICSVEVENT(pd map[string]any, md *masterDataCache) string {
	st, _ := pd["startDateTime"].(string)
	en, _ := pd["endDateTime"].(string)
	stT, err := parsePeriodTime(st)
	if err != nil {
		return ""
	}
	enT, err := parsePeriodTime(en)
	if err != nil {
		return ""
	}
	periodID, _ := pd["id"].(float64)

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

	summary := subject
	if room != "" {
		summary += " · " + room
	}
	descParts := []string{}
	if t := textField(pd, "substitution"); t != "" {
		descParts = append(descParts, "Substitution: "+t)
	}
	if t := textField(pd, "info"); t != "" {
		descParts = append(descParts, "Info: "+t)
	}
	description := strings.Join(descParts, "\n")

	var b strings.Builder
	b.WriteString("BEGIN:VEVENT\r\n")
	b.WriteString(fmt.Sprintf("UID:%d@untis-api\r\n", int64(periodID)))
	b.WriteString(fmt.Sprintf("DTSTART;TZID=Europe/Berlin:%s\r\n", stT.Format("20060102T150405")))
	b.WriteString(fmt.Sprintf("DTEND;TZID=Europe/Berlin:%s\r\n", enT.Format("20060102T150405")))
	b.WriteString("SUMMARY:" + icsEscape(summary) + "\r\n")
	if room != "" {
		b.WriteString("LOCATION:" + icsEscape(room) + "\r\n")
	}
	if description != "" {
		b.WriteString("DESCRIPTION:" + icsEscape(description) + "\r\n")
	}
	b.WriteString("TRANSP:OPAQUE\r\n")
	b.WriteString("CLASS:PUBLIC\r\n")
	b.WriteString("END:VEVENT\r\n")
	return b.String()
}

// parsePeriodTime parses a WebUntis RFC3339-like period timestamp.
func parsePeriodTime(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02T15:04Z07:00", "2006-01-02T15:04:05Z07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse time %q", s)
}

// calendarURL builds the absolute .ics subscription URL for a token.
func (p *Proxy) calendarURL(r *http.Request, token string) string {
	scheme := "https"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	return scheme + "://" + host + "/api/calendar/" + token + ".ics"
}
