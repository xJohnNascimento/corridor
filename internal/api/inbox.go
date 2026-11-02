package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	maxWebhookBody = 64 << 10
	webhookSkew    = 5 * time.Minute
)

// ParseWebhookSecrets reads Standard Webhooks secrets ("whsec_" and base64), space-separated
// during a rotation.
func ParseWebhookSecrets(value string) ([][]byte, error) {
	var keys [][]byte
	for _, secret := range strings.Fields(value) {
		encoded, ok := strings.CutPrefix(secret, "whsec_")
		key, err := base64.StdEncoding.DecodeString(encoded)
		if !ok || err != nil || len(key) < 24 {
			return nil, errors.New("webhook secrets must be whsec_ followed by at least 24 base64-encoded bytes")
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil, errors.New("no webhook secret")
	}
	return keys, nil
}

// webhookInbox verifies a provider event and stores it before answering 2xx. The event is a hint:
// the dispatcher signals it, and the workflow reads the payout back before money moves.
func (s *Server) webhookInbox(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		http.Error(w, "the body is too large", http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	id, timestamp := r.Header.Get("webhook-id"), r.Header.Get("webhook-timestamp")
	if err := verifySignature(s.webhookSecrets, id, timestamp, r.Header.Get("webhook-signature"), body, time.Now()); err != nil {
		slog.WarnContext(r.Context(), "webhook refused", "event", id, "error", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	var event struct {
		EventID string `json:"event_id"`
		Data    struct {
			Reference string `json:"reference"`
			Status    string `json:"status"`
			Sequence  *int   `json:"sequence"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &event); err != nil || event.EventID != id {
		http.Error(w, "the body does not match its webhook-id", http.StatusBadRequest)
		return
	}
	if event.Data.Status == "" || event.Data.Sequence == nil {
		http.Error(w, "the event needs a data.status and an integer data.sequence", http.StatusBadRequest)
		return
	}
	conflict, err := s.store.ReceiveWebhook(r.Context(), "ditto", id, event.Data.Reference, body)
	if err != nil {
		slog.ErrorContext(r.Context(), "store webhook", "event", id, "error", err)
		http.Error(w, "retry later", http.StatusServiceUnavailable)
		return
	}
	if conflict {
		slog.ErrorContext(r.Context(), "webhook event reused with another payload", "event", id)
	}
	w.WriteHeader(http.StatusNoContent)
}

func verifySignature(secrets [][]byte, id, timestamp, header string, body []byte, now time.Time) error {
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || id == "" {
		return errors.New("missing webhook-id or webhook-timestamp")
	}
	if skew := now.Sub(time.Unix(seconds, 0)); math.Abs(float64(skew)) > float64(webhookSkew) {
		return fmt.Errorf("timestamp is %s off", skew)
	}
	for _, key := range secrets {
		mac := hmac.New(sha256.New, key)
		_, _ = fmt.Fprintf(mac, "%s.%d.", id, seconds)
		mac.Write(body)
		expected := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
		for _, signature := range strings.Fields(header) {
			if hmac.Equal([]byte(signature), []byte(expected)) {
				return nil
			}
		}
	}
	return errors.New("no signature matches an active secret")
}
