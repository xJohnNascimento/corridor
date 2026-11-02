package verification

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"strconv"

	"connectrpc.com/connect"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/workflowservice/v1"
	"google.golang.org/protobuf/proto"

	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"
	"github.com/xJohnNascimento/ledger/api/ledger/v1/ledgerv1connect"

	corridorv1 "github.com/xJohnNascimento/corridor/api/corridor/v1"
	"github.com/xJohnNascimento/corridor/api/corridor/v1/corridorv1connect"
)

// Workflows is the part of Temporal's client the verifier needs.
type Workflows interface {
	DescribeWorkflowExecution(ctx context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
}

type Sources struct {
	HTTP         *http.Client
	LedgerURL    string
	LedgerSecret string
	LedgerID     string
	CorridorURL  string
	AuditorKey   string
	DittoURL     string
	DittoKey     string
	Temporal     Workflows
}

// bearerClient sends the key on every request, unary or streaming, because a transport sees
// both; a connect interceptor for unary calls would leave the export streams unauthenticated.
func bearerClient(base *http.Client, key string) *http.Client {
	transport := base.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &http.Client{Transport: bearerTransport{key: key, next: transport}, Timeout: base.Timeout}
}

type bearerTransport struct {
	key  string
	next http.RoundTripper
}

func (b bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.key)
	return b.next.RoundTrip(req)
}

// Collect reads every source into one dataset. Corridor's export is taken before and after the
// others; if it changed in between, the cut was not stable and the capture is repeated.
func Collect(ctx context.Context, s Sources) (*Dataset, error) {
	for range 3 {
		d := &Dataset{Running: map[string]bool{}}
		before, err := s.corridor(ctx, d)
		if err != nil {
			return nil, err
		}
		if err := s.ledger(ctx, d); err != nil {
			return nil, err
		}
		if err := s.provider(ctx, d); err != nil {
			return nil, err
		}
		after, err := s.corridor(ctx, &Dataset{Running: map[string]bool{}})
		if err != nil {
			return nil, err
		}
		if before == after {
			return d, s.workflows(ctx, d)
		}
	}
	return nil, errors.New("corridor kept changing during three captures; stop new payments and retry")
}

// framing recomputes an export's content hash and counts. records hashes the records alone,
// without the header and its capture time, to tell whether two captures saw the same data.
type framing struct {
	hash    hash.Hash
	records hash.Hash
	counts  map[string]int64
}

func newFraming() *framing {
	return &framing{hash: sha256.New(), records: sha256.New(), counts: map[string]int64{}}
}

func (f *framing) add(kind string, record proto.Message) error {
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(record)
	if err != nil {
		return err
	}
	size := len(encoded)
	if size > math.MaxUint32 {
		return fmt.Errorf("a %d byte record does not fit the frame", size)
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(size))
	f.hash.Write(length[:])
	f.hash.Write(encoded)
	if kind != "" {
		f.counts[kind]++
		f.records.Write(encoded)
	}
	return nil
}

type trailer struct {
	complete bool
	counts   map[string]int64
	hash     string
}

// check compares a stream's trailer with what was received. It returns why the source is
// incomplete, or "" when the trailer says COMPLETE and matches: a stream error, a missing or
// INCOMPLETE trailer, other counts or another hash.
func (f *framing) check(t *trailer, streamErr error) string {
	switch {
	case streamErr != nil:
		return "the stream failed: " + streamErr.Error()
	case t == nil:
		return "the stream ended without a trailer"
	case !t.complete:
		return "the source marked its own export INCOMPLETE"
	case !maps.Equal(t.counts, f.counts):
		return fmt.Sprintf("the trailer counts %v, received %v", t.counts, f.counts)
	case t.hash != hex.EncodeToString(f.hash.Sum(nil)):
		return "the trailer's content hash differs from the received records"
	}
	return ""
}

func (s Sources) ledger(ctx context.Context, d *Dataset) error {
	svc := ledgerv1connect.NewLedgerServiceClient(bearerClient(s.HTTP, s.LedgerSecret), s.LedgerURL)
	stream, err := svc.ExportAuditSnapshot(ctx, connect.NewRequest(&ledgerv1.ExportAuditSnapshotRequest{LedgerId: s.LedgerID}))
	if err != nil {
		d.Incomplete = append(d.Incomplete, "ledger: "+err.Error())
		return nil
	}
	d.Ledger = Ledger{Accounts: map[string]*ledgerv1.Account{}, Balances: map[string]*ledgerv1.Balance{}}
	f, end := newFraming(), (*trailer)(nil)
	manifest := Manifest{Source: "ledger"}
	for stream.Receive() {
		msg := stream.Msg()
		if t := msg.GetTrailer(); t != nil {
			end = &trailer{complete: t.Status == ledgerv1.SnapshotStatus_SNAPSHOT_STATUS_COMPLETE, counts: t.Counts, hash: t.ContentHash}
			manifest.ContentHash, manifest.Counts = t.ContentHash, t.Counts
			continue
		}
		kind := ""
		switch r := msg.Record.(type) {
		case *ledgerv1.ExportAuditSnapshotResponse_Header:
			manifest.CapturedAt = r.Header.CapturedAt.AsTime().String()
		case *ledgerv1.ExportAuditSnapshotResponse_Account:
			kind, d.Ledger.Accounts[r.Account.Id] = "account", r.Account
		case *ledgerv1.ExportAuditSnapshotResponse_Balance:
			kind, d.Ledger.Balances[r.Balance.AccountId] = "balance", r.Balance
		case *ledgerv1.ExportAuditSnapshotResponse_Entry:
			kind, d.Ledger.Entries = "entry", append(d.Ledger.Entries, r.Entry)
		case *ledgerv1.ExportAuditSnapshotResponse_Reservation:
			kind, d.Ledger.Reservations = "reservation", append(d.Ledger.Reservations, r.Reservation)
		case *ledgerv1.ExportAuditSnapshotResponse_Command:
			kind = "command"
		}
		if err := f.add(kind, msg); err != nil {
			return err
		}
	}
	if reason := f.check(end, stream.Err()); reason != "" {
		d.Incomplete = append(d.Incomplete, "ledger export: "+reason)
	}
	d.Manifests = append(d.Manifests, manifest)
	return nil
}

func (s Sources) corridor(ctx context.Context, d *Dataset) (string, error) {
	ops := corridorv1connect.NewOpsServiceClient(bearerClient(s.HTTP, s.AuditorKey), s.CorridorURL)
	stream, err := ops.ExportSnapshot(ctx, connect.NewRequest(&corridorv1.ExportSnapshotRequest{}))
	if err != nil {
		d.Incomplete = append(d.Incomplete, "corridor: "+err.Error())
		return "", nil
	}
	f, end := newFraming(), (*trailer)(nil)
	manifest := Manifest{Source: "corridor"}
	c := &d.Corridor
	for stream.Receive() {
		msg := stream.Msg()
		if t := msg.GetTrailer(); t != nil {
			end = &trailer{complete: t.Status == corridorv1.SnapshotStatus_SNAPSHOT_STATUS_COMPLETE, counts: t.Counts, hash: t.ContentHash}
			manifest.ContentHash, manifest.Counts = t.ContentHash, t.Counts
			continue
		}
		kind := ""
		switch r := msg.Record.(type) {
		case *corridorv1.ExportSnapshotResponse_Header:
			manifest.CapturedAt = r.Header.CapturedAt.AsTime().String()
		case *corridorv1.ExportSnapshotResponse_Payment:
			kind, c.Payments = "payment", append(c.Payments, r.Payment)
		case *corridorv1.ExportSnapshotResponse_Trade:
			kind, c.Trades = "trade", append(c.Trades, r.Trade)
		case *corridorv1.ExportSnapshotResponse_Evidence:
			kind, c.Evidence = "evidence", append(c.Evidence, r.Evidence)
		case *corridorv1.ExportSnapshotResponse_LedgerIntent:
			kind, c.Intents = "ledger_intent", append(c.Intents, r.LedgerIntent)
		case *corridorv1.ExportSnapshotResponse_Command:
			kind, c.Commands = "command", append(c.Commands, r.Command)
		case *corridorv1.ExportSnapshotResponse_Proposal:
			kind, c.Proposals = "proposal", append(c.Proposals, r.Proposal)
		}
		if err := f.add(kind, msg); err != nil {
			return "", err
		}
	}
	if reason := f.check(end, stream.Err()); reason != "" {
		d.Incomplete = append(d.Incomplete, "corridor export: "+reason)
	}
	d.Manifests = append(d.Manifests, manifest)
	return hex.EncodeToString(f.records.Sum(nil)), nil
}

type statementLine struct {
	Kind     string `json:"kind"`
	Movement *struct {
		Sequence     int64  `json:"sequence"`
		Currency     string `json:"currency"`
		Amount       string `json:"amount"`
		BalanceAfter string `json:"balance_after"`
		Kind         string `json:"kind"`
		Reference    string `json:"reference"`
	} `json:"movement"`
	Outcome *struct {
		Operation string `json:"operation"`
		Reference string `json:"reference"`
		Status    string `json:"status"`
		Currency  string `json:"currency"`
		Amount    string `json:"amount"`
	} `json:"outcome"`
}

// providerAmounts parses the amounts of one statement line; the first one that is not a decimal
// integer makes the statement incomplete.
type providerAmounts struct {
	err error
}

// parseOptional reads an amount the provider leaves out when the outcome has none, such as a
// rejected or closed trade; a value that is present must still be an amount.
func (p *providerAmounts) parseOptional(field, value string) int64 {
	if value == "" {
		return 0
	}
	return p.parse(field, value)
}

func (p *providerAmounts) parse(field, value string) int64 {
	v, err := strconv.ParseInt(value, 10, 64)
	if err != nil && p.err == nil {
		p.err = fmt.Errorf("%s %q is not an amount", field, value)
	}
	return v
}

// provider reads a full statement from its first movement, and checks its page count and content
// hash against the manifest; a truncated statement is INCOMPLETE.
func (s Sources) provider(ctx context.Context, d *Dataset) error {
	var manifest struct {
		ID          string `json:"statement_id"`
		CapturedAt  string `json:"captured_at"`
		ContentHash string `json:"content_hash"`
		LineCount   int64  `json:"line_count"`
		Raw         []struct {
			Currency       string `json:"currency"`
			OpeningBalance string `json:"opening_balance"`
			ClosingBalance string `json:"closing_balance"`
			FromSequence   int64  `json:"from_sequence"`
			ToSequence     int64  `json:"to_sequence"`
		} `json:"currencies"`
	}
	if err := s.ditto(ctx, http.MethodPost, "/v1/statements", []byte("{}"), &manifest); err != nil {
		d.Incomplete = append(d.Incomplete, "provider: "+err.Error())
		return nil
	}
	amounts := &providerAmounts{}
	for _, c := range manifest.Raw {
		d.Provider.Currencies = append(d.Provider.Currencies, ProviderCurrency{
			Currency: c.Currency, Opening: amounts.parse("opening balance", c.OpeningBalance), Closing: amounts.parse("closing balance", c.ClosingBalance),
			FromSequence: c.FromSequence, ToSequence: c.ToSequence,
		})
	}
	h, lines, cursor := sha256.New(), int64(0), ""
	for {
		var page struct {
			Lines      []json.RawMessage `json:"lines"`
			NextCursor string            `json:"next_cursor"`
		}
		path := "/v1/statements/" + url.PathEscape(manifest.ID) + "/pages?cursor=" + url.QueryEscape(cursor)
		if err := s.ditto(ctx, http.MethodGet, path, nil, &page); err != nil {
			d.Incomplete = append(d.Incomplete, "provider page: "+err.Error())
			return nil
		}
		for _, raw := range page.Lines {
			h.Write(raw)
			lines++
			var line statementLine
			if err := json.Unmarshal(raw, &line); err != nil {
				d.Incomplete = append(d.Incomplete, "provider statement line: "+err.Error())
				return nil
			}
			switch {
			case line.Movement != nil:
				m := line.Movement
				d.Provider.Movements = append(d.Provider.Movements, Movement{m.Currency, m.Sequence, amounts.parse("movement amount", m.Amount),
					amounts.parse("balance after", m.BalanceAfter), m.Kind, m.Reference})
			case line.Outcome != nil:
				o := line.Outcome
				d.Provider.Outcomes = append(d.Provider.Outcomes, Outcome{o.Operation, o.Reference, o.Status, o.Currency, amounts.parseOptional("outcome amount", o.Amount)})
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if amounts.err != nil {
		d.Incomplete = append(d.Incomplete, "provider statement: "+amounts.err.Error())
	}
	if lines != manifest.LineCount || hex.EncodeToString(h.Sum(nil)) != manifest.ContentHash {
		d.Incomplete = append(d.Incomplete, fmt.Sprintf("provider statement: %d of %d lines or another hash", lines, manifest.LineCount))
	}
	d.Manifests = append(d.Manifests, Manifest{Source: "provider", CapturedAt: manifest.CapturedAt, ContentHash: manifest.ContentHash,
		Counts: map[string]int64{"lines": lines}})
	return nil
}

func (s Sources) ditto(ctx context.Context, method, path string, body []byte, into any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.DittoURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.DittoKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s", method, path, strconv.Quote(string(data)))
	}
	return json.Unmarshal(data, into)
}

// workflows asks Temporal about every payment that is not terminal. An unreachable Temporal makes
// the result INCOMPLETE; it never proves a payment absent.
func (s Sources) workflows(ctx context.Context, d *Dataset) error {
	for _, p := range d.Corridor.Payments {
		if terminal(p.Status) {
			continue
		}
		resp, err := s.Temporal.DescribeWorkflowExecution(ctx, "payment/"+p.Id, "")
		if err != nil {
			d.Incomplete = append(d.Incomplete, "temporal: "+err.Error())
			return nil
		}
		d.Running[p.Id] = resp.GetWorkflowExecutionInfo().GetStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING
	}
	return nil
}
