package notification

import (
	"context"
	"fmt"
	"time"
)

// QueueDepth is how many notifications of a channel are waiting.
type QueueDepth struct {
	// Due: ready to send now. A growing number means the workers (or the
	// rate limit) cannot keep up.
	Due int64 `json:"due"`
	// Waiting: scheduled for later, or backing off before a retry.
	Waiting int64 `json:"waiting"`
	// Processing: claimed by a worker right now.
	Processing int64 `json:"processing"`
}

type Percentiles struct {
	P50 *float64 `json:"p50"`
	P95 *float64 `json:"p95"`
	P99 *float64 `json:"p99"`
}

// ChannelStats covers one channel over a recent time window.
type ChannelStats struct {
	Queue QueueDepth `json:"queue"`

	Sent   int64 `json:"sent"`   // notifications delivered in the window
	Failed int64 `json:"failed"` // notifications that gave up in the window
	// SuccessRate is Sent / (Sent + Failed): of the notifications that
	// finished in the window, how many were delivered. Nil if none finished.
	SuccessRate *float64 `json:"success_rate"`

	Attempts int64 `json:"attempts"` // provider calls in the window, including retries
	// AttemptSuccessRate is the share of provider calls that succeeded.
	// It drops during an outage even if every notification is eventually sent.
	AttemptSuccessRate *float64 `json:"attempt_success_rate"`

	// SentPerSecondAvg is Sent averaged over the whole window, not the
	// current rate. For the current rate use Prometheus:
	// rate(notifications_processed_total{outcome="sent"}[1m]).
	SentPerSecondAvg float64 `json:"sent_per_second_avg"`

	// ProviderLatencyMs: how long provider calls took.
	ProviderLatencyMs Percentiles `json:"provider_latency_ms"`
	// DeliveryLatencyMs: from when a notification became due (created, or
	// its scheduled time) until it was sent. Includes queueing and retries.
	DeliveryLatencyMs Percentiles `json:"delivery_latency_ms"`
}

// QueueDepths returns the current queue per channel. Both queries hit
// partial indexes, so they stay fast however many finished rows there are.
func (s *Store) QueueDepths(ctx context.Context) (map[Channel]*QueueDepth, error) {
	out := make(map[Channel]*QueueDepth, len(Channels))
	for _, ch := range Channels {
		out[ch] = &QueueDepth{}
	}

	rows, err := s.pool.Query(ctx, `
		SELECT channel,
		       count(*) FILTER (WHERE next_attempt_at <= now()),
		       count(*) FILTER (WHERE next_attempt_at > now())
		FROM notifications WHERE status = 'pending'
		GROUP BY channel`)
	if err != nil {
		return nil, fmt.Errorf("queue depth: %w", err)
	}
	for rows.Next() {
		var ch string
		var due, waiting int64
		if err := rows.Scan(&ch, &due, &waiting); err != nil {
			rows.Close()
			return nil, err
		}
		if q, ok := out[Channel(ch)]; ok {
			q.Due, q.Waiting = due, waiting
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.pool.Query(ctx, `
		SELECT channel, count(*) FROM notifications WHERE status = 'processing' GROUP BY channel`)
	if err != nil {
		return nil, fmt.Errorf("processing count: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ch string
		var n int64
		if err := rows.Scan(&ch, &n); err != nil {
			return nil, err
		}
		if q, ok := out[Channel(ch)]; ok {
			q.Processing = n
		}
	}
	return out, rows.Err()
}

// Stats returns per-channel statistics for the last `window`. Everything is
// computed from the database, so the numbers are correct no matter how many
// worker replicas are running.
func (s *Store) Stats(ctx context.Context, window time.Duration) (map[Channel]*ChannelStats, error) {
	queue, err := s.QueueDepths(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[Channel]*ChannelStats, len(Channels))
	for _, ch := range Channels {
		out[ch] = &ChannelStats{Queue: *queue[ch]}
	}
	secs := window.Seconds()

	// Provider calls.
	rows, err := s.pool.Query(ctx, `
		SELECT n.channel,
		       count(*),
		       count(*) FILTER (WHERE d.error IS NULL),
		       percentile_cont(0.50) WITHIN GROUP (ORDER BY d.duration_ms),
		       percentile_cont(0.95) WITHIN GROUP (ORDER BY d.duration_ms),
		       percentile_cont(0.99) WITHIN GROUP (ORDER BY d.duration_ms)
		FROM delivery_attempts d
		JOIN notifications n ON n.id = d.notification_id
		WHERE d.started_at >= now() - make_interval(secs => $1)
		GROUP BY n.channel`, secs)
	if err != nil {
		return nil, fmt.Errorf("attempt stats: %w", err)
	}
	for rows.Next() {
		var ch string
		var total, ok int64
		var p Percentiles
		if err := rows.Scan(&ch, &total, &ok, &p.P50, &p.P95, &p.P99); err != nil {
			rows.Close()
			return nil, err
		}
		if st, found := out[Channel(ch)]; found {
			st.Attempts = total
			st.AttemptSuccessRate = ratio(ok, total)
			st.ProviderLatencyMs = p
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Delivered notifications.
	rows, err = s.pool.Query(ctx, `
		SELECT channel,
		       count(*),
		       percentile_cont(0.50) WITHIN GROUP (ORDER BY lat),
		       percentile_cont(0.95) WITHIN GROUP (ORDER BY lat),
		       percentile_cont(0.99) WITHIN GROUP (ORDER BY lat)
		FROM (
		    SELECT channel, extract(epoch FROM sent_at - coalesce(scheduled_at, created_at)) * 1000 AS lat
		    FROM notifications
		    WHERE status = 'sent' AND sent_at >= now() - make_interval(secs => $1)
		) s
		GROUP BY channel`, secs)
	if err != nil {
		return nil, fmt.Errorf("delivery stats: %w", err)
	}
	for rows.Next() {
		var ch string
		var sent int64
		var p Percentiles
		if err := rows.Scan(&ch, &sent, &p.P50, &p.P95, &p.P99); err != nil {
			rows.Close()
			return nil, err
		}
		if st, found := out[Channel(ch)]; found {
			st.Sent = sent
			st.SentPerSecondAvg = round2(float64(sent) / secs)
			st.DeliveryLatencyMs = p
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Notifications that gave up.
	rows, err = s.pool.Query(ctx, `
		SELECT channel, count(*) FROM notifications
		WHERE status = 'failed' AND updated_at >= now() - make_interval(secs => $1)
		GROUP BY channel`, secs)
	if err != nil {
		return nil, fmt.Errorf("failure stats: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ch string
		var failed int64
		if err := rows.Scan(&ch, &failed); err != nil {
			return nil, err
		}
		if st, found := out[Channel(ch)]; found {
			st.Failed = failed
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, st := range out {
		st.SuccessRate = ratio(st.Sent, st.Sent+st.Failed)
		st.ProviderLatencyMs = st.ProviderLatencyMs.rounded()
		st.DeliveryLatencyMs = st.DeliveryLatencyMs.rounded()
	}
	return out, nil
}

func ratio(part, total int64) *float64 {
	if total == 0 {
		return nil
	}
	r := round4(float64(part) / float64(total))
	return &r
}

func (p Percentiles) rounded() Percentiles {
	r := func(v *float64) *float64 {
		if v == nil {
			return nil
		}
		x := round2(*v)
		return &x
	}
	return Percentiles{P50: r(p.P50), P95: r(p.P95), P99: r(p.P99)}
}

func round2(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }
func round4(v float64) float64 { return float64(int64(v*10000+0.5)) / 10000 }
