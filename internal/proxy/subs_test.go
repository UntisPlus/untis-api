package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"untis-proxy/internal/store"
)

func subsRequest(t *testing.T, p *Proxy, user string, method, path, body string) *httptest.ResponseRecorder {
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

	if strings.HasPrefix(path, "/api/webhooks") {
		p.handleSubsWebhooks(rec, req)
	} else {
		p.handleSubsNtfy(rec, req)
	}
	return rec
}

func TestSubsOwnClassWebhookOnly(t *testing.T) {
	p, st := newTestProxy(t)
	// carla is a student of class 4419; no privileged flags.
	if err := st.UpsertUser(&store.User{Username: "carla", Method: "key", ClassID: 4419}); err != nil {
		t.Fatalf("add carla: %v", err)
	}
	// own class: allowed
	rec := subsRequest(t, p, "carla", http.MethodPost, "/api/webhooks", `{"classId":4419,"url":"https://e/h"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("own-class webhook code = %d (body %s)", rec.Code, rec.Body.String())
	}
	// someone else's class: forbidden
	rec = subsRequest(t, p, "carla", http.MethodPost, "/api/webhooks", `{"classId":9999,"url":"https://e/h2"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign-class webhook code = %d, want 403", rec.Code)
	}
	// school-wide: forbidden for plain student
	rec = subsRequest(t, p, "carla", http.MethodPost, "/api/webhooks", `{"classId":0,"url":"https://e/h3"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("school-wide webhook code = %d, want 403", rec.Code)
	}
	// listing: carla sees only her class hook
	rec = subsRequest(t, p, "carla", http.MethodGet, "/api/webhooks", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list code = %d", rec.Code)
	}
	var out struct {
		Webhooks []map[string]any `json:"webhooks"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Webhooks) != 1 || out.Webhooks[0]["classId"].(float64) != 4419 {
		t.Fatalf("unexpected webhooks visible to carla: %s", rec.Body.String())
	}
}

func TestSubsEditorCanSubscribeAnyPooledClass(t *testing.T) {
	p, st := newTestProxy(t)
	_ = st.UpsertUser(&store.User{Username: "caroline", Method: "key", ClassID: 4419})
	if err := st.SetPerm("caroline", store.FeatureEditor, true); err != nil {
		t.Fatalf("grant editor: %v", err)
	}
	// editor may create a school-wide topic
	rec := subsRequest(t, p, "caroline", http.MethodPost, "/api/ntfy", `{"classId":0,"topic":"schoollife"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("editor school-wide ntfy code = %d (body %s)", rec.Code, rec.Body.String())
	}
	// editor may target any pooled class
	rec = subsRequest(t, p, "caroline", http.MethodPost, "/api/ntfy", `{"classId":4419,"topic":"class4419"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("editor class ntfy code = %d (body %s)", rec.Code, rec.Body.String())
	}
	// unpooled class still forbidden even for editor
	rec = subsRequest(t, p, "caroline", http.MethodPost, "/api/ntfy", `{"classId":7777,"topic":"x"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unpooled class ntfy code = %d, want 403", rec.Code)
	}
}

func TestSubsDeleteOwnership(t *testing.T) {
	p, st := newTestProxy(t)
	_ = st.UpsertUser(&store.User{Username: "dave", Method: "key", ClassID: 4419})
	_ = st.UpsertUser(&store.User{Username: "eve", Method: "key", ClassID: 4419})
	id, err := st.AddWebhook(&store.Webhook{School: "schuldorf", ClassID: 4419, URL: "https://e/h", Enabled: true, CreatedBy: "dave"})
	if err != nil {
		t.Fatalf("seed webhook: %v", err)
	}
	// eve cannot delete dave's hook
	req := httptest.NewRequest(http.MethodDelete, "/api/webhooks/"+strconv.FormatInt(id, 10), nil)
	s := p.sessions.New("eve", 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: s.ID})
	rec := httptest.NewRecorder()
	p.handleSubsWebhooks(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("eve delete dave's hook code = %d, want 403", rec.Code)
	}
	// dave can delete his own
	req = httptest.NewRequest(http.MethodDelete, "/api/webhooks/"+strconv.FormatInt(id, 10), nil)
	s = p.sessions.New("dave", 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: s.ID})
	rec = httptest.NewRecorder()
	p.handleSubsWebhooks(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dave delete own hook code = %d (body %s)", rec.Code, rec.Body.String())
	}
}
