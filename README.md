# Recall memory + Wikipedia demo

An agent that remembers preferences and project facts, accepts corrections, and
can show what it knew before. It uses [Recall](https://github.com/Polign/recall)
for typed memory and [Polign](https://polign.com) for storage. Both the Anthropic
and OpenAI conversation loops use the same Recall client.

Try telling it **“I use Vim”**, then **“I switched to Neovim.”** Start a new
session and ask which editor you use. Ask for the history to see both statements.

The terminal prints each tool call and its result. A browser interface includes
a memory inspector, so you can see current beliefs, earlier assertions, and
withdrawals as you talk.

## Run locally

You'll need Go 1.25+, Polign server v0.6.4+, and an Anthropic or OpenAI API key.
[Install Polign](https://github.com/Polign/polign#install), then:

```sh
export ANTHROPIC_API_KEY=...
./run-demo.sh fs:./recall-bucket -wikipedia-collection ""
```

This starts a local database and the terminal agent. Memory uses Recall's
built-in lexical embedder; there is no model download for memory retrieval.
To use an OpenAI model, set `OPENAI_API_KEY` and pass its model ID:

```sh
./run-demo.sh fs:./recall-bucket -wikipedia-collection "" -model gpt-5
```

Use `/reset` to clear the conversation and check that the agent still reads
saved memories. `/quit` exits. Run the same command again to reopen the same
database. Writes and recovery follow Polign's storage guarantees.

For browser chat and the inspector:

```sh
./run-demo.sh fs:./recall-bucket -wikipedia-collection "" -web 127.0.0.1:8080
```

Open <http://127.0.0.1:8080>. The inspector is at `/memories/`. Use `-trace=false`
to keep memory tool inputs and results out of application logs.

## What Recall handles

The demo imports **Recall v0.4.0** directly. It provides:

- Typed facts: strings, numbers, and booleans are validated against the registry.
- Corrections: a newer assertion replaces a single-valued preference; lists of
  technologies or interests can hold several values.
- History: assertions and withdrawals are append-only events. Older records
  are never rewritten to maintain a status flag.
- Current and past answers: exact queries, numeric filters, and `as_of` reads
  use Recall's memory rules.
- Cached reads: Recall's materialized view uses the backend's log watermark
  when available and falls back to uncached reads when it cannot verify it.

The [demo registry](predicates.json) includes Recall's starter predicates and
examples such as `daily_step_goal` (number), `uses_dark_mode` (boolean), and
`likes` (multiple strings). Supply `-predicates path/to/registry.json` for your
own definitions.

`memkit` is now an adapter for the demo's tool results and inspector. It calls
Recall's Go client and Polign HTTP backend; it no longer implements its own
correction or deletion rules. Inspector labels are derived from Recall's audit
replay, rather than stored in the database.

## Tools you can watch

| Tool | What it does |
| --- | --- |
| `remember_fact` / `remember_preference` | Save a typed assertion and report any current belief it replaced. |
| `recall` | Read current beliefs, search, filter numeric values, or answer as of an earlier time. |
| `memory_history` | Read the assertions and withdrawals for a subject and predicate. |
| `list_predicates` | Show the memory types the registry accepts. |
| `forget` | Withdraw a typed value, or all current values with `all: true`. |
| `search_wikipedia` | Query the separate knowledge index when enabled. |

An explicit confidence of `0` is preserved; omitting it defaults to `1`.
Forgetting accepts typed values, including `false` and `0`. It keeps the event
history and is **not permanent deletion**. `recall(include_history: true)` also
shows the event log; combine that option only with subject/predicate filters,
`as_of`, and `limit`.

## Existing demo memories

The default collection is now **`recall_demo_lexical_v1`**. The previous
`memories` collection used mutable memkit records and a different embedder.
Those records are left untouched; this version does not migrate them.

If you explicitly select a legacy collection, the demo rejects its old records
instead of treating superseded or deleted values as current facts. Use the new
default or choose a fresh collection with `-collection`.

Other agents can use the same memory by connecting to the same Polign endpoint,
collection, and namespace with Recall v0.4.0, the same registry, and the same
embedding method. Set `POLIGN_API_KEY` for a server requiring authentication.

The old GIFs and recordings in this repository show the previous memkit
implementation. The current inspector labels rows `active`, `historical`, or
`withdrawal`; future events are shown as `pending`.

## Add Wikipedia search

The default `run-demo.sh` store is `s3://polign-demo-wiki-en-uw1/polign-v4` in
`us-west-1`, containing
the `wikipedia_bge` passage index:

```sh
./run-demo.sh
```

This requires AWS credentials with read access to the index and write access
for the Recall collection. You can pass a different store URI explicitly.
Wikipedia search has no write operation. The agent cites returned article URLs,
and the UI adds a retrieval label only after the search tool succeeds.

Wikipedia uses lexical search by default. For semantic retrieval, run the
`polign_demo/serve/embedserve.py` sidecar with the index's
`BAAI/bge-small-en-v1.5` model and pass `-wikipedia-embed http://127.0.0.1:23200`.
This does not change the memory embedder or collection.

For optional semantic **memory** retrieval, use `-memory-embed model`. That
loads the demo's existing small local model, downloading it on first use. Its
default collection is `recall_demo_model_v1`. Keep lexical and model embeddings
in separate collections; changing the embedder does not migrate stored vectors.

## Deployment and tests

The [dstack deployment](dstack/) runs the agent, an authenticated browser UI,
and Polign v0.6.5 inside a confidential VM. This change updates the deployment
configuration; it does not redeploy an existing VM.

```sh
go test -race ./...
go vet ./...
python3 tests/restart.py /absolute/path/to/polign-server
```

The restart test uses a temporary local database. It writes and corrects facts,
kills the server, restarts it from the same store, and verifies typed reads and
new corrections after recovery. No model API or cloud bucket is used.

The [local dstack smoke test](dstack/smoke-test.sh) checks browser authentication,
routing, health, and the inspector.

## Options

| Flag | Purpose |
| --- | --- |
| `-polign` | Database URL; default `http://127.0.0.1:24100`. |
| `-collection` | Shared memory collection; defaults depend on the memory embedder. |
| `-memory-embed` | `lexical` (default, no download) or `model` (optional semantic model). |
| `-model` / `-provider` | Conversation model and optional explicit `anthropic` or `openai` provider. |
| `-predicates` | Custom registry JSON file. |
| `-wikipedia-collection` | Read-only knowledge collection; empty disables it. |
| `-wikipedia-embed` | Optional BGE embedding sidecar for Wikipedia. |
| `-web` / `-inspect` | Browser chat or a standalone memory inspector address. |
| `-script` | Replay user lines from a file. |
| `-trace` | Print tool inputs/results; default `true`. |
| `-data-dir` / `-data-url` | Model cache and artifact source, used only with `-memory-embed model`. |

Run `go run . -help` for all options.

## License

[Apache 2.0](LICENSE).
