package verification

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"go.temporal.io/api/workflowservice/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"
	"github.com/xJohnNascimento/ledger/api/ledger/v1/ledgerv1connect"

	corridorv1 "github.com/xJohnNascimento/corridor/api/corridor/v1"
	"github.com/xJohnNascimento/corridor/api/corridor/v1/corridorv1connect"
)

const (
	fakeLedgerKey   = "ledger-key"
	fakeAuditorKey  = "auditor-key"
	fakeDittoKey    = "ditto-key"
	badHash         = "bad hash"
	wrongCounts     = "wrong counts"
	missingTrailer  = "missing trailer"
	incompleteState = "incomplete trailer"
)

// world is the three sources as fakes. Each fault is applied to one source's export; the corridor
// export can change between calls to exercise the stable cut.
type world struct {
	ledgerFault, corridorFault, dittoFault string
	corridorPayments                       func(call int) int
	status                                 corridorv1.PaymentStatus
	temporalErr                            error
	ledgerCalls, corridorCalls             atomic.Int32
}

func newWorld() *world {
	return &world{
		corridorPayments: func(int) int { return 1 },
		status:           corridorv1.PaymentStatus_PAYMENT_STATUS_COMPLETED,
	}
}

// frameHash is the export's content hash written out independently of the collector: every
// message but the trailer, each behind its four-byte length.
func frameHash[M proto.Message](messages []M) string {
	h := sha256.New()
	for _, m := range messages {
		encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
		if err != nil {
			panic(err)
		}
		h.Write(binary.BigEndian.AppendUint32(nil, uint32(len(encoded)))) //nolint:gosec // fixture messages are a few bytes
		h.Write(encoded)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func faulty(fault string, status *int32, counts map[string]int64, hash *string) (omit bool) {
	switch fault {
	case badHash:
		*hash = strings.Repeat("0", 64)
	case wrongCounts:
		for kind := range counts {
			counts[kind]++
		}
	case missingTrailer:
		return true
	case incompleteState:
		*status = int32(ledgerv1.SnapshotStatus_SNAPSHOT_STATUS_INCOMPLETE)
	}
	return false
}

func (w *world) ledgerMessages() []*ledgerv1.ExportAuditSnapshotResponse {
	records := []*ledgerv1.ExportAuditSnapshotResponse{
		{Record: &ledgerv1.ExportAuditSnapshotResponse_Header{Header: &ledgerv1.SnapshotHeader{SchemaVersion: 1, CapturedAt: timestamppb.New(time.Unix(1, 0))}}},
		{Record: &ledgerv1.ExportAuditSnapshotResponse_Account{Account: &ledgerv1.Account{Id: "a-1", LedgerId: "corridor"}}},
	}
	status, counts, hash := int32(ledgerv1.SnapshotStatus_SNAPSHOT_STATUS_COMPLETE), map[string]int64{"account": 1}, frameHash(records)
	if faulty(w.ledgerFault, &status, counts, &hash) {
		return records
	}
	return append(records, &ledgerv1.ExportAuditSnapshotResponse{Record: &ledgerv1.ExportAuditSnapshotResponse_Trailer{Trailer: &ledgerv1.SnapshotTrailer{
		Status: ledgerv1.SnapshotStatus(status), Counts: counts, ContentHash: hash,
	}}})
}

func (w *world) corridorMessages(call int) []*corridorv1.ExportSnapshotResponse {
	records := []*corridorv1.ExportSnapshotResponse{
		{Record: &corridorv1.ExportSnapshotResponse_Header{Header: &corridorv1.SnapshotHeader{SchemaVersion: 1, CapturedAt: timestamppb.New(time.Unix(int64(call), 0))}}},
	}
	n := w.corridorPayments(call)
	for i := range n {
		records = append(records, &corridorv1.ExportSnapshotResponse{Record: &corridorv1.ExportSnapshotResponse_Payment{Payment: &corridorv1.Payment{
			Id: "p-" + string(rune('a'+i)), Status: w.status,
		}}})
	}
	status, counts, hash := int32(corridorv1.SnapshotStatus_SNAPSHOT_STATUS_COMPLETE), map[string]int64{"payment": int64(n)}, frameHash(records)
	if faulty(w.corridorFault, &status, counts, &hash) {
		return records
	}
	return append(records, &corridorv1.ExportSnapshotResponse{Record: &corridorv1.ExportSnapshotResponse_Trailer{Trailer: &corridorv1.SnapshotTrailer{
		Status: corridorv1.SnapshotStatus(status), Counts: counts, ContentHash: hash,
	}}})
}

type fakeLedger struct {
	ledgerv1connect.UnimplementedLedgerServiceHandler
	w *world
}

func (f fakeLedger) ExportAuditSnapshot(_ context.Context, _ *connect.Request[ledgerv1.ExportAuditSnapshotRequest], stream *connect.ServerStream[ledgerv1.ExportAuditSnapshotResponse]) error {
	f.w.ledgerCalls.Add(1)
	for _, m := range f.w.ledgerMessages() {
		if err := stream.Send(m); err != nil {
			return err
		}
	}
	return nil
}

type fakeOps struct {
	corridorv1connect.UnimplementedOpsServiceHandler
	w *world
}

func (f fakeOps) ExportSnapshot(_ context.Context, _ *connect.Request[corridorv1.ExportSnapshotRequest], stream *connect.ServerStream[corridorv1.ExportSnapshotResponse]) error {
	call := int(f.w.corridorCalls.Add(1))
	for _, m := range f.w.corridorMessages(call) {
		if err := stream.Send(m); err != nil {
			return err
		}
	}
	return nil
}

// statement serves one page of two lines: a funding movement and a trade outcome.
func (w *world) statement() (manifest map[string]any, lines []string) {
	lines = []string{
		`{"kind":"MOVEMENT","movement":{"sequence":1,"currency":"USD","amount":"100","balance_after":"100","kind":"FUNDING","reference":"seed"}}`,
		`{"kind":"OUTCOME","outcome":{"operation":"TRADE","reference":"p-a","status":"FILLED","currency":"USD","amount":"100"}}`,
		`{"kind":"OUTCOME","outcome":{"operation":"TRADE","reference":"p-b","status":"CLOSED"}}`,
	}
	closing := "100"
	switch w.dittoFault {
	case "malformed amount":
		lines[0] = strings.Replace(lines[0], `"amount":"100"`, `"amount":"1,00"`, 1)
	case "malformed line":
		lines[1] = `"not an object"`
	case "malformed balance":
		closing = "a lot"
	}
	h := sha256.New()
	for _, line := range lines {
		h.Write([]byte(line))
	}
	hash, count := hex.EncodeToString(h.Sum(nil)), int64(len(lines))
	switch w.dittoFault {
	case badHash:
		hash = strings.Repeat("0", 64)
	case "truncated":
		count++
	}
	return map[string]any{
		"statement_id": "s-1", "captured_at": "now", "content_hash": hash, "line_count": count,
		"currencies": []map[string]any{{"currency": "USD", "opening_balance": "0", "closing_balance": closing, "from_sequence": 1, "to_sequence": 1}},
	}, lines
}

func (w *world) ditto(rw http.ResponseWriter, req *http.Request) {
	manifest, lines := w.statement()
	rw.Header().Set("Content-Type", "application/json")
	if req.Method == http.MethodPost {
		_ = json.NewEncoder(rw).Encode(manifest)
		return
	}
	raw := make([]json.RawMessage, len(lines))
	for i, line := range lines {
		raw[i] = json.RawMessage(line)
	}
	_ = json.NewEncoder(rw).Encode(map[string]any{"lines": raw, "next_cursor": ""})
}

// requireKey answers 401 unless the request carries the source's key, which is what makes a
// stream that forgot its bearer header fail here.
func requireKey(key string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer "+key {
			http.Error(rw, "unauthenticated", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(rw, req)
	})
}

type fakeTemporal struct {
	err error
}

func (f fakeTemporal) DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	return nil, f.err
}

func (w *world) sources(t *testing.T) Sources {
	t.Helper()
	mux := http.NewServeMux()
	path, handler := ledgerv1connect.NewLedgerServiceHandler(fakeLedger{w: w})
	mux.Handle(path, requireKey(fakeLedgerKey, handler))
	path, handler = corridorv1connect.NewOpsServiceHandler(fakeOps{w: w})
	mux.Handle(path, requireKey(fakeAuditorKey, handler))
	mux.Handle("/v1/statements", requireKey(fakeDittoKey, http.HandlerFunc(w.ditto)))
	mux.Handle("/v1/statements/", requireKey(fakeDittoKey, http.HandlerFunc(w.ditto)))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return Sources{
		HTTP: server.Client(), LedgerURL: server.URL, LedgerSecret: fakeLedgerKey, LedgerID: "corridor",
		CorridorURL: server.URL, AuditorKey: fakeAuditorKey, DittoURL: server.URL, DittoKey: fakeDittoKey,
		Temporal: fakeTemporal{err: w.temporalErr},
	}
}

func TestACompleteCaptureHasNoMissingSource(t *testing.T) {
	w := newWorld()
	d, err := Collect(t.Context(), w.sources(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Incomplete) != 0 || len(d.Manifests) != 3 || len(d.Corridor.Payments) != 1 || len(d.Ledger.Accounts) != 1 || len(d.Provider.Movements) != 1 || len(d.Provider.Outcomes) != 2 {
		t.Fatalf("incomplete %v, %d manifests, %d payments, %d accounts, %d movements, %d outcomes",
			d.Incomplete, len(d.Manifests), len(d.Corridor.Payments), len(d.Ledger.Accounts), len(d.Provider.Movements), len(d.Provider.Outcomes))
	}
}

func TestABrokenExportIsIncompleteNeverPass(t *testing.T) {
	faults := []struct{ name, reason string }{
		{badHash, "content hash"},
		{wrongCounts, "counts"},
		{missingTrailer, "without a trailer"},
		{incompleteState, "INCOMPLETE"},
	}
	for _, source := range []string{"ledger", "corridor"} {
		for _, fault := range faults {
			t.Run(source+" with "+fault.name, func(t *testing.T) {
				w := newWorld()
				if source == "ledger" {
					w.ledgerFault = fault.name
				} else {
					w.corridorFault = fault.name
				}
				d, err := Collect(t.Context(), w.sources(t))
				if err != nil {
					t.Fatal(err)
				}
				if len(d.Incomplete) != 1 || !strings.HasPrefix(d.Incomplete[0], source+" export:") || !strings.Contains(d.Incomplete[0], fault.reason) {
					t.Fatalf("incomplete %q, want one %s export reason mentioning %q", d.Incomplete, source, fault.reason)
				}
				if r := Verify(d); r.Result != Incomplete {
					t.Fatalf("the report is %s", r.Result)
				}
			})
		}
	}
}

func TestABrokenStatementIsIncompleteNeverPass(t *testing.T) {
	faults := []struct{ name, reason string }{
		{badHash, "another hash"},
		{"truncated", "another hash"},
		{"malformed amount", "movement amount"},
		{"malformed balance", "closing balance"},
		{"malformed line", "provider statement line"},
	}
	for _, fault := range faults {
		t.Run(fault.name, func(t *testing.T) {
			w := newWorld()
			w.dittoFault = fault.name
			d, err := Collect(t.Context(), w.sources(t))
			if err != nil {
				t.Fatal(err)
			}
			if len(d.Incomplete) == 0 || !strings.Contains(strings.Join(d.Incomplete, "\n"), fault.reason) {
				t.Fatalf("incomplete %q, want a reason mentioning %q", d.Incomplete, fault.reason)
			}
		})
	}
}

func TestACorridorThatChangesBetweenCallsIsCapturedAgain(t *testing.T) {
	w := newWorld()
	w.corridorPayments = func(call int) int { return min(call, 2) }
	d, err := Collect(t.Context(), w.sources(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Corridor.Payments) != 2 || w.corridorCalls.Load() != 4 || w.ledgerCalls.Load() != 2 {
		t.Fatalf("%d payments after %d corridor and %d ledger calls, want 2 after 4 and 2",
			len(d.Corridor.Payments), w.corridorCalls.Load(), w.ledgerCalls.Load())
	}
}

func TestACorridorThatNeverSettlesFailsTheCapture(t *testing.T) {
	w := newWorld()
	w.corridorPayments = func(call int) int { return call }
	_, err := Collect(t.Context(), w.sources(t))
	if err == nil || !strings.Contains(err.Error(), "kept changing") || w.ledgerCalls.Load() != 3 {
		t.Fatalf("%v after %d ledger captures, want three captures and an error", err, w.ledgerCalls.Load())
	}
}

func TestAnUnreachableTemporalIsIncomplete(t *testing.T) {
	w := newWorld()
	w.status = corridorv1.PaymentStatus_PAYMENT_STATUS_CONVERTING
	w.temporalErr = errors.New("down")
	d, err := Collect(t.Context(), w.sources(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Incomplete) != 1 || d.Incomplete[0] != "temporal: down" {
		t.Fatalf("incomplete %q", d.Incomplete)
	}
}

func TestAnExportStreamIsAuthenticatedWithItsKey(t *testing.T) {
	w := newWorld()
	sources := w.sources(t)
	sources.AuditorKey = "another key"
	d, err := Collect(t.Context(), sources)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Incomplete) == 0 || !strings.Contains(d.Incomplete[0], "unauthenticated") {
		t.Fatalf("a wrong key on the export stream gave %q", d.Incomplete)
	}
}
