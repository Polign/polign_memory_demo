package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

type webStubAgent struct {
	turns  []string
	resets int
}

func (a *webStubAgent) Turn(_ context.Context, text string) (AgentReply, error) {
	a.turns = append(a.turns, text)
	return AgentReply{Text: "remembered: " + text, RetrievedFrom: []string{"wikipedia_bge"}}, nil
}

func (a *webStubAgent) Reset() { a.resets++ }

func TestWebChatAndReset(t *testing.T) {
	agent := &webStubAgent{}
	handler := webHandler(agent, "test-agent", inspectorFake(t), "memories", func() bool { return true })

	chat := httptest.NewRecorder()
	handler.ServeHTTP(chat, httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"message":"I use Vim"}`)))
	if chat.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", chat.Code, chat.Body.String())
	}
	var response struct {
		Label         string   `json:"label"`
		Reply         string   `json:"reply"`
		RetrievedFrom []string `json:"retrieved_from"`
	}
	if err := json.Unmarshal(chat.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Reply != "remembered: I use Vim" || response.Label != "test-agent" || !reflect.DeepEqual(response.RetrievedFrom, []string{"wikipedia_bge"}) {
		t.Fatalf("chat response = %#v", response)
	}

	reset := httptest.NewRecorder()
	handler.ServeHTTP(reset, httptest.NewRequest(http.MethodPost, "/api/reset", nil))
	if reset.Code != http.StatusNoContent || agent.resets != 1 {
		t.Fatalf("reset status = %d, resets = %d", reset.Code, agent.resets)
	}
}

func TestWebHealthTracksPolign(t *testing.T) {
	healthy := false
	handler := webHandler(&webStubAgent{}, "test", inspectorFake(t), "memories", func() bool { return healthy })

	down := httptest.NewRecorder()
	handler.ServeHTTP(down, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if down.Code != http.StatusServiceUnavailable {
		t.Fatalf("down status = %d, want 503", down.Code)
	}

	healthy = true
	up := httptest.NewRecorder()
	handler.ServeHTTP(up, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if up.Code != http.StatusOK || up.Body.String() != "ok\n" {
		t.Fatalf("up = %d %q", up.Code, up.Body.String())
	}
}

func TestWebRejectsBadChatInput(t *testing.T) {
	handler := webHandler(&webStubAgent{}, "test", inspectorFake(t), "memories", nil)
	for _, body := range []string{`{}`, `{"message":""}`, `{"message":"ok","extra":true}`, `{`, `{"message":"ok"}{}`} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, rec.Code)
		}
	}
}

func TestWebRejectsCrossOriginMutation(t *testing.T) {
	handler := webHandler(&webStubAgent{}, "test", inspectorFake(t), "memories", nil)
	for _, path := range []string{"/api/chat", "/api/reset"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"message":"hello"}`))
		req.Header.Set("Origin", "https://attacker.example")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s status = %d, want 403", path, rec.Code)
		}
	}
}

func TestWebServesUIAndInspector(t *testing.T) {
	handler := webHandler(&webStubAgent{}, "test", inspectorFake(t), "memories", nil)
	for path, want := range map[string]string{"/": "Polign memory demo", "/app.js": "Retrieved from", "/memories/": "3 records"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET %s = %d, body missing %q", path, rec.Code, want)
		}
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("GET %s missing CSP", path)
		}
	}
}
