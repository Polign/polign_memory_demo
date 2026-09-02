# polign memory + Wikipedia demo

A terminal agent with two deliberately separate collections in one durable
database:

- `memories` is writable typed agent memory. Facts and preferences stated in
  conversation are stored here and survive agent and server restarts.
- `wikipedia_bge` is the read-only English Wikipedia passage index. General
  questions are answered from its returned passages and include source URLs.

The default server store is
`s3://polign-demo-wiki-en/polign-v4`, so the demo can query the existing
Wikipedia index while keeping memory records in their own collection.

![The four-act demo: durable writes, agent restart, server kill -9 and cold restart from the bucket, and a supersession](demo.gif)

Most "AI memory" stuffs text into a vector store and hopes similarity search
finds it again. This demo treats agent memory as what it actually is: state.
Every memory is a typed record:

```
kind:        preference
subject:     user
predicate:   prefers_editor
value:       neovim
confidence:  1.0
source:      user_stated
status:      active
```

The store enforces the schema. Predicates come from a closed registry
([predicates.json](predicates.json)) that declares each one's cardinality and
value type, and those declarations decide what a write means:

- `prefers_editor` is single-valued: a new value supersedes the old one. The
  old record is not overwritten; it flips to `status: superseded` with a link
  to its replacement, so the history is queryable.
- `likes` is multi-valued: a new value is an additional fact, and repeating
  an existing one is idempotent.
- `daily_step_goal` is a number and `uses_dark_mode` is a boolean: values are
  validated against the declared type (the string "8000" is rejected with an
  error naming the expected type, and the model corrects itself) and stored
  as that type, so numbers compare numerically in recall filters.

Supersession is a consequence of the schema, never a model judgment. The
model's job is conversation; the database's job is semantics. The schema
itself is enforced by the demo's store layer; polign_db contributes the typed
values, the filters, and the durability underneath it.

Everything is backed by [polign_db](https://polign.com), so the source of
truth is an object-store bucket. Writes are durable before they are
acknowledged, reads see your writes immediately, and the server can be killed
and restarted from the bucket with nothing lost.

## Run it

You need Go, an Anthropic or OpenAI API key, and `polign-server`
(`curl -fsSL https://get.polign.com | sh`).

```sh
export ANTHROPIC_API_KEY=...
./run-demo.sh
```

Claude models are the default. To use an OpenAI model instead, export
`OPENAI_API_KEY` and pass the model id, e.g.
`./run-demo.sh -model gpt-5`; the provider is inferred from
the id. Same store, same tools, same typed
semantics, because the schema enforcement lives in the database layer, not in
the prompt or the provider.

That starts a server against `s3://polign-demo-wiki-en/polign-v4` and opens
the agent. AWS credentials need read access to the Wikipedia index and write
access for the `memories` collection. Pass another store explicitly when
needed: `./run-demo.sh s3://my-bucket/prefix`.

By default Wikipedia questions use the index's lexical search, which needs no
second model process. For higher-quality BGE semantic retrieval, run the
`polign_demo/serve/embedserve.py` sidecar with the same
`BAAI/bge-small-en-v1.5` model used to build `wikipedia_bge`, then pass its
address:

```sh
./run-demo.sh -wikipedia-embed http://127.0.0.1:23200
```

The memory collection continues to use the demo's small local embedder; Polign
collections can have different dimensions and the two retrieval paths never
mix records. To run the original memory-only local demo:

```sh
./run-demo.sh fs:./demo-bucket -wikipedia-collection ""
```

The first run downloads a small embedding model (one-time, ~43 MB). Every
tool call the agent makes is printed as it happens, so you can watch
the whole path: model, typed tool, validation, database.

## The script

**Act 1: read-your-writes.**

```
you> I use Vim as my editor.
  → remember_preference({"subject":"user","predicate":"prefers_editor","value":"vim"})
  ← {"stored":{"id":"m-...","value":"vim","status":"active",...}}

you> My daily step goal is 9000.
  → remember_fact({"subject":"user","predicate":"daily_step_goal","value":9000})
  ← {"stored":{"id":"m-...","value":9000,"status":"active",...}}

you> What editor do I use?
  → recall({"subject":"user","predicate":"prefers_editor"})
  ← {"count":1,"records":[{"value":"vim","status":"active",...}]}
claude> Vim.
```

Note the step goal: `9000` is stored as a JSON number, because the registry
declares `daily_step_goal` a number. Typed in, typed out.

No "eventually consistent" caveat: the write was durable in the bucket before
the tool call returned.

**Act 2: kill the agent.** Ctrl-C the demo, run it again, ask again. It still
knows. Memory is not context.

**Act 3: kill the server.** Stop everything, then restart the server from
nothing but the bucket:

```sh
kill %1                                  # or ctrl-c the whole demo
./run-demo.sh                            # same bucket, cold start
```

Ask again. Still Vim. The agent process, the server process, and the server's
memory are all disposable; the bucket is the database.

The typed value survived too, and it is still a number:

```
you> Is my step goal above 8000?
  → recall({"subject":"user","predicate":"daily_step_goal"})
  ← {"count":1,"records":[{"value":9000,...}]}
claude> Yes, your step goal is 9000, which is above 8000.
```

The value came back as a JSON number after a cold restart, because the
registry declared it one. When the model wants the database to do the
comparison instead, recall takes value_min and value_max as numeric range
filters over number-typed values (see the recall primitives below); the
integration tests pin that path.

**Act 4: the contradiction (the point of the demo).**

```
you> I switched to Neovim.
  → remember_preference({"subject":"user","predicate":"prefers_editor","value":"neovim"})
  ← {"stored":{"value":"neovim","status":"active",...},
     "superseded":[{"value":"vim","status":"superseded","superseded_by":"m-...",...}]}
claude> Noted, you've switched from Vim to Neovim.

you> What editors have I used over time?
  → recall({"subject":"user","predicate":"prefers_editor","include_history":true})
  ← {"count":2,"records":[...vim superseded..., ...neovim active...]}
```

The database knew `prefers_editor` is single-valued, so the write became a
supersession, with the old value kept as linked history. No prompt told the
model to detect a contradiction; the schema did it.

## Inspect the store

Run with `-inspect 127.0.0.1:24102` (for example
`./run-demo.sh fs:./demo-bucket -inspect 127.0.0.1:24102`) and open that
address in a browser. You get one read-only table of every record the agent
can see, refreshing as you talk; superseded rows are struck through and link
to the record that replaced them.

![The inspector after the four acts: the superseded Vim record struck through, Neovim active](inspector.png)

## Run it in a dstack TEE

The [dstack deployment](dstack/) runs the agent, Polign, persistent storage,
and an authenticated browser UI inside one confidential VM. It includes a
local Compose override, a deployment configuration, checksum-pinned Polign
installation, and an end-to-end routing/authentication smoke test.

The application also has a web mode outside dstack:

```sh
go run . -web 127.0.0.1:8080 -inspect ""
```

Open <http://127.0.0.1:8080>; the memory inspector is available at
`/memories/`. Use `-trace=false` when logs must not contain memory tool inputs
and results.

## Record the script

`record-demo.sh` replays all four acts hands-free against a fresh bucket,
including the kill -9 and the cold restart, so the whole thing can be
captured with `asciinema rec demo.cast -c ./record-demo.sh`. Run
`./run-demo.sh` once first so the embedding model is already cached. You can
also replay any single act yourself:
`./run-demo.sh fs:./demo-bucket -script demo/act1.txt`.

## Recall and knowledge search are separate primitives

- **Exact:** `recall(subject, predicate, kind, min_confidence, value_min,
  value_max)` is a filtered query over typed metadata. `confidence` and
  number-typed values are stored as numbers, so `min_confidence: 0.8` and
  `value_min: 8000` compare numerically, not stringly.
- **Semantic:** `recall(query: "my dev setup")` embeds the query and searches
  the same records, still filtered to `status: active`.

Semantic retrieval and durable typed state are different primitives. Here
they run over one store, in one bucket, with one consistency contract.

General-knowledge questions use a third tool, `search_wikipedia(query, limit)`.
It always targets `wikipedia_bge` through Polign's cold object-store query path
and returns `title`, `url`, and `text` for grounding. It has no write operation;
the remember and forget tools remain bound to `memories` only.

## Use the pattern in your own agent

The memory layer is an importable package,
[memkit](memkit/): the typed client, the predicate registry, and the store
with its supersession semantics. Bring your own predicates JSON and your own
embedder:

```go
import "github.com/Polign/polign_memory_demo/memkit"

registry, _ := memkit.LoadRegistry(myPredicatesJSON)
db := memkit.NewPolignClient("http://127.0.0.1:24100")
store := memkit.NewStore(db, "memories", registry, myEmbedder)

store.Remember("preference", "user", "prefers_editor", "neovim", 1, "user_stated")
records, _ := store.Recall(memkit.RecallQuery{Subject: "user", Predicate: "prefers_editor"})
```

Writes are validated against the registry, supersession follows from
cardinality, and recall is the same two primitives the demo uses.

## Tests

```sh
go test ./...                    # unit tests against an in-process fake server
```

The durability claims are pinned by integration tests against a real server:

```sh
polign-server -store fs:/tmp/mem-bucket -http 127.0.0.1:24100 -grpc 127.0.0.1:24101 &
POLIGN_MEMORY_DEMO_URL=http://127.0.0.1:24100 go test -run TestIntegrationWrite -v
kill -9 %1   # yes, -9
polign-server -store fs:/tmp/mem-bucket -http 127.0.0.1:24100 -grpc 127.0.0.1:24101 &
POLIGN_MEMORY_DEMO_URL=http://127.0.0.1:24100 go test -run 'TestIntegrationRecallAfterRestart|TestIntegrationWriteAfterRestart' -v
```

The last phase writes a fresh supersession against the cold-started server and
asserts it is immediately visible to exact recall — the same order as act 4.

## Flags

```
-polign      polign_db HTTP address        (default http://127.0.0.1:24100)
-collection  collection for the memories   (default "memories")
-wikipedia-collection  read-only knowledge collection (default "wikipedia_bge"; empty disables)
-wikipedia-embed       optional BGE sidecar; enables semantic search
-wikipedia-embed-dim   BGE vector width (default 384)
-wikipedia-nprobe      IVF cells for semantic Wikipedia search (default 8)
-model       model id                      (default claude-opus-5; gpt-*/o* ids use OpenAI)
-provider    force "anthropic" or "openai" instead of inferring from -model
-predicates  registry JSON to use instead of the embedded one
-data-dir    embedding model cache dir     (default: user cache dir)
-script      replay user lines from a file instead of reading stdin, then exit
-inspect     serve the read-only inspector at this address (e.g. 127.0.0.1:24102)
-web        serve browser chat + inspector instead of the terminal (e.g. :8080)
-trace      print memory tool inputs/results (default true; disable for public logs)
```

## License

[Apache 2.0](LICENSE).
