# toolkits

Small Go packages shared by a handful of exporters and probes: metrics plumbing,
caches, HTTP and WebSocket clients, and a few helpers. Each package stands alone;
import only what you need.

```sh
go get github.com/rosenlo/toolkits@latest
```

## Packages

### Metrics

| Package | What it is for |
|---|---|
| [`promutil`](promutil) | Prometheus plumbing: collector/vec constructors, turning collected metrics into a remote-write request (`BuildWriteRequest`), batched remote write with retry and backoff (`BatchRemoteWriteWithRetry`) and with resumable progress (`BatchRemoteWriteProgress`), plus a small query client (`/api/v1/query`, `query_range`, `export`, label values). |
| [`eventtime`](eventtime) | Counts observations from a partitioned log (e.g. Kafka) in windows of their **own event time** and remote-writes each window stamped with that time, instead of the export time. A per-partition watermark decides when a window is first written; later arrivals re-write it at the same timestamp, and a published watermark tells a reader how far the windows are complete. Counter, distinct-count and histogram families. See the package doc. |
| [`profiling`](profiling) | Memory limit and usage from cgroups (v1 and v2, with a root fallback), heap dumps, and a watchdog that dumps and uploads a heap profile past a usage threshold. |

### Caches and maps

| Package | What it is for |
|---|---|
| [`lra`](lra) | A byte-size-bounded least-recently-used cache with optional expiry and an eviction callback, and `ShardedCache`, which splits it so concurrent writers do not serialise on one lock. |
| [`safemap`](safemap) | A mutex-guarded map whose entries each carry their own expiration, with a periodic cleaner that drops expired ones. |
| [`ttlsafemap`](ttlsafemap) | A map with a TTL per item and a cleanup timer, and a sharded variant (`ShardMap`). |
| [`structure/stringmap`](structure/stringmap) | A set of strings (`Add`, `Delete`, `Exists`, `ToSlice`). Not safe for concurrent use. |

### HTTP and WebSocket

| Package | What it is for |
|---|---|
| [`http/httpclient`](http/httpclient) | A `net/http` client wrapper with default dial and request timeouts, basic auth, and a `Request`/`RequestWithContext` that returns the body. |
| [`http/httpserver`](http/httpserver) | An `http.Server` wrapper with graceful shutdown. |
| [`http/requests`](http/requests) | A chainable request builder over `gorequest`, with optional verbose response printing. |
| [`http/rest`](http/rest) | A chainable REST client over `gorequest` with a base URL, query params, retry and debug logging. |
| [`wsclient`](wsclient) | A reconnect-friendly WebSocket client that reports its message-channel depth as a metric. |
| [`websocketclient`](websocketclient) | **Deprecated** — use `wsclient`. |

### Process and runtime

| Package | What it is for |
|---|---|
| [`election`](election) | Leader election on a Kubernetes `Lease` (client-go `leaderelection`): start it, then ask `IsMaster` or wait on the leader callback. |
| [`cronjob`](cronjob) | Register and run functions on cron schedules. |
| [`signal`](signal) | Signal helpers: wait for SIGTERM or SIGINT while ignoring SIGHUP, a SIGHUP/SIGTERM channel, and sending yourself SIGHUP. |
| [`semaphore`](semaphore) | A counting semaphore to bound concurrency. |
| [`log`](log) | A thin zap-based logger with package-level `Info`/`Warn`/`Error`/`Debug` (and `f` variants) and `With` for fields. |
| [`gnu`](gnu) | CPU and memory facts read from `/proc` (`CpuInfo`, `MemInfo`). |

### Small helpers

| Package | What it is for |
|---|---|
| [`trie`](trie) | A prefix trie, e.g. for path allow-lists. |
| [`file`](file) | Read and write files and lines; walk a directory. |
| [`hash`](hash) | `MD5` of a string. |
| [`mathutil`](mathutil) | `Mean` and `StdDev`. |
| [`str`](str) | String concatenation. |
| [`common`](common) | Slice helpers (`Contains`, `DuplicateRemove`) and `ToJSON`. |
| [`structure/stack`](structure/stack), [`structure/tree`](structure/tree) | A bounded stack, and a binary tree with slice conversions. |

## Testing

```sh
go test -race ./...
```

`eventtime` checks its own sources for leftovers a private codebase leaves in
comments (dates, issue numbers, addresses). A second check reads a list of words
that must not appear from the file named by `TOOLKITS_PRIVATE_WORDS`, kept outside
the repository; without it that check is skipped.

## License

MIT — see [LICENSE](LICENSE).
