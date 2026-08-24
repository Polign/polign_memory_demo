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
)

func main() {
	polignURL := flag.String("polign", "http://127.0.0.1:24100", "polign_db server HTTP address")
	collection := flag.String("collection", "memories", "collection the memories live in (one per agent identity)")
	model := flag.String("model", "claude-opus-5", "model id; a claude-* id uses the Anthropic API, a gpt-*/o* id uses the OpenAI API (see also -provider)")
	provider := flag.String("provider", "", "force \"anthropic\" or \"openai\" instead of inferring from -model")
	dataDir := flag.String("data-dir", "", "embedding model directory (default: user cache dir)")
	dataURL := flag.String("data-url", DefaultDataURL, "embedding model artifact (URL or local tarball)")
	predicatesPath := flag.String("predicates", "", "predicate registry JSON (default: the embedded registry)")
	flag.Parse()

	if err := run(*polignURL, *collection, *model, *provider, *dataDir, *dataURL, *predicatesPath); err != nil {
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

func run(polignURL, collection, model, provider, dataDir, dataURL, predicatesPath string) error {
	logf := func(format string, args ...any) { fmt.Printf(dim+format+reset+"\n", args...) }

	raw := defaultPredicates
	if predicatesPath != "" {
		var err error
		if raw, err = os.ReadFile(predicatesPath); err != nil {
			return err
		}
	}
	registry, err := LoadRegistry(raw)
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

	db := NewPolignClient(polignURL)
	if !db.Healthy() {
		return fmt.Errorf("no polign_db server at %s (start one with: polign-server -store fs:./demo-bucket -http 127.0.0.1:24100)", polignURL)
	}

	store := NewStore(db, collection, registry, embedder.Embed)

	if provider == "" {
		provider = inferProvider(model)
	}
	var agent Agent
	switch provider {
	case "anthropic":
		if os.Getenv("ANTHROPIC_API_KEY") == "" {
			logf("ANTHROPIC_API_KEY is not set; using whatever credentials the Anthropic SDK resolves (e.g. ant auth login)")
		}
		agent = newAnthropicAgent(model, store)
	case "openai":
		if os.Getenv("OPENAI_API_KEY") == "" {
			logf("OPENAI_API_KEY is not set; the OpenAI SDK will fail without it")
		}
		agent = newOpenAIAgent(model, store)
	default:
		return fmt.Errorf("unknown provider %q (want anthropic or openai)", provider)
	}

	fmt.Printf("polign memory demo: %s (%s) against %s (collection %q)\n", model, provider, polignURL, collection)
	fmt.Printf(dim + "tool calls print as they happen. /quit exits, /reset clears the conversation (not the store).\n" + reset)

	sc := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\nyou> ")
		if !sc.Scan() {
			fmt.Println()
			return sc.Err()
		}
		line := strings.TrimSpace(sc.Text())
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
		fmt.Printf("\nclaude> %s\n", reply)
	}
}
