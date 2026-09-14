// polign_memory_demo is a terminal and web agent using Recall for typed memory
// and Polign for storage. Recall validates facts, applies corrections, and
// preserves the history of assertions and withdrawals across sessions.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Polign/polign_memory_demo/memkit"
	"github.com/Polign/recall"
	recallpolign "github.com/Polign/recall/polign"
)

func main() {
	polignURL := flag.String("polign", "http://127.0.0.1:24100", "polign_db server HTTP address")
	collection := flag.String("collection", "", "shared Recall collection (default recall_demo_lexical_v1, or recall_demo_model_v1 with -memory-embed model)")
	memoryEmbed := flag.String("memory-embed", "lexical", "memory retrieval: lexical (no download) or model (optional local semantic model)")
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
	var hosted hostedOptions
	flag.StringVar(&hosted.Addr, "hosted", "", "serve the multi-visitor hosted demo (the polign.com live demo) at this address instead of a terminal or single-user web UI (e.g. 127.0.0.1:23300)")
	flag.StringVar(&hosted.Nodes, "hosted-nodes", "http://127.0.0.1:23400,http://127.0.0.1:23402", "comma-separated polign_db nodes sharing one store; each visitor gets one agent per node")
	flag.StringVar(&hosted.Issuer, "hosted-issuer", "", "OpenID issuer of the sign-in ID tokens, e.g. https://cognito-idp.us-east-1.amazonaws.com/us-east-1_xxxx")
	flag.StringVar(&hosted.Audience, "hosted-audience", "", "audience (app client id) the ID tokens must carry")
	flag.StringVar(&hosted.Origins, "hosted-origins", "https://polign.com,https://www.polign.com", "comma-separated browser origins allowed to call the hosted API")
	flag.StringVar(&hosted.KeyFile, "hosted-key-file", "", "file caching the per-namespace polign_db API keys this process mints (owner-only permissions)")
	flag.StringVar(&hosted.KeyStore, "hosted-key-store", "", "object-store spec the nodes run on, used to mint namespaced keys with polign-apikey")
	flag.StringVar(&hosted.APIKeyBin, "hosted-apikey", "polign-apikey", "polign-apikey binary used to mint namespaced keys")
	flag.IntVar(&hosted.TurnsPerHour, "hosted-turns-per-hour", 30, "model turns one visitor may take per hour across both agents")
	flag.IntVar(&hosted.MaxConcurrent, "hosted-max-concurrent", 4, "model turns in flight at once across all visitors")
	flag.Parse()

	if *collection == "" {
		*collection = "recall_demo_lexical_v1"
		if *memoryEmbed == "model" {
			*collection = "recall_demo_model_v1"
		}
	}
	if err := run(*memoryEmbed, *polignURL, *collection, *wikipediaCollection, *wikipediaEmbed, *wikipediaEmbedDim, *wikipediaNProbe, *model, *provider, *dataDir, *dataURL, *predicatesPath, *scriptPath, *inspectAddr, *webAddr, *traceTools, hosted); err != nil {
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

// hostedOptions carries the -hosted-* flags.
type hostedOptions struct {
	Addr, Nodes, Issuer, Audience, Origins, KeyFile, KeyStore, APIKeyBin string
	TurnsPerHour, MaxConcurrent                                          int
}

func run(memoryEmbed, polignURL, collection, wikipediaCollection, wikipediaEmbed string, wikipediaEmbedDim, wikipediaNProbe int, model, provider, dataDir, dataURL, predicatesPath, scriptPath, inspectAddr, webAddr string, traceTools bool, hosted hostedOptions) error {
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

	var embedder recall.Embedder = recall.LexicalEmbedder{}
	switch memoryEmbed {
	case "lexical":
	case "model":
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
		model, err := LoadModel(dataDir)
		if err != nil {
			return err
		}
		embedder = recall.EmbedFunc(func(ctx context.Context, text string) ([]float32, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return model.Embed(text), ctx.Err()
		})
	default:
		return fmt.Errorf("unknown memory embedder %q (want lexical or model)", memoryEmbed)
	}

	db := memkit.NewPolignClientWithKey(polignURL, os.Getenv("POLIGN_API_KEY"))
	if !db.Healthy() {
		return fmt.Errorf("no polign_db server at %s (start one with: polign-server -store fs:./demo-bucket -http 127.0.0.1:24100)", polignURL)
	}
	if provider == "" {
		provider = inferProvider(model)
	}
	if hosted.Addr != "" {
		// In hosted mode -polign is only the knowledge node; memory lives on
		// the -hosted-nodes, one namespace per visitor.
		var wikipedia wikipediaSource
		if wikipediaCollection != "" {
			wikipedia = newWikipediaSearch(db, wikipediaCollection, wikipediaEmbed, wikipediaEmbedDim, wikipediaNProbe)
		}
		return runHosted(hosted, collection, registry, embedder, wikipedia, model, provider)
	}

	backend, err := recallpolign.New(recallpolign.Config{BaseURL: polignURL, APIKey: os.Getenv("POLIGN_API_KEY")})
	if err != nil {
		return err
	}
	store, err := memkit.NewStore(backend, collection, registry, embedder)
	if err != nil {
		return err
	}
	checkCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := store.Check(checkCtx); err != nil {
		return err
	}
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

	fmt.Printf("Recall memory demo: %s (%s) against %s (memory %q", model, provider, polignURL, collection)
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

// runHosted wires the hosted server from flags: Cognito token verification,
// per-namespace keys minted through polign-apikey, and a fresh agent per seat.
func runHosted(o hostedOptions, collection string, registry memkit.Registry, embedder recall.Embedder, wikipedia wikipediaSource, model, provider string) error {
	switch provider {
	case "anthropic":
		if os.Getenv("ANTHROPIC_API_KEY") == "" {
			return fmt.Errorf("ANTHROPIC_API_KEY is required in hosted mode")
		}
	case "openai":
		if os.Getenv("OPENAI_API_KEY") == "" {
			return fmt.Errorf("OPENAI_API_KEY is required in hosted mode")
		}
	default:
		return fmt.Errorf("unknown provider %q (want anthropic or openai)", provider)
	}
	if o.KeyStore == "" || o.KeyFile == "" {
		return fmt.Errorf("hosted mode needs -hosted-key-store and -hosted-key-file")
	}
	verifier, err := newTokenVerifier(o.Issuer, o.Audience, nil)
	if err != nil {
		return err
	}
	keys, err := loadKeyring(o.KeyFile, apikeyCommand{bin: o.APIKeyBin, store: o.KeyStore})
	if err != nil {
		return err
	}
	var nodes []string
	for _, node := range strings.Split(o.Nodes, ",") {
		if node = strings.TrimSpace(node); node != "" {
			if !memkit.NewPolignClient(node).Healthy() {
				return fmt.Errorf("no polign_db server at %s", node)
			}
			nodes = append(nodes, node)
		}
	}
	var origins []string
	for _, origin := range strings.Split(o.Origins, ",") {
		if origin = strings.TrimSpace(origin); origin != "" {
			origins = append(origins, origin)
		}
	}
	newAgent := func(store *memkit.Store, wikipedia wikipediaSource) Agent {
		if provider == "openai" {
			return newOpenAIAgent(model, store, wikipedia, false)
		}
		return newAnthropicAgent(model, store, wikipedia, false)
	}
	server, err := newHostedServer(hostedConfig{
		Origins: origins, Nodes: nodes, Collection: collection, Registry: registry, Embedder: embedder,
		Wikipedia: wikipedia, NewAgent: newAgent, Label: labelForProvider(provider), Model: model,
		Verify: verifier.Verify, Keys: keys, TurnsPerHour: o.TurnsPerHour, MaxConcurrent: o.MaxConcurrent,
	})
	if err != nil {
		return err
	}
	fmt.Printf("Recall memory demo, hosted: %s (%s), memory %q on %s", model, provider, collection, strings.Join(nodes, " and "))
	if wikipedia != nil {
		fmt.Printf(", Wikipedia %q", wikipedia.Collection())
	}
	fmt.Printf("\nhosted API: http://%s\n", o.Addr)
	return serveHosted(o.Addr, server)
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
