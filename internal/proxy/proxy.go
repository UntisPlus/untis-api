package proxy

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"untis-proxy/internal/session"
	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

type Options struct {
	School string
	TTL    time.Duration
}

type Proxy struct {
	store    *store.Store
	untis    *untis.Client
	sessions *session.Manager
	opts     Options
	tt       *ttCache

	klMu    sync.Mutex
	klasses map[int64]string
	klAt    time.Time

	secretsMu sync.Mutex
	secrets   map[string]string

	recon *elementDB

	mdMu   sync.Mutex
	md     *masterDataCache
	mdNext time.Time

	mdJSONMu sync.Mutex
	mdJSON   []byte

	hub *notifyHub
}

// masterDataCache holds name lookups from getUserData2017 masterData, used to
// render the weekly REST response's element descriptors.
type masterDataCache struct {
	teachers map[int64]string
	rooms    map[int64]string
	subjects map[int64]string
	klassen  map[int64]string
}

func New(st *store.Store, uc *untis.Client, sm *session.Manager, opts Options) *Proxy {
	if opts.TTL <= 0 {
		opts.TTL = 5 * time.Minute
	}
	return &Proxy{
		store:    st,
		untis:    uc,
		sessions: sm,
		opts:     opts,
		tt:       newTTCache(opts.TTL),
		klasses:  map[int64]string{},
		secrets:  map[string]string{},
		recon:    newElementDB(),
		md:       &masterDataCache{},
		hub:      newNotifyHub(),
	}
}

// masterData returns cached masterData name maps, refreshing them at most once
// an hour via any pool account.
func (p *Proxy) masterData(school string) *masterDataCache {
	p.mdMu.Lock()
	defer p.mdMu.Unlock()
	if p.md != nil && time.Now().Before(p.mdNext) {
		return p.md
	}
	u, err := p.store.AnyUser()
	if err != nil || u == nil {
		return p.md
	}
	cookie, err := p.untis.Session(school, u.Username, u.Password, u.Method)
	if err != nil {
		return p.md
	}
	body, _ := json.Marshal(map[string]any{
		"id": "untis-proxy-md", "jsonrpc": "2.0", "method": "getUserData2017",
		"params": []any{map[string]any{
			"elementId": 0, "deviceOs": "AND", "deviceOsVersion": "",
			"auth": map[string]any{"user": u.Username},
		}},
	})
	newBody, err := p.rewriteAuthForOwner(school, body, u)
	if err != nil {
		return p.md
	}
	b, _, _, err := p.untis.RawIntern(school, cookie, "getUserData2017", newBody)
	if err != nil {
		return p.md
	}
	var resp struct {
		Result struct {
			MasterData struct {
				Teachers []struct {
					ID   int64  `json:"id"`
					Name string `json:"name"`
				} `json:"teachers"`
				Rooms []struct {
					ID   int64  `json:"id"`
					Name string `json:"name"`
				} `json:"rooms"`
				Subjects []struct {
					ID   int64  `json:"id"`
					Name string `json:"name"`
				} `json:"subjects"`
				Klassen []struct {
					ID   int64  `json:"id"`
					Name string `json:"name"`
				} `json:"klassen"`
			} `json:"masterData"`
		} `json:"result"`
	}
	if json.Unmarshal(b, &resp) != nil {
		return p.md
	}
	md := &masterDataCache{
		teachers: map[int64]string{},
		rooms:    map[int64]string{},
		subjects: map[int64]string{},
		klassen:  map[int64]string{},
	}
	for _, t := range resp.Result.MasterData.Teachers {
		md.teachers[t.ID] = t.Name
	}
	for _, r := range resp.Result.MasterData.Rooms {
		md.rooms[r.ID] = r.Name
	}
	for _, s := range resp.Result.MasterData.Subjects {
		md.subjects[s.ID] = s.Name
	}
	for _, k := range resp.Result.MasterData.Klassen {
		md.klassen[k.ID] = k.Name
	}
	p.md = md
	p.mdNext = time.Now().Add(time.Hour)
	// Persist names to DB for CLI fuzzy lookup (fire-and-forget).
	_ = p.store.SaveMasterNames("TEACHER", md.teachers)
	_ = p.store.SaveMasterNames("ROOM", md.rooms)
	_ = p.store.SaveMasterNames("SUBJECT", md.subjects)
	_ = p.store.SaveMasterNames("CLASS", md.klassen)
	return p.md
}

func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/WebUntis/jsonrpc.do", p.handleJSONRPC)
	mux.HandleFunc("/WebUntis/jsonrpc_intern.do", p.handleJSONRPCIntern)
	mux.HandleFunc("/WebUntis/api/", p.handleREST)
	mux.HandleFunc("/status", p.handleStatus)
	mux.HandleFunc("/me", p.handleMe)
	mux.HandleFunc("POST /api/calendar/token", p.handleCalendarToken)
	mux.HandleFunc("GET /api/calendar/{token}", p.handleCalendarICS)
	mux.HandleFunc("GET /api/timetable/changes", p.handleTimetableChanges)
	mux.HandleFunc("GET /api/timetable/stream", p.handleTimetableStream)
	return mux
}

func idVal(id json.RawMessage) any {
	if len(id) == 0 || string(id) == "null" {
		return nil
	}
	var v any
	if json.Unmarshal(id, &v) == nil {
		return v
	}
	return string(id)
}

func (p *Proxy) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(v)
	_, _ = w.Write(b)
}

func (p *Proxy) writeJSONRPCError(w http.ResponseWriter, id json.RawMessage, message string, code int) {
	m := map[string]any{
		"jsonrpc": "2.0",
		"id":      idVal(id),
		"error":   map[string]any{"message": message, "code": code},
	}
	p.writeJSON(w, m)
}

func (p *Proxy) sessionUser(r *http.Request) *store.User {
	ck, err := r.Cookie("JSESSIONID")
	if err != nil || ck.Value == "" {
		return nil
	}
	s := p.sessions.Get(ck.Value)
	if s == nil {
		return nil
	}
	u, err := p.store.GetUser(s.Username)
	if err != nil || u == nil {
		return nil
	}
	return u
}

func (p *Proxy) setSessionCookies(w http.ResponseWriter, sid, school string) {
	http.SetCookie(w, &http.Cookie{
		Name: "JSESSIONID", Value: sid, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteNoneMode,
	})
	sc := "_" + base64.StdEncoding.EncodeToString([]byte(school))
	http.SetCookie(w, &http.Cookie{Name: "schoolname", Value: sc, Path: "/", MaxAge: 1209600})
}

func (p *Proxy) classNameFor(school, cookie string, classID int64) string {
	p.klMu.Lock()
	defer p.klMu.Unlock()
	if classID == 0 {
		return ""
	}
	if name, ok := p.klasses[classID]; ok && !p.klassesExpired() {
		return name
	}
	p.refreshKlassesLocked(school, cookie)
	return p.klasses[classID]
}

// hasPerm reports whether a user holds an explicit per-user permission feature
// (ignoring global switches).
func (p *Proxy) hasPerm(username, feature string) bool {
	ok, _ := p.store.HasPerm(username, feature)
	return ok
}

// isBoosted reports whether a user effectively gets Boosted raw timetable
// forwarding (holds the boosted flag).
func (p *Proxy) isBoosted(username string) bool {
	ok, _ := p.store.BoostedAccess(username)
	return ok
}

// isEditor reports whether a user holds the editor flag (absence/lesson/subject
// write methods).
func (p *Proxy) isEditor(username string) bool {
	ok, _ := p.store.EditorAccess(username)
	return ok
}

// isWriteMethod reports whether a JSON-RPC method is a lesson/subject/absence
// write (mutation) method, gated by the editor flag. Reading your own absences
// is always allowed.
func isWriteMethod(method string) bool {
	for _, prefix := range []string{"set", "add", "update", "delete", "change"} {
		if strings.HasPrefix(strings.ToLower(method), prefix) {
			return true
		}
	}
	return false
}

func (p *Proxy) klassesExpired() bool {
	return time.Since(p.klAt) > time.Hour
}

func (p *Proxy) refreshKlassesLocked(school, cookie string) {
	m := map[int64]string{}
	refresh := func(ck string) {
		b, err := p.untis.GetKlassen(school, ck)
		if err != nil {
			return
		}
		var res struct {
			Result []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"result"`
		}
		if json.Unmarshal(b, &res) != nil {
			return
		}
		for _, c := range res.Result {
			m[c.ID] = c.Name
		}
	}
	if cookie != "" {
		refresh(cookie)
	}
	if len(m) == 0 {
		if u, _ := p.store.AnyUser(); u != nil {
			if ck, err := p.untis.Session(school, u.Username, u.Password, u.Method); err == nil {
				refresh(ck)
			}
		}
	}
	if len(m) > 0 {
		p.klasses = m
		p.klAt = time.Now()
	}
}
