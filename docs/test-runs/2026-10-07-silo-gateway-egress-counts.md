# Silo gateway egress byte counts under load (2026-10-07 plan §6)

Task 2425, executing §6 of `docs/test-plans/2026-10-07.md` (the plan is on `candidate`, from task
2411).

## Environment

- Sandbox VM: 4 vCPU, 7 GiB RAM, Debian, `go version go1.26.6 linux/amd64`.
- Base: `main` at `1389b9483` (Merge pull request #3146). The fix is `1834d2733` on
  `grain/task-2425`.
- Tested commit: `11dbb21e1` ("Fix egress audit reporting bytes_out:0 …").
  `git branch -r --contains 11dbb21e1` lists `origin/main`, `origin/grain-merge-queue/main` and
  task branches cut from main, but **not** `origin/candidate`. So, as the plan says, this ran on
  `main`.
- Stubs: these are the gateway package's existing test harness (`newEnv`). It runs real silod on
  SQLite, the real gateway and the real egress proxy on loopback. The stubs are:
  - the fake GCE (`pkg/silo/gce/fake`), the one real cloud API involved;
  - `fakeResolver`, which resolves names to the documentation address `203.0.113.10`;
  - `upstream.dial`, which sends that address to local servers. This change adds `upstream.route`,
    so a port can be pointed at a server the test chooses.

  The upstreams themselves are a raw HTTP/1.1 server (`countingUpstream`) and an
  `httptest.NewTLSServer`. Nothing in grain or silod is mocked.

## Overlap check

- The `2026-10-06` plan §11 report (`docs/test-runs/*-silo-v1-part2.md`) has not landed on
  `main` or `candidate`. Only `2026-10-06-silo-v1-part1.md` exists, and it does not mention
  egress byte counts, `countedConn` or the WaitGroup.
- No other remote `grain/task-*` branch changes `pkg/silo/gateway/proxy.go`.
- This sandbox has no tool for listing queued tasks, so the open-task check covered branches
  only.

## What was written

All of it is in `pkg/silo/gateway/egress_counts_test.go`.

- **`TestEgressPlainHTTPByteCounts`**:
  - Sends plain-HTTP POSTs through the real gateway with 8 clients in parallel. Bodies are 0 B,
    1 KiB, 64 KiB and 8 MiB, and each size is sent N times.
  - Covers three upstream behaviours, chosen per request by its Host
    (`<behaviour>-<size>-<i>.example.com`):
    - `read`: reads the body and replies 200 with a body of the same size.
    - `reject`: replies 413 at once without reading the body, then drains the connection until
      the gateway hangs up.
    - `hangup`: sends 200 headers that promise 1000 bytes, then closes.
  - The upstream records the bytes it read from and wrote to each connection. For every request
    the test asserts:
    - exactly one `egress.connect` audit record;
    - `bytes_out` ≥ the bytes the upstream read, and ≥ the request line;
    - for `read`, `bytes_out` ≥ the body plus the request line;
    - `bytes_in` = the bytes the upstream wrote (for `read` and `reject`), and `bytes_in` ≥ the
      response body (for `read`);
    - for `hangup`, `0 ≤ bytes_in ≤` the bytes the upstream wrote. An upstream that closes with
      the body unread sends a reset, which can discard its own headers before the gateway reads
      them. `bytes_in: 0` was seen for 8 MiB requests, and it is correct.
  - N is 8 by default, so CI stays fast. `GRAIN_EGRESS_LOAD=200` gives the plan's 200.
- **`TestEgressConnectByteCounts`**:
  - Sends 8 MiB up and 8 MiB down through `CONNECT tls.example.com:443` to an
    `httptest.NewTLSServer`.
  - Asserts that the audit record is allowed and that `bytes_out` and `bytes_in` are each
    ≥ 8 MiB.
  - CONNECT goes through `tunnel`, not `countedConn`. It counts with `io.Copy`'s return values
    after both copy goroutines finish, so it has no race of this kind.
- **`TestCountedConnFinish`**: a deterministic unit test of the fix (see below).

## Commands and iteration counts

```sh
# Load, as the plan asks: 200 per size per behaviour, -race, -count=5
GRAIN_EGRESS_LOAD=200 go test -race -count=5 -v \
  -run 'TestEgressPlainHTTPByteCounts|TestEgressConnectByteCounts|TestCountedConnFinish' ./pkg/silo/gateway/

# Before the fix: the same test file in a worktree at origin/main (11dbb21e1's code)
git worktree add /tmp/oldmain origin/main
GRAIN_EGRESS_LOAD=200 go test -race -count=1 -run TestEgressPlainHTTPByteCounts ./pkg/silo/gateway/   # ×5
GRAIN_EGRESS_LOAD=200 go test       -count=1 -run TestEgressPlainHTTPByteCounts ./pkg/silo/gateway/   # ×13

# Step 5: 20 runs of the whole Silo tree with the fix
for i in $(seq 1 20); do go test -race -count=1 ./pkg/silo/...; done
```

| Run | Code | Result |
|---|---|---|
| Load, `-race`, 200/size, 5 single runs | `11dbb21e1` (main) | **3 of 5 FAIL**: `WARNING: DATA RACE`, in `reject` (3 runs) and `hangup` (1 run). Every byte-count assertion held. |
| Load, no `-race`, 200/size, 13 runs | `11dbb21e1` (main) | 13 of 13 pass. One run took 61.8 s instead of ~2 s (see the observations). |
| Load, `-race`, 200/size, `-count=5` | fix `1834d2733` | 5 of 5 pass: `read`, `reject`, `hangup`, CONNECT and `TestCountedConnFinish`, in 56 s total |
| Load, no `-race`, 200/size, 8 runs | fix | 8 of 8 pass, 1.5–2.0 s each |
| `go test -race ./pkg/silo/...`, 20 runs | fix | **20 of 20 pass**, 48–63 s each. No failures to record. |

Each load run has 2,400 plain-HTTP requests: 3 behaviours × 4 sizes × 200. Five runs make
12,000 requests, all audited exactly once, plus five 8 MiB-each-way CONNECTs.

## Findings

### F1: the 11dbb21e1 WaitGroup races, and can panic in the audit (fixed here)

This is what the plan predicted.

**Mechanism:**
- `countedConn.Write` calls `wg.Add(1)`, and `forward`'s deferred audit calls `wg.Wait()`.
- The Transport's writer goroutine (`persistConn.writeLoop`) keeps writing the body after the
  response arrives, whenever the upstream answers without reading it: a 413, or a hang-up after
  the headers.
- So a Write can begin, with an `Add` from zero, while `Wait` is blocked on the previous Write or
  is just returning from it.
- Go forbids that sequence:
  - `-race` reports it as a data race on the WaitGroup's semaphore. In the traces, the write
    comes from `forward`'s deferred function (`runtime.deferreturn` in `serveEgress`) and the
    read from `io.copyBuffer` under `persistConn.writeLoop`.
  - `sync` can panic with `WaitGroup is reused before previous Wait has returned`.
- The panic is raised inside `forward`'s deferred audit, before `auditEgress`. `net/http`
  recovers it, so the connection drops **and the request is never audited**.

**Under load:** with the race detector, 3 of 5 runs of `TestEgressPlainHTTPByteCounts` at
200/size failed. Every trace showed the same pair of goroutines. No byte-count assertion failed.
Before the deferred `Wait`, `ec.close("")` has already closed the upstream, so a Write that starts
late gets `use of closed network connection` and writes 0 bytes. The undercount the plan feared
did not show up as a wrong number. The defect is the invalid synchronization itself, and the
audit record it can lose.

**Deterministic reproduction:** 5 of 5 runs failed with a DATA RACE *and* the panic. Put this
test, together with the `gatedConn` helper from `egress_counts_test.go`, in `pkg/silo/gateway` at
`11dbb21e1`, then run `go test -race -run TestWaitGroupRepro ./pkg/silo/gateway/`:

```go
func TestWaitGroupRepro(t *testing.T) {
	g := &gatedConn{entered: make(chan struct{}, 2), release: make(chan struct{})}
	ec := &egressConn{}
	cc := &countedConn{Conn: g, ec: ec}
	go func() { // the Transport's writer goroutine: headers, then body
		cc.Write([]byte("hello"))
		cc.Write([]byte("world"))
	}()
	<-g.entered
	waited := make(chan int64)
	go func() { cc.wg.Wait(); waited <- ec.out.Load() }() // forward's deferred audit
	time.Sleep(100 * time.Millisecond)                    // Wait is blocked on the first Write
	close(g.release)
	audited := <-waited
	time.Sleep(100 * time.Millisecond)
	t.Logf("audited bytes_out %d; on the wire %d", audited, g.wrote.Load())
}
```

Output, all 5 runs: `WARNING: DATA RACE` … `panic: sync: WaitGroup is reused before previous Wait
has returned`. Without `-race` the same test passed 5 of 5, because the panic depends on timing
there.

**Fix (`1834d2733`):**
- `countedConn` drops the WaitGroup. Each `Write` holds an `RWMutex` for reading. The audit calls
  the new `finish`, which takes the lock for writing and sets `done`.
- `finish` waits for any Write in flight. That Write can return, because the upstream was closed
  just before.
- Every Write that starts after `finish` returns `net.ErrClosed` without writing.
- So `ec.out` is final when the audit reads it, whether or not a Write had begun when `finish`
  ran. This is the synchronization the plan asked for: it is valid when no Write is in flight.

**Regression tests:**
- `TestCountedConnFinish` pins both halves deterministically: `finish` blocks on a gated Write
  until that Write is released and counted, and a Write after `finish` writes and counts nothing.
- The load test covers the gateway end to end.

### Observations (no finding filed)

- **A one-off 60 s stall before the fix.** One of the 13 non-race load runs at `11dbb21e1` took
  61.8 s instead of ~2 s. That is the length of the test client's or upstream's one-minute
  timeout, so one request hung, but every assertion passed and nothing was logged. It did not
  recur in the other 12 runs at `11dbb21e1`, nor in any run with the fix: 8 non-race load runs,
  5 `-race` load runs and the 20 runs of `./pkg/silo/...`. With no reproduction, I have not
  attributed it.
- **`ReverseProxy read error during body copy` log lines.** The gateway logs these for every
  `hangup` request and for some `reject` requests. They are `httputil`'s own logging of an
  upstream that cut the response short, which is expected here.
- **Not covered by this section, and left for the plan's "audit as a whole" gap:**
  - 100 MB uploads;
  - chunked request bodies;
  - a client aborting mid-body;
  - an upstream reset mid-response.

## CI verdict

Grain CI run `b54decf3be4d5786` on `1834d2733`, full suite (no `jobs` filter): **success**.

- frontend, go (15m01s), installer, lint, package cache, postgres, relay image, terraform and
  ui e2e: all green.
- None of the known flakes recurred.

The report commit's own verdict is in the task's closing note.
