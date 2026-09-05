package proxy

import (
	"encoding/base64"
	"encoding/json"
	"log"
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
	Admin  []string
}

type Proxy struct {
	store    *store.Store
	untis    *untis.Client
	sessions *session.Manager
	opts     Options
	tt       *ttCache

	secretsMu sync.Mutex
	secrets   map[string]string

	mdJSONMu sync.Mutex
	mdJSON   []byte

	hub *notifyHub

	// schools holds per-school live state (klasses, recon, master data).
	schoolsMu   sync.Mutex
	schools     map[string]*schoolState
	reconActive map[string]bool
}

// schoolState is the per-school live state (caches included) so a single
// process can serve many Untis schools with independent pooling, recon and
// master data.
type schoolState struct {
	klMu    sync.Mutex
	klasses map[int64]string
	klAt    time.Time

	recon *elementDB

	mdMu   sync.Mutex
	md     *masterDataCache
	mdNext time.Time
}

type schoolKey string

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
		secrets:  map[string]string{},
		hub:      newNotifyHub(),
		schools:  map[string]*schoolState{},
	}
}

// stateFor returns the per-school live state, creating it on first use and
// registering the school in the DB. Request paths that resolve `school` call
// this so a brand-new school (login from a school never seen before)
// auto-registers with its own pool, recon and master data.
func (p *Proxy) stateFor(school string) *schoolState {
	if school == "" {
		school = p.opts.School
	}
	p.schoolsMu.Lock()
	defer p.schoolsMu.Unlock()
	st, ok := p.schools[school]
	if ok {
		return st
	}
	st = &schoolState{klasses: map[int64]string{}}
	st.recon = newElementDB()
	p.schools[school] = st
	if elems, err := p.store.LoadReconElements(school); err == nil && len(elems) > 0 {
		st.recon.seedFrom(elems)
	}
	_ = p.store.UpsertSchool(school)
	log.Printf("[multi-school] auto-registered school %q", school)
	return st
}

// isNewSchool reports whether a school has never been registered before,
// without registering it (login handlers use this to decide whether to kick
// off a fresh recon scan).
func (p *Proxy) isNewSchool(school string) bool {
	known, err := p.store.KnownSchool(school)
	return err == nil && !known
}

// schoolYearRange mirrors the server's schoolYear helper so login-triggered
// recon scans (and any future per-school scans) use the same German school year
// bounds as the boot-time scan.
func schoolYearRange(now time.Time) (string, string) {
	start := time.Date(now.Year(), time.August, 1, 0, 0, 0, 0, now.Location())
	if now.Month() < time.August {
		start = start.AddDate(-1, 0, 0)
	}
	end := start.AddDate(1, 0, 0).AddDate(0, 0, -1)
	return start.Format("2006-01-02"), end.Format("2006-01-02")
}

// ensureReconScan starts a per-school background recon enumeration if the
// school's pool is non-empty. It is safe to call concurrently and idempotent
// per school. login/augment paths call it so a school that logs in for the
// first time gets its teacher/room/subject set populated automatically.
func (p *Proxy) ensureReconScan(school string) {
	classes, err := p.store.Pool(school)
	if err != nil || len(classes) == 0 {
		return
	}
	if school == p.opts.School {
		return // owned by StartRecon at boot
	}
	p.schoolsMu.Lock()
	_, active := p.reconActive[school]
	if !active {
		if p.reconActive == nil {
			p.reconActive = map[string]bool{}
		}
		p.reconActive[school] = true
	}
	p.schoolsMu.Unlock()
	if active {
		return
	}
	now := time.Now()
	ys, ye := schoolYearRange(now)
	log.Printf("[multi-school] starting recon for newly-logged-in school %q", school)
	p.StartRecon(school, ys, ye)
}

// masterData returns cached masterData name maps, refreshing them at most once
// an hour via any pool account.
func (p *Proxy) masterData(school string) *masterDataCache {
	st := p.stateFor(school)
	st.mdMu.Lock()
	defer st.mdMu.Unlock()
	if st.md != nil && time.Now().Before(st.mdNext) {
		return st.md
	}
	u, err := p.store.AnyUser(school)
	if err != nil || u == nil {
		return st.md
	}
	cookie, err := p.untis.Session(school, u.Username, u.Password, u.Method)
	if err != nil {
		return st.md
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
		return st.md
	}
	b, _, _, err := p.untis.RawIntern(school, cookie, "getUserData2017", newBody)
	if err != nil {
		return st.md
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
		return st.md
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
	st.md = md
	st.mdNext = time.Now().Add(time.Hour)
	// Persist names to DB for CLI fuzzy lookup (fire-and-forget).
	_ = p.store.SaveMasterNames(school, "TEACHER", md.teachers)
	_ = p.store.SaveMasterNames(school, "ROOM", md.rooms)
	_ = p.store.SaveMasterNames(school, "SUBJECT", md.subjects)
	_ = p.store.SaveMasterNames(school, "CLASS", md.klassen)
	return p.stateFor(school).md
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
	mux.HandleFunc("/api/webhooks", p.handleSubsWebhooks)
	mux.HandleFunc("/api/webhooks/", p.handleSubsWebhooks)
	mux.HandleFunc("/api/ntfy", p.handleSubsNtfy)
	mux.HandleFunc("/api/ntfy/", p.handleSubsNtfy)
	mux.HandleFunc("/admin", p.handleAdminDashboard)
	mux.HandleFunc("/admin/", p.handleAdmin)
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
	st := p.stateFor(school)
	st.klMu.Lock()
	defer st.klMu.Unlock()
	if classID == 0 {
		return ""
	}
	if name, ok := st.klasses[classID]; ok && !p.klassesExpired(st.klAt) {
		return name
	}
	p.refreshKlassesLocked(st, school, cookie)
	return st.klasses[classID]
}

// hasPerm reports whether a user holds an explicit per-user permission feature
// (ignoring global switches).
func (p *Proxy) hasPerm(username, feature string) bool {
	ok, _ := p.store.HasPerm(username, feature)
	return ok
}

// isAdmin reports whether a user holds the admin flag (DB `admin` column).
// The -admin flag seeds these rows once at boot.
func (p *Proxy) isAdmin(username string) bool {
	ok, _ := p.store.IsAdmin(username)
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

func (p *Proxy) klassesExpired(at time.Time) bool {
	return time.Since(at) > time.Hour
}

func (p *Proxy) refreshKlassesLocked(st *schoolState, school, cookie string) {
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
		if u, _ := p.store.AnyUser(school); u != nil {
			if ck, err := p.untis.Session(school, u.Username, u.Password, u.Method); err == nil {
				refresh(ck)
			}
		}
	}
	if len(m) > 0 {
		st.klasses = m
		st.klAt = time.Now()
	}
}
