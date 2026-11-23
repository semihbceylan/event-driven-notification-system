# Event-Driven Notification System

A notification service that accepts SMS, email and push requests over an HTTP API,
queues them, and delivers them through an external provider, with rate limiting,
priorities, retries and status tracking.

> Work in progress. The sections below are filled in as features land.

## Quick start

```bash
docker compose up --build
curl localhost:8080/readyz
```

| Service      | Port | Purpose                                           |
|--------------|------|---------------------------------------------------|
| api          | 8080 | HTTP API; runs DB migrations on startup           |
| worker       | –    | Claims queued notifications and delivers them     |
| mockprovider | 8090 | Offline stand-in for webhook.site, with optional failure injection |
| postgres     | 5432 | Source of truth and work queue                    |
| redis        | 6379 | Shared rate limiter and pub/sub                   |

To send to a real webhook.site endpoint instead of the mock, copy `.env.example` to `.env`
and set `PROVIDER_URL`.

## Architecture

_TODO_

## Design decisions

### PostgreSQL as the queue

The `notifications` table is both the record of every notification and the work queue.
Workers claim rows with `SELECT … FOR UPDATE SKIP LOCKED`, so many workers can pull
from it at once without taking the same row. This gives several features with little code:

- **Priority:** `ORDER BY priority`
- **Scheduled sends and retry backoff:** a row is only picked up when `next_attempt_at <= now()`
- **Cancellation:** `UPDATE … WHERE status = 'pending'`; a row being sent cannot be cancelled
- **No dual-write problem:** saving a notification and enqueueing it are the same insert

The required throughput is well within what this handles: 100 msg/s × 3 channels is at most 300 rows/s.
At much higher volumes I would move the queue to Kafka or RabbitMQ behind the same interface.

### Delivery and retries

Each channel has its own pool of sender goroutines (`WORKERS_PER_CHANNEL`, default 10), so a slow
email provider cannot hold up SMS. A sender loops: **claim → wait for a rate-limit slot → send → record**.

```
pending ──claim──▶ processing ──2xx──────────────────────────▶ sent
   ▲                   │
   │                   ├─ retryable error, attempts left ──────▶ pending (next_attempt_at = now + backoff)
   │                   ├─ permanent error, or no attempts left ─▶ failed
   └── reaper ◀────────┘  lock expired (worker crashed)
```

| Provider result | Retry? | Why |
|---|---|---|
| 2xx | – | Sent. A 2xx with an unexpected body still counts as sent; retrying would send it twice. |
| 408, 429, 5xx, timeout, connection error | Yes | Temporary; the provider may recover. |
| Other 4xx | No | The request itself is wrong (bad number, rejected content). It would fail again. |

- **Backoff:** exponential with jitter. The delay doubles per attempt (2s, 4s, 8s … capped at 10 min) and
  a random point in its upper half is used, so messages that failed together do not all retry at the same moment.
  A `Retry-After` header from the provider wins if it asks for longer.
- **Attempts:** `MAX_ATTEMPTS` (default 5), then `failed`. Every try is stored in `delivery_attempts`
  with its status code, error and duration.
- **Crash safety:** a claim sets `locked_until`. If a worker dies mid-send, the reaper puts the row back
  in the queue after the lock expires. `attempts` doubles as a fencing token: a worker whose lock expired
  cannot overwrite the result of the worker that took over.
- **Delivery guarantee: at-least-once.** If a worker crashes after the provider accepted a message but
  before saving the result, the message is sent again. The notification ID is sent as an
  `Idempotency-Key` header so a provider that supports it can drop the duplicate.
- **Graceful shutdown:** on SIGTERM, senders stop claiming, and in-flight sends finish and are recorded.

### Rate limiting

A Redis sliding-window log (`internal/ratelimit`), shared by all worker replicas: at most 100 sends per
channel in **any** rolling second, not just per calendar second. The check runs as one Lua script, so
it is atomic across workers, and it uses Redis's clock, so workers do not have to agree on time.

Measured with 1000 SMS and 10% simulated provider failures: the batch drained at ~100/s, and
`redis-cli MONITOR` showed never more than 100 slots granted in any one-second span. Measured where the request
leaves the worker, a few more can appear in a rolling second during bursts, because the delay between
getting a slot and sending varies by a few milliseconds. If a provider enforces its limit strictly,
configure some headroom, e.g. `RATE_LIMIT_PER_SECOND=90`.

If Redis is unreachable, workers stop sending (and return claimed notifications to the queue) instead
of sending without a limit.

## Running tests

```bash
make test
```

## How I used AI tools

_TODO_
