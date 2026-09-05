package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"untis-proxy/internal/session"
	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

func newTestProxy(t *testing.T) (*Proxy, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SetAdmin("bob", true); err != nil {
		t.Fatalf("set admin: %v", err)
	}
	_ = st.SetDefaultSchool("schuldorf")
	p := New(st, untis.New(untis.Config{Server: "school.example.com", School: "schuldorf"}), session.NewManager(5*time.Minute), Options{School: "schuldorf"})
	return p, st
}

func adminRequest(t *testing.T, p *Proxy, user string, method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	s := p.sessions.New(user, 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: s.ID})
	rec := httptest.NewRecorder()
	p.handleAdmin(rec, req)
	return rec
}

func TestAdminStatus(t *testing.T) {
	p, st := newTestProxy(t)
	_, _ = st.AddWebhook(&store.Webhook{School: "schuldorf", URL: "https://e/h", Enabled: true})
	_, _ = st.AddNtfyTopic(&store.NtfyTopic{School: "schuldorf", Topic: "schoollife", Enabled: true})
	rec := adminRequest(t, p, "bob", http.MethodGet, "/admin/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if out["admins"].(float64) != 1 || out["webhooks"].(float64) != 1 || out["ntfyTopics"].(float64) != 1 {
		t.Errorf("unexpected status summary: %v", out)
	}
}

func TestAdminForbiddenForNonAdmin(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "mallory", Method: "key"}); err != nil {
		t.Fatalf("add user: %v", err)
	}
	rec := adminRequest(t, p, "mallory", http.MethodGet, "/admin/status", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status code = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestAdminUnauthorizedWithoutSession(t *testing.T) {
	p, _ := newTestProxy(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/status", nil)
	rec := httptest.NewRecorder()
	p.handleAdmin(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status code = %d, want 401", rec.Code)
	}
}

func TestAdminUsersCRUD(t *testing.T) {
	p, st := newTestProxy(t)
	rec := adminRequest(t, p, "bob", http.MethodPost, "/admin/users", `{"username":"alice","admin":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create user code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	rec = adminRequest(t, p, "bob", http.MethodGet, "/admin/users", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list users code = %d", rec.Code)
	}
	var out struct {
		Users []map[string]any `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	found := false
	for _, u := range out.Users {
		if u["username"] == "alice" {
			found = true
		}
	}
	if !found {
		t.Fatalf("alice missing from listing: %s", rec.Body.String())
	}
	// set admin
	rec = adminRequest(t, p, "bob", http.MethodPost, "/admin/users/alice", `{"admin":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set admin code = %d (body %s)", rec.Code, rec.Body.String())
	}
	if ok, _ := st.IsAdmin("alice"); !ok {
		t.Fatalf("alice should be admin after promotion")
	}
	// delete alice
	rec = adminRequest(t, p, "bob", http.MethodDelete, "/admin/users/alice", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete code = %d", rec.Code)
	}
	if u, _ := st.GetUser("alice"); u != nil {
		t.Fatalf("alice should be deleted")
	}
}

func TestAdminSchools(t *testing.T) {
	p, _ := newTestProxy(t)
	rec := adminRequest(t, p, "bob", http.MethodPost, "/admin/schools/neuendorf", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("register school code = %d", rec.Code)
	}
	rec = adminRequest(t, p, "bob", http.MethodGet, "/admin/schools", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list schools code = %d", rec.Code)
	}
	var out struct {
		Schools []map[string]any `json:"schools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(out.Schools) != 1 || out.Schools[0]["name"] != "neuendorf" {
		t.Fatalf("expected neuendorf, got %s", rec.Body.String())
	}
	if _, ok := out.Schools[0]["addedAt"].(float64); !ok {
		t.Fatalf("addedAt should be epoch seconds: %v", out.Schools[0])
	}
}

func TestAdminWebhooksAndNtfy(t *testing.T) {
	p, _ := newTestProxy(t)
	rec := adminRequest(t, p, "bob", http.MethodPost, "/admin/webhooks", `{"school":"schuldorf","classId":4419,"url":"https://e/h","secret":"s"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add webhook code = %d (body %s)", rec.Code, rec.Body.String())
	}
	rec = adminRequest(t, p, "bob", http.MethodPost, "/admin/ntfy", `{"school":"schuldorf","classId":0,"topic":"schoollife"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add ntfy code = %d (body %s)", rec.Code, rec.Body.String())
	}
	rec = adminRequest(t, p, "bob", http.MethodGet, "/admin/webhooks", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list webhooks code = %d", rec.Code)
	}
	var wh struct {
		Webhooks []map[string]any `json:"webhooks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wh); err != nil {
		t.Fatalf("bad webhook json: %v", err)
	}
	if len(wh.Webhooks) != 1 || wh.Webhooks[0]["classId"].(float64) != 4419 {
		t.Fatalf("unexpected webhooks: %s", rec.Body.String())
	}
	rec = adminRequest(t, p, "bob", http.MethodGet, "/admin/ntfy", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list ntfy code = %d", rec.Code)
	}
	var nf struct {
		Topics []map[string]any `json:"topics"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &nf); err != nil {
		t.Fatalf("bad ntfy json: %v", err)
	}
	if len(nf.Topics) != 1 {
		t.Fatalf("unexpected ntfy topics: %s", rec.Body.String())
	}
	// delete both
	rec = adminRequest(t, p, "bob", http.MethodDelete, "/admin/webhooks/1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete webhook code = %d", rec.Code)
	}
	rec = adminRequest(t, p, "bob", http.MethodDelete, "/admin/ntfy/1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete ntfy code = %d", rec.Code)
	}
}

func TestAdminDashboardServesHTML(t *testing.T) {
	p, _ := newTestProxy(t)
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	s := p.sessions.New("bob", 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: s.ID})
	rec := httptest.NewRecorder()
	p.handleAdminDashboard(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard code = %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q, want text/html", ct)
	}
	if len(rec.Body.String()) < 1000 {
		t.Fatalf("dashboard HTML suspiciously small: %d bytes", len(rec.Body.String()))
	}
}
