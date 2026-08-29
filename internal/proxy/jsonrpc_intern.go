package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

func (p *Proxy) handleJSONRPCIntern(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	log.Printf("[intern] %s?%s body=%s", r.URL.Path, r.URL.RawQuery, string(body))
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.Unmarshal(body, &req)
	school := r.URL.Query().Get("school")
	if school == "" {
		school = p.opts.School
	}

	switch req.Method {
	case "getUserData2017":
		p.keyLogin(w, r, school, body)
	case "getTimetable2017":
		p.getTimetable2017(w, r, school, req.ID, body)
	case "getAppSharedSecret":
		b, status, _, err := p.untis.RawIntern(school, "", "getAppSharedSecret", body)
		if err != nil {
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		var req struct {
			Params []struct {
				UserName string `json:"userName"`
				Password string `json:"password"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		var resp struct {
			Result string `json:"result"`
		}
		_ = json.Unmarshal(b, &resp)
		secret := resp.Result
		if secret == "" && len(req.Params) > 0 {
			// Some apps send the shared secret itself as the "password" (the
			// real server rejects that with -8998). If the value is a valid
			// base32 secret, capture it so we can replay the account.
			if pw := req.Params[0].Password; isBase32Secret(pw) {
				secret = pw
			}
		}
		if secret != "" && len(req.Params) > 0 && req.Params[0].UserName != "" {
			p.secretsMu.Lock()
			p.secrets[strings.ToLower(req.Params[0].UserName)] = secret
			p.secretsMu.Unlock()
			_ = p.store.UpsertSecret(req.Params[0].UserName, secret)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(b)
	default:
		// Info-center style methods (absences, events, messages, ...) are
		// self-authenticating: BetterUntis includes an auth block per request.
		// Forward as-is so the real server validates the OTP and returns only
		// that user's own data.
		if hasAuthBlock(body) {
			m := r.URL.Query().Get("m")
			if m == "" {
				m = req.Method
			}
			b, status, _, err := p.untis.RawIntern(school, "", m, body)
			if err != nil {
				p.writeJSONRPCError(w, req.ID, "upstream error", -1)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(b)
			return
		}
		user := p.sessionUser(r)
		if user == nil {
			p.writeJSONRPCError(w, req.ID, "not logged in", -8520)
			return
		}
		cookie, err := p.untis.Session(school, user.Username, user.Password, user.Method)
		if err != nil {
			p.writeJSONRPCError(w, req.ID, "not logged in", -8520)
			return
		}
		m := r.URL.Query().Get("m")
		if m == "" {
			m = req.Method
		}
		b, status, _, err := p.untis.RawIntern(school, cookie, m, body)
		if err != nil {
			p.writeJSONRPCError(w, req.ID, "upstream error", -1)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(b)
	}
}

// hasAuthBlock reports whether a jsonrpc_intern.do request body carries the
// self-authentication block BetterUntis sends on every request.
func hasAuthBlock(body []byte) bool {
	var req struct {
		Params []struct {
			Auth struct {
				User string `json:"user"`
				OTP  any    `json:"otp"`
			} `json:"auth"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return false
	}
	for _, pr := range req.Params {
		if pr.Auth.User != "" {
			return true
		}
	}
	return false
}

func isBase32Secret(s string) bool {
	if len(s) < 16 || len(s)%8 == 4 || len(s)%8 == 1 || len(s)%8 == 5 {
		return false
	}
	for _, c := range s {
		if c == '=' {
			continue
		}
		if c < 'A' || c > 'Z' {
			if c < '2' || c > '7' {
				return false
			}
		}
	}
	return true
}

func (p *Proxy) keyLogin(w http.ResponseWriter, r *http.Request, school string, body []byte) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Params []struct {
			Auth struct {
				User       string          `json:"user"`
				OTP        json.RawMessage `json:"otp"`
				ClientTime int64           `json:"clientTime"`
				Key        string          `json:"key"`
			} `json:"auth"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Params) == 0 {
		p.writeJSONRPCError(w, req.ID, "no username specified", -8502)
		return
	}
	auth := req.Params[0].Auth
	if auth.User == "" {
		p.writeJSONRPCError(w, req.ID, "no username specified", -8502)
		return
	}
	otp := strings.TrimSpace(string(auth.OTP))
	if strings.HasPrefix(otp, "\"") {
		if s, err := strconv.Unquote(otp); err == nil {
			otp = s
		}
	}
	extra := map[string]any{}
	replayKey := auth.Key
	if replayKey == "" {
		replayKey = p.secrets[strings.ToLower(auth.User)]
	}
	if replayKey == "" {
		replayKey, _ = p.store.GetSecret(auth.User)
	}
	if replayKey != "" {
		extra["key"] = replayKey
	}
	cookie, realBody, err := p.untis.KeyLogin(school, auth.User, otp, auth.ClientTime, extra)
	if err != nil {
		if ue, ok := err.(*untis.UpstreamError); ok {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(ue.Raw)
			return
		}
		p.writeJSONRPCError(w, req.ID, "bad credentials", -8504)
		return
	}

	isAnon := auth.User == "#anonymous#"
	info, _ := p.untis.PersonInfo(school, cookie)
	if replayKey == "" {
		if existing, err := p.store.GetUser(auth.User); err == nil && existing != nil && existing.Password != "" {
			replayKey = existing.Password
		}
	}
	// Donation rule: students donate their class to the pool automatically;
	// non-students donate only when granted god-api. Everyone is still stored
	// (for stock passthrough + session), but non-donors have class_id=0 so
	// Pool()/PoolContains()/OwnerForClass stop counting them.
	donateClassID := info.ClassID
	if info.PersonType != 5 && !p.isGod(auth.User) {
		donateClassID = 0
	}
	user := &store.User{
		Username:    auth.User,
		Password:    replayKey,
		Method:      "key",
		PersonID:    info.PersonID,
		PersonType:  info.PersonType,
		ClassID:     donateClassID,
		ClassName:   p.classNameFor(school, cookie, info.ClassID),
		Email:       info.Email,
		DisplayName: info.DisplayName,
	}
	// The official anonymous login is only a session: don't persist
	// "#anonymous#" as a pool account or owner.
	if !isAnon {
		_ = p.store.UpsertUser(user)
		_ = p.store.Touch(auth.User)
	}
	go p.untis.Logout(school, cookie)

	s := p.sessions.New(auth.User, info.ClassID)
	p.setSessionCookies(w, s.ID, school)
	rewritten := p.markPooledElementsDisplayable(realBody, auth.User)
	p.mdJSONMu.Lock()
	p.mdJSON = rewritten
	p.mdJSONMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(rewritten)
}

// markPooledElementsDisplayable rewrites the masterData lists in a
// getUserData2017 response so pooled classes are displayable and teachers/rooms/
// subjects the requesting user is allowed to reconstruct are displayAllowed.
// Element reconstruction access is granted per type (TEACHER/ROOM/SUBJECT) via
// a per-user override or the global switch; un-granted types stay hidden.
func (p *Proxy) markPooledElementsDisplayable(body []byte, username string) []byte {
	var resp struct {
		Result struct {
			MasterData struct {
				Klassen []struct {
					ID          int64 `json:"id"`
					Displayable bool  `json:"displayable"`
				} `json:"klassen"`
				Teachers []struct {
					ID             int64 `json:"id"`
					DisplayAllowed bool  `json:"displayAllowed"`
				} `json:"teachers"`
				Rooms []struct {
					ID             int64 `json:"id"`
					DisplayAllowed bool  `json:"displayAllowed"`
				} `json:"rooms"`
				Subjects []struct {
					ID             int64 `json:"id"`
					DisplayAllowed bool  `json:"displayAllowed"`
				} `json:"subjects"`
			} `json:"masterData"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Result.MasterData.Klassen == nil {
		return body
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	pooled := map[int64]bool{}
	for _, k := range resp.Result.MasterData.Klassen {
		ok, err := p.store.PoolContains(k.ID)
		if err == nil && ok {
			pooled[k.ID] = true
		}
	}
	if len(pooled) == 0 {
		return body
	}
	result, _ := m["result"].(map[string]any)
	md, _ := result["masterData"].(map[string]any)

	setDisplayable := func(listKey string, want func(id int64) bool, flag string) {
		items, _ := md[listKey].([]any)
		for _, item := range items {
			ci, ok := item.(map[string]any)
			if !ok {
				continue
			}
			idf, ok := ci["id"].(float64)
			if !ok {
				continue
			}
			if want(int64(idf)) {
				ci[flag] = true
			}
		}
	}
	setDisplayable("klassen", func(id int64) bool { return pooled[id] }, "displayable")
	can := map[string]bool{}
	for _, t := range []string{"TEACHER", "ROOM", "SUBJECT"} {
		ok, _ := p.store.ReconAccess(username, t)
		can[t] = ok
	}
	setDisplayable("teachers", func(id int64) bool {
		return can["TEACHER"] && p.recon.has("TEACHER", id)
	}, "displayAllowed")
	setDisplayable("rooms", func(id int64) bool {
		return can["ROOM"] && p.recon.has("ROOM", id)
	}, "displayAllowed")
	setDisplayable("subjects", func(id int64) bool {
		return can["SUBJECT"] && p.recon.has("SUBJECT", id)
	}, "displayAllowed")

	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func (p *Proxy) getTimetable2017(w http.ResponseWriter, r *http.Request, school string, id json.RawMessage, body []byte) {
	var req struct {
		Params []struct {
			ID        int64  `json:"id"`
			Type      string `json:"type"`
			StartDate string `json:"startDate"`
			EndDate   string `json:"endDate"`
			Auth      struct {
				User string `json:"user"`
			} `json:"auth"`
		} `json:"params"`
	}
	_ = json.Unmarshal(body, &req)
	if len(req.Params) == 0 {
		p.writeJSONRPCError(w, id, "no username specified", -8502)
		return
	}
	pr := req.Params[0]

	// BetterUntis self-authenticates every request via the auth block, so the
	// requester identity comes from there when present, else the session cookie.
	requesterName := pr.Auth.User
	if requesterName == "" {
		if u := p.sessionUser(r); u != nil {
			requesterName = u.Username
		}
	}
	if requesterName == "" {
		p.writeJSONRPCError(w, id, "not logged in", -8520)
		return
	}
	requester, err := p.store.GetUser(requesterName)
	if err != nil || requester == nil {
		p.writeJSONRPCError(w, id, "not logged in", -8520)
		return
	}

	classID := int64(0)
	switch pr.Type {
	case "STUDENT":
		u, err := p.store.UserByPersonID(pr.ID)
		if err == nil && u != nil {
			classID = u.ClassID
		}
	case "CLASS":
		classID = pr.ID
	case "TEACHER", "ROOM", "SUBJECT":
		p.serveElementTimetable(w, r, school, id, pr)
		return
	default:
		b, status, _, err := p.untis.RawIntern(school, "", "getTimetable2017", body)
		if err != nil {
			p.writeJSONRPCError(w, id, "no right for timetable", -8509)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(b)
		return
	}

	if classID == 0 {
		p.writeJSONRPCError(w, id, "no right for timetable", -8509)
		return
	}
	ok, err := p.store.PoolContains(classID)
	if err != nil || !ok {
		p.writeJSONRPCError(w, id, "no right for timetable", -8509)
		return
	}
	owner, err := p.store.OwnerForClass(classID)
	if err != nil || owner == nil {
		p.writeJSONRPCError(w, id, "no right for timetable", -8509)
		return
	}

	key := fmt.Sprintf("2017|%d|%s|%s", classID, pr.StartDate, pr.EndDate)
	if false {
		if b, ok := p.tt.Get(key); ok {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
			return
		}
	}

	newBody := body
	if requester.Username != owner.Username {
		newBody, err = p.rewriteAuthForOwner(school, body, owner)
		if err != nil {
			p.writeJSONRPCError(w, id, "no right for timetable", -8509)
			return
		}
	}
	b, status, _, err := p.untis.RawIntern(school, "", "getTimetable2017", newBody)
	if err != nil {
		p.writeJSONRPCError(w, id, "no right for timetable", -8509)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// serveElementTimetable answers a TEACHER/ROOM/SUBJECT getTimetable2017 request
// by reconstructing the element's schedule from the pooled classes' timetables.
// Only elements that appear in pooled data are served; everything else gets the
// same -8509 the real server would return.
func (p *Proxy) serveElementTimetable(w http.ResponseWriter, r *http.Request, school string, id json.RawMessage, pr struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	StartDate string `json:"startDate"`
	EndDate   string `json:"endDate"`
	Auth      struct {
		User string `json:"user"`
	} `json:"auth"`
}) {
	requesterName := pr.Auth.User
	if requesterName == "" {
		if u := p.sessionUser(r); u != nil {
			requesterName = u.Username
		}
	}
	if requesterName == "" {
		p.writeJSONRPCError(w, id, "not logged in", -8520)
		return
	}
	requester, err := p.store.GetUser(requesterName)
	if err != nil || requester == nil {
		p.writeJSONRPCError(w, id, "not logged in", -8520)
		return
	}
	// Reconstructed teacher/room/subject timetables are gated per element type
	// (individual override or global switch); nothing is enabled by default.
	allowed, _ := p.store.ReconAccess(requester.Username, pr.Type)
	if !allowed {
		p.writeJSONRPCError(w, id, "no right for timetable", -8509)
		return
	}
	if pr.StartDate == "" || pr.EndDate == "" {
		p.writeJSONRPCError(w, id, "no right for timetable", -8509)
		return
	}
	if !p.recon.has(pr.Type, pr.ID) {
		p.writeJSONRPCError(w, id, "no right for timetable", -8509)
		return
	}
	periods, err := p.elementPeriods(school, pr.Type, pr.ID, pr.StartDate, pr.EndDate)
	if err != nil {
		p.writeJSONRPCError(w, id, "no right for timetable", -8509)
		return
	}
	out := map[string]any{
		"jsonrpc": "2.0",
		"id":      idVal(id),
		"result": map[string]any{
			"timetable": map[string]any{
				"displayableStartDate": pr.StartDate,
				"displayableEndDate":   pr.EndDate,
				"periods":              periods,
			},
		},
	}
	// The real server always includes result.masterData (with a fresh timeStamp)
	// in getTimetable2017 responses; the app uses it to detect that its cached
	// masterData is stale and refetch it. Without this the app keeps its old
	// cached copy where rooms/teachers have displayAllowed=false, so they never
	// appear. Embed the rewritten masterData with a bumped timestamp instead.
	if md := p.rewrittenMasterData(); md != nil {
		out["result"].(map[string]any)["masterData"] = md
	}
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(out)
	_, _ = w.Write(b)
}

// rewrittenMasterData returns the cached, displayAllowed-rewritten masterData
// with its timeStamp bumped so clients treat it as newer than anything they
// cached before.
func (p *Proxy) rewrittenMasterData() map[string]any {
	p.mdJSONMu.Lock()
	defer p.mdJSONMu.Unlock()
	if len(p.mdJSON) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(p.mdJSON, &m) != nil {
		return nil
	}
	res, _ := m["result"].(map[string]any)
	if res == nil {
		return nil
	}
	md, _ := res["masterData"].(map[string]any)
	if md == nil {
		return nil
	}
	md["timeStamp"] = time.Now().UnixMilli()
	return md
}

// rewriteAuthForOwner replaces the auth block in a getTimetable2017 request with
// a fresh OTP for the class owner, so the real server serves the class timetable.
func (p *Proxy) rewriteAuthForOwner(school string, body []byte, owner *store.User) ([]byte, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	params, ok := req["params"].([]any)
	if !ok || len(params) == 0 {
		return nil, fmt.Errorf("no params")
	}
	p0, ok := params[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("bad param")
	}

	var otp, secret string
	switch owner.Method {
	case "key":
		secret = owner.Password
		if secret == "" {
			secret, _ = p.store.GetSecret(owner.Username)
		}
	default:
		secBody, _ := json.Marshal(map[string]any{
			"id": "untis-proxy", "jsonrpc": "2.0", "method": "getAppSharedSecret",
			"params": []any{map[string]any{"userName": owner.Username, "password": owner.Password}},
		})
		b, _, _, err := p.untis.RawIntern(school, "", "getAppSharedSecret", secBody)
		if err != nil {
			return nil, err
		}
		var resp struct {
			Result string `json:"result"`
		}
		if err := json.Unmarshal(b, &resp); err != nil || resp.Result == "" {
			return nil, fmt.Errorf("no shared secret for %s", owner.Username)
		}
		secret = resp.Result
	}
	otp = untis.TOTP(secret)

	p0["auth"] = map[string]any{
		"user":       owner.Username,
		"otp":        otp,
		"clientTime": time.Now().UnixMilli(),
	}
	return json.Marshal(req)
}
