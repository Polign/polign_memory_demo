package memkit

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTokenTransportDeleteManyAndBarrier(t *testing.T) {
	var barrierToken, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/vectors:delete"):
			w.Header().Set("X-Polign-Write-Token", "wal-42")
			_, _ = w.Write([]byte(`{"ids":["a","b"],"truncated":false}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/vectors/__barrier__"):
			barrierToken = r.Header.Get("X-Polign-Require-Write-Token")
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		default:
			http.Error(w, `{"error":"unexpected"}`, http.StatusTeapot)
		}
	}))
	defer srv.Close()

	var tokens WriteTokens
	client := NewPolignClientWithTransport(srv.URL, "plgn_test", TokenTransport(&tokens, nil))
	n, err := client.DeleteMany("memories", []string{"a", "b", "c"})
	if err != nil || n != 2 {
		t.Fatalf("DeleteMany = %d, %v", n, err)
	}
	if tokens.Latest() != "wal-42" || auth != "Bearer plgn_test" {
		t.Fatalf("token = %q auth = %q", tokens.Latest(), auth)
	}
	if err := client.Barrier("memories", tokens.Latest()); err != nil {
		t.Fatal(err)
	}
	if barrierToken != "wal-42" {
		t.Fatalf("barrier sent %q", barrierToken)
	}
	if n, err := client.DeleteMany("memories", nil); n != 0 || err != nil {
		t.Fatalf("empty DeleteMany = %d, %v", n, err)
	}
	barrierToken = "unset"
	if err := client.Barrier("memories", ""); err != nil || barrierToken != "unset" {
		t.Fatalf("empty barrier made a request: %q %v", barrierToken, err)
	}
}
