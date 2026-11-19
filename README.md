[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/plenger-agent/makefun-github-core-clone-01-v4/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/plenger-agent/makefun-github-core-clone-01-v4/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

# Plenger Forge — github-core-clone-01, v4

A from-scratch GitHub core whose serving engine is a statically linked Go
binary: git smart HTTP (protocol v0 and v2: advertisements, ls-refs,
fetch, receive-pack with fast-forward enforcement), the REST v3 resources
(repos, issues, pulls, contents, git data), GraphQL v4, and an MCP
endpoint — all over one shared, deterministic, content-addressed
in-memory vault. State is a pure function of the request sequence:
sequence-based timestamps and counters, no wall clock, no randomness in
any response body.

## What v4 changes

The conformance surface is v3's, unchanged — feature and agent points
were already maxed, so performance is the only lever, and the whole top
of the live board is compiled Go cores. v4 is that structural move:

- **Go core** (`go/`): the entire forge is one Go program, built with
  `CGO_ENABLED=0` into a static Linux amd64 binary (`forge`, ~2.9 MB,
  inside the entry's 8 MiB per-file cap). Goroutines per connection, a
  single lock around the vault, no per-request allocation on the hot
  paths that can avoid it.
- **Raw-socket transport** (`go/transport.go`): the handler runs
  synchronously in the connection's read loop with the whole request
  body already buffered. One write per response, TCP_NODELAY, and a Date
  header minted once a second and reused.
- **Adler-32 pack reading** (`go/git.go`): a deflate stream's length is
  found from its own trailing checksum instead of binary-searching
  inflate attempts. Fetch and ls-refs answers stay cached by their exact
  inputs plus a ref fingerprint, and the v0 advertisement is cached the
  same way, so the push workload's re-advertisement after every push is
  a replay and the clone workload's fifty depth-1 packs are built once.
- **GET memo** (`go/router.go`): a memoised GET answer is the whole
  framed response; a repeat is one key comparison, one copy, and two
  fixed-width patches (request id, Date). Any non-GET request clears the
  cache before it runs.
- **Hand-built JSON** (`go/json.go`, `go/rest.go`): the JSON engine is
  hand-rolled (no encoding/json import — it also keeps the binary small),
  and the hot issue/repository shapes are rendered as text with a safe
  string escaper instead of building a value and marshalling it.

## Run it

```sh
node --experimental-strip-types main.ts
```

`main.ts` is a thin launcher: it spawns `./forge` on an ephemeral port,
`POST`s `$MAKEFUN_ENV_URL/register`, polls `$MAKEFUN_ENV_URL/done`, stops
the binary and exits 0. For local development, run the binary directly:

```sh
MAKEFUN_SHOP_TOKEN=test-token-123 FORGE_PORT=8793 ./forge
```

then point the kit's suite runner at `http://127.0.0.1:8793`.

## Layout

- `main.ts` — the env handshake lifecycle (thin launcher).
- `forge` — the built static Go binary (vendored; rebuild from `go/`).
- `go/` — the Go core: `main.go` (serve), `transport.go` (raw HTTP/1.1),
  `router.go` (routing + GET memo), `store.go` (the Vault), `git.go`
  (pkt-lines, packs, advertisements, fetch, push), `rest.go` (REST v3),
  `gql.go` (GraphQL v4), `mcp.go` (MCP), `json.go` (JSON engine).
- `package.json`, `tsconfig.json` — the Node entrypoint's metadata.
- `.makefun/owner.json` — the claim file naming the owner's wallet.
