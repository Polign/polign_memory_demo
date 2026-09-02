package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Polign/polign_memory_demo/memkit"
)

const webHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Polign memory</title>
<style>
  :root { color-scheme: dark; font: 16px/1.5 Inter, system-ui, sans-serif; background: #0d1117; color: #e6edf3; }
  body { margin: 0 auto; max-width: 52rem; padding: 2rem 1rem; }
  header { display: flex; align-items: baseline; justify-content: space-between; gap: 1rem; }
  h1 { font-size: 1.3rem; margin: 0 0 1.5rem; }
  a { color: #3fb950; }
  #chat { min-height: 22rem; border: 1px solid #30363d; border-radius: .6rem; padding: 1rem; overflow-wrap: anywhere; }
  .message { margin: 0 0 1rem; white-space: pre-wrap; }
  .message b { color: #8b949e; display: block; font-size: .8rem; text-transform: uppercase; }
  .provenance { display: block; width: fit-content; margin-top: .45rem; padding: .12rem .5rem; border: 1px solid #2f6f44; border-radius: 999px; color: #7ee787; background: #12261a; font-size: .75rem; }
  form { display: flex; gap: .5rem; margin-top: 1rem; }
  input { flex: 1; min-width: 0; background: #161b22; color: inherit; border: 1px solid #30363d; border-radius: .4rem; padding: .75rem; }
  button { background: #238636; color: white; border: 0; border-radius: .4rem; padding: .75rem 1rem; cursor: pointer; }
  button.secondary { background: #30363d; }
  button:disabled { opacity: .6; cursor: wait; }
  #status { color: #8b949e; min-height: 1.5rem; margin-top: .5rem; }
</style>
</head>
<body>
<header><h1>Polign memory demo</h1><a href="/memories/">inspect memories</a></header>
<main id="chat" aria-live="polite"></main>
<form id="form">
  <input id="message" maxlength="8000" autocomplete="off" placeholder="Tell the agent something worth remembering" required autofocus>
  <button id="send" type="submit">Send</button>
  <button id="reset" class="secondary" type="button">Reset chat</button>
</form>
<div id="status"></div>
<script src="/app.js"></script>
</body>
</html>`

const webJS = `(() => {
  const chat = document.querySelector('#chat');
  const form = document.querySelector('#form');
  const input = document.querySelector('#message');
  const send = document.querySelector('#send');
  const reset = document.querySelector('#reset');
  const status = document.querySelector('#status');

  function add(role, text, retrievedFrom = []) {
    const row = document.createElement('div');
    row.className = 'message';
    const label = document.createElement('b');
    label.textContent = role;
    row.append(label, document.createTextNode(text));
    for (const source of retrievedFrom) {
      const provenance = document.createElement('span');
      provenance.className = 'provenance';
      provenance.textContent = 'Retrieved from ' + source;
      row.append(provenance);
    }
    chat.append(row);
    row.scrollIntoView({ behavior: 'smooth', block: 'end' });
  }

  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    const message = input.value.trim();
    if (!message) return;
    add('you', message);
    input.value = '';
    send.disabled = true;
    status.textContent = 'Thinking…';
    try {
      const response = await fetch('/api/chat', {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ message })
      });
      const body = await response.json();
      if (!response.ok) throw new Error(body.error || 'request failed');
      add(body.label, body.reply, body.retrieved_from || []);
      status.textContent = '';
    } catch (error) {
      status.textContent = error.message;
    } finally {
      send.disabled = false;
      input.focus();
    }
  });

  reset.addEventListener('click', async () => {
    const response = await fetch('/api/reset', { method: 'POST', credentials: 'same-origin' });
    if (response.ok) {
      chat.replaceChildren();
      status.textContent = 'Conversation cleared; durable memories remain.';
    } else {
      status.textContent = 'Could not reset the conversation.';
    }
  });
})();`

type webApp struct {
	agent   Agent
	label   string
	healthy func() bool
	mu      sync.Mutex
}

func webHandler(agent Agent, label string, store *memkit.Store, collection string, healthy func() bool) http.Handler {
	app := &webApp{agent: agent, label: label, healthy: healthy}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", app.health)
	mux.HandleFunc("GET /app.js", app.javascript)
	mux.HandleFunc("POST /api/chat", app.chat)
	mux.HandleFunc("POST /api/reset", app.reset)
	mux.HandleFunc("GET /memories", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/memories/", http.StatusMovedPermanently)
	})
	mux.Handle("GET /memories/", http.StripPrefix("/memories", inspectorHandler(store, collection)))
	mux.HandleFunc("GET /", app.index)
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; connect-src 'self'; img-src 'none'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (app *webApp) health(w http.ResponseWriter, _ *http.Request) {
	if app.healthy != nil && !app.healthy() {
		http.Error(w, "polign unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

func (app *webApp) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(webHTML))
}

func (app *webApp) javascript(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write([]byte(webJS))
}

func (app *webApp) chat(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeWebJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request rejected"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req struct {
		Message string `json:"message"`
	}
	if err := dec.Decode(&req); err != nil {
		writeWebJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeWebJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" || len(req.Message) > 8000 {
		writeWebJSON(w, http.StatusBadRequest, map[string]string{"error": "message must contain 1 to 8000 bytes"})
		return
	}

	app.mu.Lock()
	reply, err := app.agent.Turn(r.Context(), req.Message)
	app.mu.Unlock()
	if err != nil {
		log.Printf("web chat: provider request failed: %v", err)
		writeWebJSON(w, http.StatusBadGateway, map[string]string{"error": "model request failed"})
		return
	}
	writeWebJSON(w, http.StatusOK, struct {
		Label         string   `json:"label"`
		Reply         string   `json:"reply"`
		RetrievedFrom []string `json:"retrieved_from"`
	}{Label: app.label, Reply: reply.Text, RetrievedFrom: reply.RetrievedFrom})
}

func (app *webApp) reset(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeWebJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request rejected"})
		return
	}
	app.mu.Lock()
	app.agent.Reset()
	app.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// sameOrigin blocks browser CSRF against ambient HTTP Basic credentials. CLI
// clients do not send Origin and remain usable; browsers do, and their host
// must match the request host (the public TLS scheme is terminated upstream).
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

func writeWebJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func serveWeb(addr string, handler http.Handler) error {
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    32 << 10,
	}
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("web server: %w", err)
}
