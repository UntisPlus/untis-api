package proxy

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"untis-proxy/internal/store"
)

// adminDashboardHTML is the embedded single-page admin UI served at /admin.
//
//go:embed static/admin.html
var adminDashboardHTML string

// handleAdminDashboard serves the single-page admin UI. It requires an admin
// session; the page itself talks to the JSON /admin/* endpoints in the browser
// so the server never mixes HTML into the API.
func (p *Proxy) handleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	u := p.sessionUser(r)
	if u == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if !p.isAdmin(u.Username) {
		p.forbidden(w)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(adminDashboardHTML))
}

// handleAdmin is the HTTP root for the admin API. Every route requires an
// admin session (users.admin flag). It exposes everything untisctl can do —
// users, pool, permissions, tokens, schools, webhooks and ntfy topics — as
// JSON for the /admin dashboard (and for scripting). Requests that hit this
// handler arrive via /admin/ (the trailing slash), which never serves HTML.
func (p *Proxy) handleAdmin(w http.ResponseWriter, r *http.Request) {
	u := p.sessionUser(r)
	if u == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if !p.isAdmin(u.Username) {
		p.forbidden(w)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/admin")
	path = strings.Trim(path, "/")
	parts := strings.Split(path, "/")

	switch {
	case len(parts) == 1 && parts[0] == "status" && r.Method == http.MethodGet:
		p.adminStatus(w)
	case parts[0] == "users":
		p.adminUsers(w, r, parts[1:])
	case parts[0] == "perms" && r.Method == http.MethodGet:
		p.adminPerms(w)
	case parts[0] == "pool":
		p.adminPool(w, r, parts[1:])
	case parts[0] == "tokens":
		p.adminTokens(w, r, parts[1:])
	case parts[0] == "schools":
		p.adminSchools(w, r, parts[1:])
	case parts[0] == "webhooks":
		p.adminWebhooks(w, r, parts[1:])
	case parts[0] == "ntfy":
		p.adminNtfy(w, r, parts[1:])
	case parts[0] == "recon":
		p.adminRecon(w, r, parts[1:])
	default:
		p.writeJSON(w, map[string]any{"error": "not found"})
	}
}

func (p *Proxy) adminStatus(w http.ResponseWriter) {
	users, _ := p.store.ListUsers()
	userCount := len(users)
	pool, _ := p.store.Pool("")
	schools, _ := p.store.ListSchools()
	tokens, _ := p.store.ListClassTokens()
	perms, _ := p.store.AllPerms()
	webhooks, _ := p.store.ListWebhooks("")
	ntfy, _ := p.store.ListNtfyTopics("")

	var adminCount, boosted, editor, recon int
	for _, us := range users {
		if us.Admin {
			adminCount++
		}
		if p.isBoosted(us.Username) {
			boosted++
		}
		if p.isEditor(us.Username) {
			editor++
		}
		if p.hasPerm(us.Username, store.FeatureRecon) {
			recon++
		}
	}

	global := map[string]bool{}
	for _, pr := range perms {
		if pr.Username == store.GlobalPermUser() {
			global[pr.Feature] = pr.Allowed
		}
	}

	p.writeJSON(w, map[string]any{
		"version":    p.opts.School,
		"school":     p.opts.School,
		"users":      userCount,
		"admins":     adminCount,
		"boosted":    boosted,
		"editors":    editor,
		"recon":      recon,
		"pool":       len(pool),
		"schools":    len(schools),
		"tokens":     len(tokens),
		"webhooks":   len(webhooks),
		"ntfyTopics": len(ntfy),
		"global":     global,
	})
}

func (p *Proxy) adminPerms(w http.ResponseWriter) {
	perms, err := p.store.AllPerms()
	if err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	p.writeJSON(w, map[string]any{"perms": perms})
}

func (p *Proxy) adminPool(w http.ResponseWriter, r *http.Request, parts []string) {
	var school string
	if len(parts) > 0 && parts[0] != "" {
		school = parts[0]
	}
	classes, err := p.store.Pool(school)
	if err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	type poolEntry struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Owner string `json:"owner"`
	}
	out := make([]poolEntry, 0, len(classes))
	for _, c := range classes {
		owner, _ := p.store.OwnerForClass(school, c.ID)
		ownerName := ""
		if owner != nil {
			ownerName = owner.Username
		}
		out = append(out, poolEntry{c.ID, c.Name, ownerName})
	}
	p.writeJSON(w, map[string]any{"school": school, "pool": out})
}

func (p *Proxy) adminTokens(w http.ResponseWriter, r *http.Request, parts []string) {
	if r.Method == http.MethodDelete && len(parts) == 1 {
		n, err := p.store.DeleteClassToken(parts[0])
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.writeJSON(w, map[string]any{"revoked": n > 0})
		return
	}
	// GET /admin/tokens and GET /admin/tokens/{school}
	school := ""
	if len(parts) == 1 && parts[0] != "" {
		school = parts[0]
	}
	tokens, err := p.store.ListClassTokens()
	if err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	if school != "" {
		var filtered []*store.ClassToken
		for _, t := range tokens {
			if t.School == school {
				filtered = append(filtered, t)
			}
		}
		tokens = filtered
	}
	type tokenOut struct {
		Token       string `json:"token"`
		School      string `json:"school"`
		ClassID     int64  `json:"classId"`
		ElementType string `json:"elementType"`
		ElementID   int64  `json:"elementId"`
		Days        int    `json:"days"`
		LastAccess  int64  `json:"lastAccess"`
	}
	out := make([]tokenOut, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, tokenOut{t.Token, t.School, t.ClassID, t.ElementType, t.ElementID, t.Days, t.LastAccess})
	}
	p.writeJSON(w, map[string]any{"tokens": out})
}

func (p *Proxy) adminSchools(w http.ResponseWriter, r *http.Request, parts []string) {
	if r.Method == http.MethodPost && len(parts) == 1 {
		_ = p.store.UpsertSchool(parts[0])
		p.stateFor(parts[0])
		p.writeJSON(w, map[string]any{"school": parts[0], "registered": true})
		return
	}
	schools, err := p.store.ListSchools()
	if err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	type schoolOut struct {
		Name     string `json:"name"`
		AddedAt  int64  `json:"addedAt"`
		LastSeen int64  `json:"lastSeen"`
	}
	out := make([]schoolOut, 0, len(schools))
	for _, sc := range schools {
		out = append(out, schoolOut{sc.Name, sc.AddedAt.Unix(), sc.LastSeen.Unix()})
	}
	p.writeJSON(w, map[string]any{"schools": out})
}

func (p *Proxy) adminWebhooks(w http.ResponseWriter, r *http.Request, parts []string) {
	if r.Method == http.MethodPost {
		var req struct {
			School  string `json:"school"`
			ClassID int64  `json:"classId"`
			URL     string `json:"url"`
			Secret  string `json:"secret"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			p.writeJSON(w, map[string]any{"error": "bad request"})
			return
		}
		if req.School == "" {
			req.School = p.opts.School
		}
		if req.URL == "" {
			p.writeJSON(w, map[string]any{"error": "url required"})
			return
		}
		id, err := p.store.AddWebhook(&store.Webhook{
			School: req.School, ClassID: req.ClassID, URL: req.URL,
			Secret: req.Secret, Enabled: true, CreatedBy: "admin",
		})
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.writeJSON(w, map[string]any{"id": id})
		return
	}
	if r.Method == http.MethodDelete && len(parts) == 1 {
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "bad id"})
			return
		}
		n, err := p.store.DeleteWebhook(id)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.writeJSON(w, map[string]any{"deleted": n > 0})
		return
	}
	// GET
	school := ""
	if len(parts) == 1 && parts[0] != "" {
		school = parts[0]
	}
	hooks, err := p.store.ListWebhooks(school)
	if err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	type hookOut struct {
		ID      int64  `json:"id"`
		School  string `json:"school"`
		ClassID int64  `json:"classId"`
		URL     string `json:"url"`
	}
	out := make([]hookOut, 0, len(hooks))
	for _, h := range hooks {
		out = append(out, hookOut{h.ID, h.School, h.ClassID, h.URL})
	}
	p.writeJSON(w, map[string]any{"webhooks": out})
}

func (p *Proxy) adminNtfy(w http.ResponseWriter, r *http.Request, parts []string) {
	if r.Method == http.MethodPost {
		var req struct {
			School  string `json:"school"`
			ClassID int64  `json:"classId"`
			Topic   string `json:"topic"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			p.writeJSON(w, map[string]any{"error": "bad request"})
			return
		}
		if req.School == "" {
			req.School = p.opts.School
		}
		if req.Topic == "" {
			p.writeJSON(w, map[string]any{"error": "topic required"})
			return
		}
		id, err := p.store.AddNtfyTopic(&store.NtfyTopic{
			School: req.School, ClassID: req.ClassID, Topic: req.Topic,
			Enabled: true, CreatedBy: "admin",
		})
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.writeJSON(w, map[string]any{"id": id})
		return
	}
	if r.Method == http.MethodDelete && len(parts) == 1 {
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "bad id"})
			return
		}
		n, err := p.store.DeleteNtfyTopic(id)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.writeJSON(w, map[string]any{"deleted": n > 0})
		return
	}
	school := ""
	if len(parts) == 1 && parts[0] != "" {
		school = parts[0]
	}
	topics, err := p.store.ListNtfyTopics(school)
	if err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	type topicOut struct {
		ID      int64  `json:"id"`
		School  string `json:"school"`
		ClassID int64  `json:"classId"`
		Topic   string `json:"topic"`
	}
	out := make([]topicOut, 0, len(topics))
	for _, n := range topics {
		out = append(out, topicOut{n.ID, n.School, n.ClassID, n.Topic})
	}
	p.writeJSON(w, map[string]any{"topics": out})
}

func (p *Proxy) adminRecon(w http.ResponseWriter, r *http.Request, parts []string) {
	school := p.opts.School
	if len(parts) == 1 && parts[0] != "" {
		school = parts[0]
	}
	st := p.stateFor(school)
	st.recon.snapshot()
	p.writeJSON(w, map[string]any{
		"school":   school,
		"teachers": len(st.recon.teachers),
		"rooms":    len(st.recon.rooms),
		"subjects": len(st.recon.subjects),
		"counts":   st.recon.counts(),
	})
}

// adminUsers handles GET /admin/users and the POST mutations:
//
//	POST /admin/users            {username, password?, method?, admin?}
//	POST /admin/users/{u}/perm   {feature, allowed}
//	POST /admin/users/{u}/admin  {admin: true|false}
//	DELETE /admin/users/{u}
func (p *Proxy) adminUsers(w http.ResponseWriter, r *http.Request, parts []string) {
	if r.Method == http.MethodGet && len(parts) == 0 {
		users, err := p.store.ListUsers()
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		permRows, _ := p.store.AllPerms()
		perms := map[string]map[string]bool{}
		for _, pr := range permRows {
			if perms[pr.Username] == nil {
				perms[pr.Username] = map[string]bool{}
			}
			perms[pr.Username][pr.Feature] = pr.Allowed
		}
		out := make([]map[string]any, 0, len(users))
		for _, u := range users {
			out = append(out, map[string]any{
				"username":     u.Username,
				"method":       u.Method,
				"personId":     u.PersonID,
				"personType":   u.PersonType,
				"classId":      u.ClassID,
				"className":    u.ClassName,
				"school":       u.School,
				"admin":        u.Admin,
				"displayName":  u.DisplayName,
				"email":        u.Email,
				"lastSeen":     u.LastSeen.Unix(),
				"permissions":  perms[u.Username],
				"boosted":      p.isBoosted(u.Username),
				"editor":       p.isEditor(u.Username),
				"reconAllowed": p.hasPerm(u.Username, store.FeatureRecon),
			})
		}
		p.writeJSON(w, map[string]any{"users": out})
		return
	}

	if r.Method == http.MethodPost && len(parts) == 0 {
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Method   string `json:"method"`
			Admin    bool   `json:"admin"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" {
			p.writeJSON(w, map[string]any{"error": "bad request"})
			return
		}
		if req.Method == "" {
			req.Method = "password"
		}
		existing, _ := p.store.GetUser(req.Username)
		if existing != nil {
			existing.Admin = req.Admin
			if req.Password != "" {
				existing.Password = req.Password
				existing.Method = req.Method
			}
			if err := p.store.UpsertUser(existing); err != nil {
				p.writeJSON(w, map[string]any{"error": "store error"})
				return
			}
		} else {
			if err := p.store.SetAdmin(req.Username, req.Admin); err != nil {
				p.writeJSON(w, map[string]any{"error": "store error"})
				return
			}
		}
		if req.Admin {
			_ = p.store.SetPerm(req.Username, store.FeatureBoosted, true)
		}
		p.writeJSON(w, map[string]any{"ok": true})
		return
	}

	if len(parts) == 1 {
		username := parts[0]
		switch {
		case r.Method == http.MethodDelete:
			n, err := p.store.DeleteUser(username)
			if err != nil {
				p.writeJSON(w, map[string]any{"error": "store error"})
				return
			}
			p.writeJSON(w, map[string]any{"deleted": n > 0})
			return
		case r.Method == http.MethodPost:
			var req map[string]any
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				p.writeJSON(w, map[string]any{"error": "bad request"})
				return
			}
			if _, ok := req["feature"]; ok {
				feature, _ := req["feature"].(string)
				allowed, _ := req["allowed"].(bool)
				var err error
				switch feature {
				case store.FeatureBoosted:
					err = p.store.SetBoostedFlag(username, allowed)
				case store.FeatureEditor:
					err = p.store.SetPerm(username, store.FeatureEditor, allowed)
				case store.FeatureRecon:
					err = p.store.SetPerm(username, store.FeatureRecon, allowed)
				default:
					err = p.store.SetPerm(username, feature, allowed)
				}
				if err != nil {
					p.writeJSON(w, map[string]any{"error": "store error"})
					return
				}
				p.writeJSON(w, map[string]any{"ok": true})
				return
			}
			if v, ok := req["admin"].(bool); ok {
				if err := p.store.SetAdmin(username, v); err != nil {
					p.writeJSON(w, map[string]any{"error": "store error"})
					return
				}
				p.writeJSON(w, map[string]any{"ok": true})
				return
			}
			p.writeJSON(w, map[string]any{"error": "unknown mutation"})
			return
		}
	}
	p.writeJSON(w, map[string]any{"error": "not found"})
}

// forbidden issues the standard 403 JSON body used across the REST API.
func (p *Proxy) forbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = fmt.Fprint(w, `{"error":"forbidden"}`)
}
