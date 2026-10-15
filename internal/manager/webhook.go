package manager

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/mdenesfe/wax/internal/cryptoutil"
)

// Webhook delivery policy.
const (
	webhookMaxAttempts = 12               // ~1 day with DefaultTiming
	webhookTimeout     = 15 * time.Second // per attempt
)

// Sign computes the X-Wax-Signature value for a body.
func Sign(secret, body []byte) string {
	return "sha256=" + hex.EncodeToString(cryptoutil.HMACSHA256(secret, body))
}

// webhookClient does not follow redirects: a webhook must answer at the
// configured URL.
func newWebhookClient() *http.Client {
	return &http.Client{
		Timeout:       webhookTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// deliveryLoop delivers this instance's queued webhooks in order.
func (in *Instance) deliveryLoop(ctx context.Context) {
	defer in.m.recoverLoop("webhooks", in.ID)
	for {
		wait, err := in.deliverOne(ctx)
		if err != nil && ctx.Err() == nil {
			in.log.Warn("webhook delivery error", "error", err)
		}
		if wait == 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-in.hookKick:
		case <-time.After(wait):
		}
	}
}

// deliverOne attempts the head of the queue and returns how long to wait
// before the next call (0 = immediately).
func (in *Instance) deliverOne(ctx context.Context) (time.Duration, error) {
	st, tm := in.m.store, in.m.cfg.Timing
	it, due, err := st.NextWebhook(ctx, in.ID)
	if err != nil {
		return tm.WebhookRetry.Min, err
	}
	if it == nil {
		if due != nil {
			return max(time.Until(*due), time.Millisecond), nil
		}
		return tm.Idle, nil
	}
	cfg := in.config()
	if cfg.WebhookURL == "" {
		// Webhook removed meanwhile: drop what is queued.
		return 0, st.MarkWebhook(ctx, it.ID, "dead", time.Now(), "no webhook url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.WebhookURL, bytes.NewReader(it.Payload))
	if err != nil {
		return 0, st.MarkWebhook(ctx, it.ID, "dead", time.Now(), err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "wax-webhook/1")
	req.Header.Set("X-Wax-Event", it.Event)
	req.Header.Set("X-Wax-Delivery", strconv.FormatInt(it.ID, 10))
	req.Header.Set("X-Wax-Instance", in.ID)
	if len(cfg.WebhookSecret) > 0 {
		req.Header.Set("X-Wax-Signature", Sign(cfg.WebhookSecret, it.Payload))
	}
	resp, err := in.m.hookHTTP.Do(req)
	if err == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return 0, st.MarkWebhook(ctx, it.ID, "delivered", time.Now(), "")
		}
		err = fmt.Errorf("http %d", resp.StatusCode)
	}
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if it.Attempts+1 >= webhookMaxAttempts {
		in.log.Error("webhook delivery given up", "delivery", it.ID, "event", it.Event, "error", err)
		return 0, st.MarkWebhook(ctx, it.ID, "dead", time.Now(), err.Error())
	}
	delay := tm.WebhookRetry.Delay(it.Attempts + 1)
	if merr := st.MarkWebhook(ctx, it.ID, "pending", time.Now().Add(delay), err.Error()); merr != nil {
		return delay, errors.Join(err, merr)
	}
	return delay, err
}
