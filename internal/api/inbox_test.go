package api_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func sign(id string, at time.Time, body []byte) string {
	key, _ := base64.StdEncoding.DecodeString(webhookSecret[len("whsec_"):])
	mac := hmac.New(sha256.New, key)
	_, _ = fmt.Fprintf(mac, "%s.%d.", id, at.Unix())
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func (h *harness) deliver(id string, at time.Time, body []byte, signature string) int {
	h.t.Helper()
	req, _ := http.NewRequestWithContext(h.t.Context(), http.MethodPost, h.internal.URL+"/webhooks/ditto", bytes.NewReader(body))
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", strconv.FormatInt(at.Unix(), 10))
	req.Header.Set("webhook-signature", signature)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestTheInboxStoresOnlySignedEventsOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	now := time.Now()
	body := []byte(`{"event_id":"evt_1","type":"payout.status_changed","data":{"reference":"o-1","status":"COMPLETED","sequence":3}}`)

	for name, c := range map[string]struct {
		at        time.Time
		signature string
		want      int
	}{
		"forged":            {now, "v1,AAAA", http.StatusUnauthorized},
		"stale":             {now.Add(-6 * time.Minute), sign("evt_1", now.Add(-6*time.Minute), body), http.StatusUnauthorized},
		"signed":            {now, sign("evt_1", now, body), http.StatusNoContent},
		"duplicate":         {now.Add(time.Second), sign("evt_1", now.Add(time.Second), body), http.StatusNoContent},
		"rotated alongside": {now, "v1,AAAA " + sign("evt_1", now, body), http.StatusNoContent},
	} {
		if got := h.deliver("evt_1", c.at, body, c.signature); got != c.want {
			t.Errorf("%s: %d, want %d", name, got, c.want)
		}
	}
	other := []byte(`{"event_id":"evt_1","type":"payout.status_changed","data":{"reference":"o-1","status":"FAILED","sequence":3}}`)
	if got := h.deliver("evt_1", now, other, sign("evt_1", now, other)); got != http.StatusNoContent {
		t.Fatalf("a reused id with another payload: %d", got)
	}
	var pending, conflicts int
	err := h.store.Pool().QueryRow(context.Background(), `
		select count(*) filter (where status = 'PENDING'), count(*) filter (where status = 'CONFLICT') from fx.webhook_inbox`,
	).Scan(&pending, &conflicts)
	if err != nil || pending != 1 || conflicts != 1 {
		t.Fatalf("%d pending and %d conflict rows (%v), want one of each", pending, conflicts, err)
	}
}

func TestTheInboxRefusesEventsTheDispatcherCouldNotRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	now := time.Now()
	cases := map[string]struct {
		body string
		want int
	}{
		"a missing status":      {`{"event_id":"evt_x","data":{"reference":"o-1","sequence":3}}`, http.StatusBadRequest},
		"an empty status":       {`{"event_id":"evt_x","data":{"reference":"o-1","status":"","sequence":3}}`, http.StatusBadRequest},
		"a missing sequence":    {`{"event_id":"evt_x","data":{"reference":"o-1","status":"COMPLETED"}}`, http.StatusBadRequest},
		"a fractional sequence": {`{"event_id":"evt_x","data":{"reference":"o-1","status":"COMPLETED","sequence":3.5}}`, http.StatusBadRequest},
		"a textual sequence":    {`{"event_id":"evt_x","data":{"reference":"o-1","status":"COMPLETED","sequence":"3"}}`, http.StatusBadRequest},
		"no data":               {`{"event_id":"evt_x"}`, http.StatusBadRequest},
		"another event id":      {`{"event_id":"evt_y","data":{"reference":"o-1","status":"COMPLETED","sequence":3}}`, http.StatusBadRequest},
		"a sequence of zero":    {`{"event_id":"evt_x","data":{"reference":"o-1","status":"COMPLETED","sequence":0}}`, http.StatusNoContent},
		"a body over the limit": {`{"event_id":"evt_x","pad":"` + strings.Repeat("a", 70<<10) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for name, c := range cases {
		body := []byte(c.body)
		if got := h.deliver("evt_x", now, body, sign("evt_x", now, body)); got != c.want {
			t.Errorf("%s: %d, want %d", name, got, c.want)
		}
	}
	var stored int
	if err := h.store.Pool().QueryRow(context.Background(), "select count(*) from fx.webhook_inbox").Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("%d events stored (%v), want only the valid one", stored, err)
	}
}
