// polign_memory_demo is a terminal agent with a typed, durable memory store
// backed by polign_db. Memories are typed records with database semantics
// (registry-enforced predicates, cardinality-driven supersession, filtered
// and semantic recall), and the source of truth is the server's bucket: kill
// the agent, kill the server, wipe local state, and the memories survive.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Polign/polign_memory_demo/memkit"
)

func main() {
	polignURL := flag.String("polign", "http://127.0.0.1:24100", "polign_db server HTTP address")
	collection := flag.String("collection", "memories", "collection the memories live in (one per agent identity)")
	wikipediaCollection := flag.String("wikipedia-collection", "wikipedia_bge", "read-only Wikipedia collection (empty disables Wikipedia answers)")
	wikipediaEmbed := flag.String("wikipedia-embed", "", "optional BGE query-embedding sidecar address; enables semantic search (empty uses lexical search)")
	wikipediaEmbedDim := flag.Int("wikipedia-embed-dim", 384, "vector width returned by the Wikipedia BGE sidecar")
	wikipediaNProbe := flag.Int("wikipedia-nprobe", 8, "IVF cells probed by semantic Wikipedia queries")
	model := flag.String("model", "claude-opus-5", "model id; a claude-* id uses the Anthropic API, a gpt-*/o* id uses the OpenAI API (see also -provider)")
	provider := flag.String("provider", "", "force \"anthropic\" or \"openai\" instead of inferring from -model")
	dataDir := flag.String("data-dir", "", "embedding model directory (default: user cache dir)")
	dataURL := flag.String("data-url", DefaultDataURL, "embedding model artifact (URL or local tarball)")
	predicatesPath := flag.String("predicates", "", "predicate registry JSON (default: the embedded registry)")
	scriptPath := flag.String("script", "", "replay user lines from this file instead of reading stdin, then exit")
	inspectAddr := flag.String("inspect", "", "serve a read-only inspector page at this address (e.g. 127.0.0.1:24102)")
	webAddr := flag.String("web", "", "serve the chat UI and memory inspector at this address instead of using the terminal (e.g. :8080)")
	traceTools := flag.Bool("trace", true, "print tool inputs and results; disable when deployment logs are public")
	flag.Parse()

	if err := run(*polignURL, *collection, *wikipediaCollection, *wikipediaEmbed, *wikipediaEmbedDim, *wikipediaNProbe, *model, *provider, *dataDir, *dataURL, *predicatesPath, *scriptPath, *inspectAddr, *webAddr, *traceTools); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// inferProvider picks the API a model id belongs to.
func inferProvider(model string) string {
	for _, prefix := range []string{"gpt-", "o1", "o3", "o4"} {
		if strings.HasPrefix(model, prefix) {
			return "openai"
		}
	}
	return "anthropic"
}

func run(polignURL, collection, wikipediaCollection, wikipediaEmbed string, wikipediaEmbedDim, wikipediaNProbe int, model, provider, dataDir, dataURL, predicatesPath, scriptPath, inspectAddr, webAddr string, traceTools bool) error {
	logf := func(format string, args ...any) { fmt.Printf(dim+format+reset+"\n", args...) }
	collection = strings.TrimSpace(collection)
	wikipediaCollection = strings.TrimSpace(wikipediaCollection)
	if collection == "" {
		return fmt.Errorf("memory collection must not be empty")
	}
	if wikipediaCollection != "" && wikipediaCollection == collection {
		return fmt.Errorf("memory collection and Wikipedia collection must be different (both are %q)", collection)
	}

	raw := defaultPredicates
	if predicatesPath != "" {
		var err error
		if raw, err = os.ReadFile(predicatesPath); err != nil {
			return err
		}
	}
	registry, err := memkit.LoadRegistry(raw)
	if err != nil {
		return err
	}

	if dataDir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return err
		}
		dataDir = filepath.Join(cache, "polign-memory-demo")
	}
	if err := EnsureModel(dataDir, dataURL, logf); err != nil {
		return err
	}
	embedder, err := LoadModel(dataDir)
	if err != nil {
		return err
	}

	db := memkit.NewPolignClient(polignURL)
	if !db.Healthy() {
		return fmt.Errorf("no polign_db server at %s (start one with: polign-server -store fs:./demo-bucket -http 127.0.0.1:24100)", polignURL)
	}

	store := memkit.NewStore(db, collection, registry, embedder.Embed)
	var wikipedia wikipediaSource
	if wikipediaCollection != "" {
		wikipedia = newWikipediaSearch(db, wikipediaCollection, wikipediaEmbed, wikipediaEmbedDim, wikipediaNProbe)
	}

	if inspectAddr != "" {
		if err := startInspector(inspectAddr, store, collection); err != nil {
			return err
		}
		logf("inspector: http://%s", inspectAddr)
	}

	if provider == "" {
		provider = inferProvider(model)
	}
	if webAddr != "" {
		var credential string
		switch provider {
		case "anthropic":
			credential = os.Getenv("ANTHROPIC_API_KEY")
		case "openai":
			credential = os.Getenv("OPENAI_API_KEY")
		}
		if credential == "" {
			return fmt.Errorf("%s model credential is required in web mode", provider)
		}
	}
	var agent Agent
	switch provider {
	case "anthropic":
		if os.Getenv("ANTHROPIC_API_KEY") == "" {
			logf("ANTHROPIC_API_KEY is not set; using whatever credentials the Anthropic SDK resolves (e.g. ant auth login)")
		}
		agent = newAnthropicAgent(model, store, wikipedia, traceTools)
	case "openai":
		if os.Getenv("OPENAI_API_KEY") == "" {
			logf("OPENAI_API_KEY is not set; the OpenAI SDK will fail without it")
		}
		agent = newOpenAIAgent(model, store, wikipedia, traceTools)
	default:
		return fmt.Errorf("unknown provider %q (want anthropic or openai)", provider)
	}

	fmt.Printf("polign memory demo: %s (%s) against %s (memory %q", model, provider, polignURL, collection)
	if wikipediaCollection != "" {
		mode := "lexical"
		if wikipediaEmbed != "" {
			mode = "semantic BGE"
		}
		fmt.Printf(", Wikipedia %q, %s", wikipediaCollection, mode)
	}
	fmt.Println(")")
	if webAddr != "" {
		fmt.Printf("web UI: http://%s\n", webAddr)
		return serveWeb(webAddr, webHandler(agent, labelForProvider(provider), store, collection, db.Healthy))
	}
	fmt.Printf(dim + "tool calls print as they happen. /quit exits, /reset clears the conversation (not the store).\n" + reset)

	label := labelForProvider(provider)

	var scanErr error
	next := stdinSource(&scanErr)
	if scriptPath != "" {
		lines, err := loadScript(scriptPath)
		if err != nil {
			return err
		}
		next = scriptSource(lines, typeDelay, turnPause)
	}
	if err := repl(agent, label, next); err != nil {
		return err
	}
	return scanErr
}

func labelForProvider(provider string) string {
	if provider == "openai" {
		return "gpt"
	}
	return "claude"
}

// repl drives the conversation until next reports no more lines or the user
// quits. next prints the "you> " prompt and yields one user line, already
// echoed to the terminal.
func repl(agent Agent, label string, next func() (string, bool)) error {
	for {
		line, ok := next()
		if !ok {
			return nil
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case line == "/quit" || line == "/exit":
			return nil
		case line == "/reset":
			agent.Reset()
			fmt.Println(dim + "conversation cleared; the memory store is untouched" + reset)
			continue
		}
		reply, err := agent.Turn(context.Background(), line)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			continue
		}
		fmt.Printf("\n%s> %s\n", label, reply.Text)
		for _, source := range reply.RetrievedFrom {
			fmt.Printf("%sRetrieved from %s%s\n", dim, source, reset)
		}
	}
}

// stdinSource reads user lines interactively. A scanner error lands in
// scanErr so run can surface it after the loop ends.
func stdinSource(scanErr *error) func() (string, bool) {
	sc := bufio.NewScanner(os.Stdin)
	return func() (string, bool) {
		fmt.Print("\nyou> ")
		if !sc.Scan() {
			fmt.Println()
			*scanErr = sc.Err()
			return "", false
		}
		return sc.Text(), true
	}
}
