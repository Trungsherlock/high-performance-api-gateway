# V2.4: optional shared response cache

V2.4 adds Redis response storage while preserving independent local flight groups
and backend limits. A completed fill on gateway A can satisfy B. Two overlapping
cold misses can still fetch twice: leases belong to V2.5.

```text
Eligible request -> optional L1 -> local flight -> optional Redis -> backend limiter
                                              <- publish safe completed response
```

## Modes and failure behavior

| `-cache-mode` | Local LRU | Redis response tier | Local coalescing |
|---|---|---|---|
| `local` (default) | yes | no | yes by default |
| `shared` | no | yes | yes by default |
| `layered` | yes | yes | yes by default |

`CACHE_ENABLED=false` disables all cache tiers and coalescing, preserving the
proxy baseline. Redis is optional; local mode constructs no Redis client.
Fresh local hits perform no dependency operation. The local leader checks Redis;
local followers share that operation. Unshareable backend responses retain the
existing individual streaming fallback under the limiter.

Redis read/write failures fall back to backend work through the local flight and
limiter. Writes are best effort: a Redis outage does not invalidate a completed
public response or a fresh L1 entry. Each operation has a 50 ms total deadline by
default, including pool acquisition, dialing, initialization and socket I/O;
read and publication have separate budgets. A miss can therefore spend up to two
such budgets in dependency operations, inside the request/shared-work deadline.
Default pool size and hard active-connection cap are both 16; retries are disabled.
Use `-redis-timeout` and `-redis-pool` to change these bounds.

Admin readiness means local initialization, independently of backend/Redis health.
Protection-first degradation, leases, and distributed follower admission remain
future work. Inbound requests/local followers are not globally bounded by this pool.

## Keys, values, and freshness

- Namespace: `gateway:response:v1:<sha256>`. Hash input is the existing effective
  upstream URL, including query, plus quoted Accept, Accept-Encoding and User-Agent
  representation inputs. Different upstream identities remain separate.
- Version-1 JSON stores an allowlisted header map, base64 body, and original absolute
  expiry in Unix milliseconds. Body limit is 64 KiB; encoded value limit is 96 KiB,
  including metadata/headers. Header preparation is bounded to 8 KiB and 128 values.
- Shared headers: Cache-Control, Content-Type, Content-Language, ETag,
  Last-Modified, Date, Expires and Age. Arbitrary extension headers are omitted from
  shared replay; this is a restricted application cache, not a transparent general
  HTTP cache. Invalid controls, unexpected header names, unsafe policy, malformed
  JSON, unknown schemas, excessive sizes, persistent keys and expired data miss.
- Original public-response eligibility remains mandatory before either tier:
  authorization, cookies and unsupported request headers bypass both; private,
  variant, encoded, cookie-setting, incomplete or oversized responses are not stored.
- A short atomic Lua read checks STRLEN before GET and returns the value with PTTL.
  Oversized values are never transferred to the client. Wrong Redis types and
  command/network failures are dependency errors, separately from genuine misses.
- Publication uses SET PXAT with the original bounded expiry. Pool/network delays
  cannot restart the fill TTL. Read expiry is the minimum of original expiry,
  lookup-start plus Redis PTTL, the ten-second cap and response-policy lifetime.
  Using lookup start subtracts the entire lookup duration conservatively.
  L1 promotion copies that expiry and never republishes the value.
- Absolute timestamps assume aligned clocks between the gateways and Redis; this
  milestone runs them on one Windows/WSL2 host. Clock skew across machines requires
  explicit treatment before deployment. Redis eviction can shorten retention.

Keys being hashed does not protect payload confidentiality. This local demo has
no Redis authentication/TLS configuration and binds the server to loopback.

## Reproduce

Pinned compatibility pair: Redis **7.4.2**, go-redis **v9.7.3** (go.mod/go.sum).
Start an isolated Redis, for example on a machine with a working Docker daemon:

```powershell
docker run --rm --name gateway-v24-redis -p 127.0.0.1:6379:6379 redis:7.4.2 redis-server --save "" --appendonly no --maxmemory 64mb --maxmemory-policy allkeys-lru
```

The collected run instead built the official Redis 7.4.2 source in WSL2 and used
port 16379. Its archive checksum and settings are in the raw artifact's redis.json.
`allkeys-lru` bounds Redis dataset retention; maxmemory is not a total RSS limit.
Persistence is disabled for disposable cache data. There are no coordination keys
yet; revisit eviction before adding leases. The harness never flushes a database.

```powershell
go build -o bin/gateway.exe ./cmd/gateway
go build -o bin/mockbackend.exe ./cmd/mockbackend
python bench/run-v24.py --redis-addr 127.0.0.1:6379
$env:REDIS_TEST_ADDR = '127.0.0.1:6379'
go test ./... -count=1 -timeout 60s
```

For manual use, add `-cache-mode layered -redis-addr 127.0.0.1:6379` to the gateway
command. Choose `shared` to measure the dependency round trip on every eligible
request. Linux uses extensionless binaries under bin/ with the same harness.
Without REDIS_TEST_ADDR the real-server tests explicitly skip; ordinary tests
still cover decoding, unavailable/slow dependencies, pool bounds and fallback.

## Collected evidence

[Final nine trials](../../bench/results/v2/20261103-214917-v24/trials.jsonl),
[summary](../../bench/results/v2/20261103-214917-v24/summary.json), and
[manifest](../../bench/results/v2/20261103-214917-v24/manifest.json).
Three fresh-process trials per mode used two independent gateways, a separate
50 ms mock backend, and a Python HTTP client on the same host. Each trial filled A,
requested the same key through B, then sent 100 sequential alternating warm calls.
Every trial reconciled 102 successful requests, backend attempts, tier counters,
zero dependency errors, and drained work gauges. Aggregate upstream budget stayed
at 2 x 16 permits and 2 x 16 queue slots.

| Mode | Backend fills per trial | Shared hits per trial | Warm L1 hits per trial | Median of trial warm medians |
|---|---:|---:|---:|---:|
| local | 2 | 0 | 100 | 0.130 ms |
| shared | 1 | 101 | 0 | 0.389 ms |
| layered | 1 | 1 | 100 | 0.194 ms |

This is a short path/counter comparison with persistent HTTP connections, not an
open-loop capacity benchmark. It includes Python and Windows/WSL scheduling and
fixed mode order; no CPU/RSS attribution or production speedup is claimed.
Earlier artifacts remain: 214713-v24 has an empty trials file after the first
client sent Connection: close and correctly bypassed caching (102 backend calls);
214750-v24 passed before final header hardening and is superseded by the final run.

Real Redis tests also verify A-to-B reuse, request and response safety, repeated
promotion without renewed freshness, corrupt/oversized/unsupported/persistent
values, wrong-type dependency errors, and overlapping cold misses producing two
backend fills. Non-Redis tests exercise a silent TCP dependency, bounded pool,
cancellation and available-local fallback with errors distinct from misses.
The full ordinary/race suites, vet and build passed with REDIS_TEST_ADDR set.
Logs: [ordinary](../../bench/results/v2/v24-tests.txt),
[race](../../bench/results/v2/v24-race.txt). Windows race tests use the documented
MSYS2 compiler PATH. No earlier benchmark results or original V1 plan were changed.

## Decisions and references

Shared storage enables cross-instance reuse but adds a network dependency and
serialization cost. L1 avoids that cost for warm data at the price of independent
copies; shared-only mode exposes the cost directly. Fail-open behavior prioritizes
availability but can increase duplicate backend work during cache failure.
Publication is unconditional in this milestone: no fencing or ordering between
concurrent writers is claimed. Redis is a single shared failure authority.

Official references: [go-redis options at the pinned version](https://github.com/redis/go-redis/blob/v9.7.3/options.go),
[PTTL](https://redis.io/docs/latest/commands/pttl/),
[SET/PXAT](https://redis.io/docs/latest/commands/set/),
[Lua atomicity](https://redis.io/docs/latest/develop/programmability/eval-intro/), and
[Redis eviction](https://redis.io/docs/latest/reference/eviction/).
