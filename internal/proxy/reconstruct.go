package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"
)

// elementDB tracks which teachers/rooms/subjects actually appear in the pooled
// classes' timetables. Only those are made displayable in client apps and are
// served reconstructed timetables for.
type elementDB struct {
	mu        sync.RWMutex
	teachers  map[int64]bool
	rooms     map[int64]bool
	subjects  map[int64]bool
	scanUntil map[int64]string
}

func newElementDB() *elementDB {
	return &elementDB{
		teachers:  map[int64]bool{},
		rooms:     map[int64]bool{},
		subjects:  map[int64]bool{},
		scanUntil: map[int64]string{},
	}
}

// snapshot returns the current element sets as type -> [ids], suitable for
// persisting on shutdown.
func (e *elementDB) snapshot() map[string][]int64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := map[string][]int64{}
	for id := range e.teachers {
		out["TEACHER"] = append(out["TEACHER"], id)
	}
	for id := range e.rooms {
		out["ROOM"] = append(out["ROOM"], id)
	}
	for id := range e.subjects {
		out["SUBJECT"] = append(out["SUBJECT"], id)
	}
	return out
}

// seedFrom merges in a previously-persisted set of known elements. It is a warm
// start only: the background scan re-runs on boot and revalidates (and removes
// nothing) once it fetches fresh timetables.
func (e *elementDB) seedFrom(elems map[string][]int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for t, ids := range elems {
		for _, id := range ids {
			switch t {
			case "TEACHER":
				e.teachers[id] = true
			case "ROOM":
				e.rooms[id] = true
			case "SUBJECT":
				e.subjects[id] = true
			}
		}
	}
}

func (e *elementDB) has(elType string, id int64) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	switch elType {
	case "TEACHER":
		return e.teachers[id]
	case "ROOM":
		return e.rooms[id]
	case "SUBJECT":
		return e.subjects[id]
	}
	return false
}

// fetchClassChunk fetches one class timetable over a single (<=2 week) chunk,
// using the class owner's account. Results are cached by class+range.
func (p *Proxy) fetchClassChunk(school string, classID int64, start, end string) ([]map[string]any, error) {
	key := fmt.Sprintf("recon|%d|%s|%s", classID, start, end)
	if v, ok := p.tt.Get(key); ok {
		var out []map[string]any
		if err := json.Unmarshal(v, &out); err == nil {
			return out, nil
		}
	}
	owner, err := p.store.OwnerForClass(classID)
	if err != nil || owner == nil {
		return nil, fmt.Errorf("no owner for class %d", classID)
	}
	body, _ := json.Marshal(map[string]any{
		"id": "untis-proxy-recon", "jsonrpc": "2.0", "method": "getTimetable2017",
		"params": []any{map[string]any{
			"id": classID, "type": "CLASS",
			"startDate": start, "endDate": end,
			"masterDataTimestamp": 0, "timetableTimestamp": 0, "timetableTimestamps": []any{},
		}},
	})
	newBody, err := p.rewriteAuthForOwner(school, body, owner)
	if err != nil {
		return nil, err
	}
	b, _, _, err := p.untis.RawIntern(school, "", "getTimetable2017", newBody)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result struct {
			Timetable struct {
				Periods []map[string]any `json:"periods"`
			} `json:"timetable"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(resp.Result.Timetable.Periods)
	p.tt.Put(key, raw)
	return resp.Result.Timetable.Periods, nil
}

func chunkDates(start, end string) ([][2]string, error) {
	s, err := time.Parse("2006-01-02", start)
	if err != nil {
		return nil, err
	}
	e, err := time.Parse("2006-01-02", end)
	if err != nil {
		return nil, err
	}
	if e.Before(s) {
		return nil, fmt.Errorf("end before start")
	}
	var out [][2]string
	for {
		chunkEnd := s.AddDate(0, 0, 13)
		if chunkEnd.After(e) {
			chunkEnd = e
		}
		out = append(out, [2]string{s.Format("2006-01-02"), chunkEnd.Format("2006-01-02")})
		if chunkEnd.Equal(e) {
			break
		}
		s = chunkEnd.AddDate(0, 0, 1)
	}
	return out, nil
}

// classPeriods returns all periods for a class over a (possibly long) range,
// fetched in <=2 week chunks and deduplicated by period id.
//
// Periods are deduplicated by their unique period id, NOT by lesson id: a
// double (or longer) lesson is represented by the school server as several
// distinct period entries that share one lessonId. Deduping on lessonId would
// collapse those consecutive time slots into a single one.
func (p *Proxy) classPeriods(school string, classID int64, start, end string) ([]map[string]any, error) {
	chunks, err := chunkDates(start, end)
	if err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	var out []map[string]any
	for _, c := range chunks {
		ps, err := p.fetchClassChunk(school, classID, c[0], c[1])
		if err != nil {
			return nil, err
		}
		for _, pd := range ps {
			pid, _ := pd["id"].(float64)
			if seen[int64(pid)] {
				continue
			}
			seen[int64(pid)] = true
			out = append(out, pd)
		}
	}
	return out, nil
}

func periodElementIDs(pd map[string]any) []struct {
	Type string
	ID   int64
} {
	var out []struct {
		Type string
		ID   int64
	}
	els, _ := pd["elements"].([]any)
	for _, el := range els {
		m, ok := el.(map[string]any)
		if !ok {
			continue
		}
		t, _ := m["type"].(string)
		idf, _ := m["id"].(float64)
		if t == "" {
			continue
		}
		out = append(out, struct {
			Type string
			ID   int64
		}{t, int64(idf)})
	}
	return out
}

// elementPeriods reconstructs the timetable for one teacher/room/subject by
// merging the pooled classes' timetables over the range. Periods are deduped by
// (start, end, subject) so a teacher teaching two classes at once appears once.
func (p *Proxy) elementPeriods(school, elType string, elID int64, start, end string) ([]map[string]any, error) {
	classes, err := p.store.Pool()
	if err != nil {
		return nil, err
	}
	type key struct {
		start, end, subject string
	}
	seen := map[key]bool{}
	var out []map[string]any
	for _, c := range classes {
		ps, err := p.classPeriods(school, c.ID, start, end)
		if err != nil {
			return nil, err
		}
		for _, pd := range ps {
			matched := false
			sub := ""
			for _, el := range periodElementIDs(pd) {
				if el.Type == elType && el.ID == elID {
					matched = true
				}
				if el.Type == "SUBJECT" {
					sub = fmt.Sprintf("%d", el.ID)
				}
			}
			if !matched {
				continue
			}
			st, _ := pd["startDateTime"].(string)
			en, _ := pd["endDateTime"].(string)
			k := key{st, en, sub}
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, pd)
		}
	}
	return out, nil
}

// scanClass enumerates teachers/rooms/subjects for one class across the school
// year by fetching week-by-week (the real server caps ranges at ~2 weeks).
func (p *Proxy) scanClass(school string, classID int64, yearStart, yearEnd string) {
	chunks, err := chunkDates(yearStart, yearEnd)
	if err != nil {
		return
	}
	// only scan up to a bounded horizon (today + a few weeks) to avoid hammering
	// the school server with the entire future year on every startup.
	horizon := time.Now().AddDate(0, 0, 21).Format("2006-01-02")
	chunks, err = chunkDates(yearStart, horizon)
	if err != nil {
		chunks, _ = chunkDates(yearStart, yearEnd)
	}
	for _, c := range chunks {
		ps, err := p.fetchClassChunk(school, classID, c[0], c[1])
		if err != nil {
			log.Printf("[recon] class %d %s..%s: %v", classID, c[0], c[1], err)
			continue
		}
		for _, pd := range ps {
			for _, el := range periodElementIDs(pd) {
				switch el.Type {
				case "TEACHER":
					p.recon.mu.Lock()
					p.recon.teachers[el.ID] = true
					p.recon.mu.Unlock()
				case "ROOM":
					p.recon.mu.Lock()
					p.recon.rooms[el.ID] = true
					p.recon.mu.Unlock()
				case "SUBJECT":
					p.recon.mu.Lock()
					p.recon.subjects[el.ID] = true
					p.recon.mu.Unlock()
				}
			}
		}
	}
	p.recon.mu.Lock()
	p.recon.scanUntil[classID] = horizon
	p.recon.mu.Unlock()
	_ = p.store.SaveReconScanAt(school, classID, horizon)
}

// StartRecon kicks off background enumeration of known teachers/rooms/subjects
// across all pooled classes.
func (p *Proxy) StartRecon(school, yearStart, yearEnd string) {	classes, err := p.store.Pool()
	if err != nil {
		log.Printf("[recon] no pool: %v", err)
		return
	}
	if len(classes) == 0 {
		return
	}
	go func() {
		// stagger to avoid a burst of simultaneous requests
		for i, c := range classes {
			time.Sleep(time.Duration(i) * 400 * time.Millisecond)
			p.scanClass(school, c.ID, yearStart, yearEnd)
		}
		log.Printf("[recon] enumeration done: %d teachers, %d rooms, %d subjects",
			len(p.recon.teachers), len(p.recon.rooms), len(p.recon.subjects))
	}()
}

// LoadRecon restores the persisted element set so reconstruction requests are
// answered immediately on boot, before the background scan finishes. The scan
// then re-runs and refreshes the fresh state. It is only ever a warm start and
// not served as a replacement for fresh data.
func (p *Proxy) LoadRecon() {
	elems, err := p.store.LoadReconElements()
	if err != nil || len(elems) == 0 {
		return
	}
	p.recon.seedFrom(elems)
	log.Printf("[recon] restored snapshot: %d teachers, %d rooms, %d subjects",
		len(elems["TEACHER"]), len(elems["ROOM"]), len(elems["SUBJECT"]))
}

// PersistRecon writes the current recon element set (and per-class scan
// progress) to persistent storage. Call on graceful shutdown.
func (p *Proxy) PersistRecon() {
	if err := p.store.SaveReconElements(p.recon.snapshot()); err != nil {
		log.Printf("[recon] persist snapshot: %v", err)
		return
	}
	log.Printf("[recon] saved snapshot")
}

// weekRange returns the Monday..Friday range containing the given date.
func weekRange(date string) (string, string, error) {
	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		return "", "", err
	}
	wd := int(d.Weekday())
	if wd == 0 {
		wd = 7
	}
	monday := d.AddDate(0, 0, -(wd - 1))
	return monday.Format("2006-01-02"), monday.AddDate(0, 0, 4).Format("2006-01-02"), nil
}

// weeklyPeriod converts a JSON-RPC timetable period into the weekly-data REST
// format, returning the converted period and the element IDs it references.
func weeklyPeriod(pd map[string]any) (map[string]any, []int64, error) {
	st, _ := pd["startDateTime"].(string)
	en, _ := pd["endDateTime"].(string)
	if st == "" || en == "" {
		return nil, nil, fmt.Errorf("no times")
	}
	stT, err := time.Parse("2006-01-02T15:04Z07:00", st)
	if err != nil {
		stT, err = time.Parse("2006-01-02T15:04:05Z07:00", st)
		if err != nil {
			return nil, nil, err
		}
	}
	enT, err := time.Parse("2006-01-02T15:04Z07:00", en)
	if err != nil {
		enT, err = time.Parse("2006-01-02T15:04:05Z07:00", en)
		if err != nil {
			return nil, nil, err
		}
	}

	elTypes := map[string]int{"CLASS": 1, "TEACHER": 2, "SUBJECT": 3, "ROOM": 4}
	els := make([]map[string]any, 0)
	var ids []int64
	for _, el := range periodElementIDs(pd) {
		t, ok := elTypes[el.Type]
		if !ok {
			continue
		}
		els = append(els, map[string]any{
			"type": t, "id": el.ID, "orgId": el.ID,
			"missing": false, "state": "REGULAR",
		})
		ids = append(ids, el.ID)
	}
	if len(els) == 0 {
		return nil, nil, fmt.Errorf("no elements")
	}

	lessonID, _ := pd["lessonId"].(float64)
	periodID, _ := pd["id"].(float64)
	isRegular := true
	if is, ok := pd["is"].([]any); ok && len(is) > 0 {
		if s, ok := is[0].(string); ok {
			isRegular = s == "REGULAR"
		}
	}

	return map[string]any{
		"id":                int64(periodID),
		"lessonId":          int64(lessonID),
		"lessonNumber":      0,
		"lessonCode":        "LESSON",
		"lessonText":        textField(pd, "lesson"),
		"periodText":        textField(pd, "period"),
		"hasPeriodText":     textField(pd, "period") != "",
		"periodInfo":        textField(pd, "info"),
		"periodAttachments": []any{},
		"substText":         textField(pd, "substitution"),
		"date":              ymdInt(stT),
		"startTime":         hmInt(stT),
		"endTime":           hmInt(enT),
		"elements":          els,
		"hasInfo":           textField(pd, "info") != "",
		"code":              0,
		"cellState":         "STANDARD",
		"priority":          5,
		"is":                map[string]any{"standard": isRegular, "event": false},
		"roomCapacity":      0,
		"studentCount":      0,
		"debugInfo":         fmt.Sprintf("%d,0,0/%d,0", int64(lessonID), int64(periodID)),
	}, ids, nil
}

func textField(pd map[string]any, field string) string {
	if m, ok := pd["text"].(map[string]any); ok {
		if v, ok := m[field].(string); ok {
			return v
		}
	}
	return ""
}

func ymdInt(t time.Time) int {
	return t.Year()*10000 + int(t.Month())*100 + t.Day()
}

func hmInt(t time.Time) int {
	return t.Hour()*100 + t.Minute()
}
