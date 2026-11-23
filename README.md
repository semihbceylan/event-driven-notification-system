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

## Running tests

```bash
make test
```

## How I used AI tools

_TODO_
