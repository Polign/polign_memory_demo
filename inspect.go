// The inspector is a read-only web page showing the memory store as one
// table. It reads through Store.Recall, so it shows exactly what the agent
// can see: active records plus superseded history, never tombstones. It is a
// window, not a management surface; it serves a single GET and nothing else.
package main

import (
	"fmt"
	"html/template"
	"net"
	"net/http"
	"sort"

	"github.com/Polign/polign_memory_demo/memkit"
)

const inspectorHTML = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta http-equiv="refresh" content="2">
<title>memories</title>
<style>
  body { background: #101214; color: #e6e8ea; font: 14px/1.6 "SF Mono", Menlo, Consolas, monospace; margin: 2rem auto; max-width: 60rem; padding: 0 1rem; }
  header { color: #8a9095; margin-bottom: 1rem; }
  header b { color: #e6e8ea; font-weight: 600; }
  table { border-collapse: collapse; width: 100%; }
  th { color: #8a9095; text-align: left; font-weight: 600; }
  th, td { border-bottom: 1px solid #23272b; padding: 0.45rem 1rem 0.45rem 0; }
  td.num { font-variant-numeric: tabular-nums; }
  tr.superseded td { text-decoration: line-through; opacity: 0.55; }
  tr.superseded td.status { text-decoration: none; }
  tr:target td { background: #16211d; }
  a { color: #35d0a0; text-decoration: none; }
  .empty { color: #8a9095; margin-top: 2rem; }
</style>
</head>
<body>
<header>memories &middot; <b>{{.Collection}}</b> &middot; {{len .Rows}} records</header>
{{if .Rows}}
<table>
<tr><th>kind</th><th>subject</th><th>predicate</th><th>value</th><th>confidence</th><th>status</th></tr>
{{range .Rows}}
<tr id="{{.ID}}"{{if .Superseded}} class="superseded"{{end}}>
<td>{{.Kind}}</td>
<td>{{.Subject}}</td>
<td>{{.Predicate}}</td>
<td>{{.Value}}</td>
<td class="num">{{.Confidence}}</td>
<td class="status">{{if .Superseded}}<a href="#{{.SupersededBy}}">superseded</a>{{else}}{{.Status}}{{end}}</td>
</tr>
{{end}}
</table>
{{else}}
<p class="empty">no memories yet</p>
{{end}}
</body>
</html>
`

var inspectorTmpl = template.Must(template.New("inspector").Parse(inspectorHTML))

type inspectorRow struct {
	ID, Kind, Subject, Predicate string
	Value                        any
	Confidence                   string
	Status, SupersededBy         string
	Superseded                   bool
}

type inspectorPage struct {
	Collection string
	Rows       []inspectorRow
}

// inspectorHandler renders the collection as one read-only table.
func inspectorHandler(store *memkit.Store, collection string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		records, err := store.Recall(memkit.RecallQuery{IncludeHistory: true, Limit: 1000})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		// A superseded row sorts directly above its replacement.
		sort.Slice(records, func(i, j int) bool {
			a, b := records[i], records[j]
			if a.Subject != b.Subject {
				return a.Subject < b.Subject
			}
			if a.Predicate != b.Predicate {
				return a.Predicate < b.Predicate
			}
			return a.ObservedAt < b.ObservedAt
		})
		page := inspectorPage{Collection: collection}
		for _, rec := range records {
			page.Rows = append(page.Rows, inspectorRow{
				ID: rec.ID, Kind: rec.Kind, Subject: rec.Subject, Predicate: rec.Predicate,
				Value: rec.Value, Confidence: fmt.Sprintf("%.2f", rec.Confidence),
				Status: rec.Status, SupersededBy: rec.SupersededBy,
				Superseded: rec.Status == "superseded",
			})
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := inspectorTmpl.Execute(w, page); err != nil {
			fmt.Println("inspector:", err)
		}
	})
}

// startInspector listens on addr (failing fast on a busy port) and serves the
// inspector in the background for the life of the process.
func startInspector(addr string, store *memkit.Store, collection string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() {
		_ = http.Serve(ln, inspectorHandler(store, collection))
	}()
	return nil
}
