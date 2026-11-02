package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	corridorv1 "github.com/xJohnNascimento/corridor/api/corridor/v1"
	"github.com/xJohnNascimento/corridor/api/corridor/v1/corridorv1connect"
	"github.com/xJohnNascimento/corridor/internal/provider"
	"github.com/xJohnNascimento/corridor/internal/store"
)

const opsUsage = `usage:
  fx cli ops reviews --as OPERATOR
  fx cli ops evidence read --payment ID
  fx cli ops evidence statement --payment ID
  fx cli ops propose --as OPERATOR --payment ID --decision ACCEPT_FILL|PAID|NOT_PAID [--evidence ID --digest HEX]
  fx cli ops approve --as OPERATOR --proposal ID --digest HEX`

// ops is the operators' console. Reviews, proposals and approvals go through the API with the
// operator's own key, so the API records who acted; evidence capture reads the provider directly.
func ops(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(opsUsage)
	}
	flags := flag.NewFlagSet("ops", flag.ContinueOnError)
	as := flags.String("as", "", "operator id; the key comes from FX_OPERATOR_KEY_<ID>")
	payment := flags.String("payment", "", "payment id")
	decision := flags.String("decision", "", "decision")
	evidence := flags.String("evidence", "", "evidence id")
	digest := flags.String("digest", "", "evidence or proposal digest")
	proposal := flags.String("proposal", "", "proposal id")
	rest := args[1:]
	if args[0] == "evidence" && len(args) > 1 {
		rest = args[2:]
	}
	if err := flags.Parse(rest); err != nil {
		return err
	}
	switch args[0] {
	case "reviews":
		return listReviews(ctx, *as)
	case "evidence":
		if len(args) < 2 {
			return errors.New(opsUsage)
		}
		return captureEvidence(ctx, args[1], *payment)
	case "propose":
		return propose(ctx, *as, *payment, *decision, *evidence, *digest)
	case "approve":
		return approve(ctx, *as, *proposal, *digest)
	}
	return errors.New(opsUsage)
}

func opsClient(as string) (corridorv1connect.OpsServiceClient, error) {
	variable := operatorKeyVariable(as)
	key := os.Getenv(variable)
	if as == "" || key == "" {
		return nil, fmt.Errorf("--as needs an operator whose key is in %s", variable)
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	idempotencyKey := hex.EncodeToString(random)
	headers := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+key)
			req.Header().Set("Idempotency-Key", idempotencyKey)
			return next(ctx, req)
		}
	})
	return corridorv1connect.NewOpsServiceClient(&http.Client{Timeout: 30 * time.Second}, envOr("FX_INTERNAL_URL", "http://127.0.0.1:8081"),
		connect.WithInterceptors(headers)), nil
}

func listReviews(ctx context.Context, as string) error {
	client, err := opsClient(as)
	if err != nil {
		return err
	}
	resp, err := client.ListReviews(ctx, connect.NewRequest(&corridorv1.ListReviewsRequest{}))
	if err != nil {
		return err
	}
	for _, r := range resp.Msg.Reviews {
		fmt.Printf("%s %s revision=%d opened=%s proposal=%s\n", r.PaymentId, strings.TrimPrefix(r.Reason.String(), "REVIEW_REASON_"),
			r.Revision, r.OpenedAt.AsTime().Format(time.RFC3339), r.CurrentProposalId)
	}
	return nil
}

func parsePayment(payment string) (uuid.UUID, error) {
	id, err := uuid.Parse(payment)
	if err != nil {
		return uuid.Nil, fmt.Errorf("--payment %q is not a payment id", payment)
	}
	return id, nil
}

func loadSubject(ctx context.Context, env *environment, payment uuid.UUID) (store.ReviewSubject, error) {
	subject, err := env.store.ReviewSubject(ctx, payment)
	if errors.Is(err, store.ErrNotFound) {
		return subject, fmt.Errorf("payment %s not found", payment)
	}
	return subject, err
}

func parseAmount(value string) (int64, error) {
	amount, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("the provider answered the amount %q: %w", value, err)
	}
	return amount, nil
}

// captureEvidence stores a provider observation for a review: a read of the trade or the payout,
// or the payout's outcome line from a new statement. It prints the id and digest a proposal cites.
func captureEvidence(ctx context.Context, kind, payment string) error {
	paymentID, err := parsePayment(payment)
	if err != nil {
		return err
	}
	env, err := load(ctx)
	if err != nil {
		return err
	}
	defer env.pool.Close()
	secrets, err := required("FX_DITTO_URL", "FX_DITTO_KEY")
	if err != nil {
		return err
	}
	ditto := provider.NewDitto(&http.Client{Timeout: 10 * time.Second}, secrets["FX_DITTO_URL"], secrets["FX_DITTO_KEY"])
	s, err := loadSubject(ctx, env, paymentID)
	if err != nil {
		return err
	}
	payout := s.PayoutID.String()
	var (
		source, operation, reference, outcome, currency, rawAmount string
		payload                                                    any
	)
	switch {
	case kind == "read" && s.Reason == "FILL_OUT_OF_TOLERANCE":
		trade, err := ditto.Trade(ctx, payment)
		if err != nil {
			return err
		}
		source, operation, reference, outcome, currency, rawAmount, payload = "READ", "TRADE", payment, trade.Status, trade.BuyCurrency, trade.BuyAmount, trade
	case kind == "read":
		read, err := ditto.Payout(ctx, payout)
		if err != nil {
			return err
		}
		source, operation, reference, outcome, currency, rawAmount, payload = "READ", "PAYOUT", payout, read.Status, read.Currency, read.Amount, read
	case kind == "statement":
		line, err := statementOutcome(ctx, ditto, payout)
		if err != nil {
			return err
		}
		source, operation, reference, outcome, currency, rawAmount, payload = "STATEMENT", "PAYOUT", payout, line.Outcome.Status, line.Outcome.Currency, line.Outcome.Amount, line
	default:
		return errors.New(opsUsage)
	}
	amount, err := parseAmount(rawAmount)
	if err != nil {
		return err
	}
	id, digest, err := store.RecordEvidence(ctx, env.pool, store.Evidence{
		Source:    source,
		Operation: operation,
		Reference: reference,
		Outcome:   outcome,
		Currency:  currency,
		Amount:    amount,
		Payload:   payload,
	})
	if err != nil {
		return err
	}
	fmt.Printf("%s %s %s %s %s %d\n", id, digest, source, outcome, currency, amount)
	return nil
}

type statementLine struct {
	LineID  string `json:"line_id"`
	Kind    string `json:"kind"`
	Outcome struct {
		Operation string `json:"operation"`
		Reference string `json:"reference"`
		Status    string `json:"status"`
		Currency  string `json:"currency"`
		Amount    string `json:"amount"`
	} `json:"outcome"`
}

func statementOutcome(ctx context.Context, ditto *provider.Ditto, reference string) (statementLine, error) {
	statement, err := ditto.CreateStatement(ctx, "")
	if err != nil {
		return statementLine{}, err
	}
	cursor := ""
	for {
		page, err := ditto.StatementPage(ctx, statement.ID, cursor)
		if err != nil {
			return statementLine{}, err
		}
		for _, raw := range page.Lines {
			var line statementLine
			if err := json.Unmarshal(raw, &line); err != nil {
				return statementLine{}, err
			}
			if line.Kind == "OUTCOME" && line.Outcome.Operation == "PAYOUT" && line.Outcome.Reference == reference {
				return line, nil
			}
		}
		if page.NextCursor == "" {
			return statementLine{}, fmt.Errorf("statement %s has no outcome for payout %s", statement.ID, reference)
		}
		cursor = page.NextCursor
	}
}

// decisions are the ones an operator can propose from this console.
var decisions = []string{"ACCEPT_FILL", "PAID", "NOT_PAID"}

func propose(ctx context.Context, as, payment, decision, evidence, digest string) error {
	if !slices.Contains(decisions, decision) {
		return fmt.Errorf("--decision %q is not one of %s", decision, strings.Join(decisions, ", "))
	}
	paymentID, err := parsePayment(payment)
	if err != nil {
		return err
	}
	client, err := opsClient(as)
	if err != nil {
		return err
	}
	env, err := load(ctx)
	if err != nil {
		return err
	}
	defer env.pool.Close()
	s, err := loadSubject(ctx, env, paymentID)
	if err != nil {
		return err
	}
	req := &corridorv1.ProposeResolutionRequest{
		PaymentId:      payment,
		ReviewRevision: s.Revision,
		Decision:       corridorv1.Decision(corridorv1.Decision_value["DECISION_"+decision]),
		Evidence:       &corridorv1.Evidence{Id: evidence, Digest: digest},
	}
	switch decision {
	case "ACCEPT_FILL":
		evidenceID, err := uuid.Parse(evidence)
		if err != nil {
			return fmt.Errorf("--evidence %q is not an evidence id", evidence)
		}
		currency, delivered, err := env.store.EvidenceAmount(ctx, evidenceID)
		if err != nil {
			return fmt.Errorf("evidence %s: %w", evidence, err)
		}
		req.SubjectKind, req.SubjectId, req.ProviderReference = corridorv1.SubjectKind_SUBJECT_KIND_TRADE, payment, payment
		req.Amount = &corridorv1.Money{Currency: currency, Amount: strconv.FormatInt(delivered, 10)}
	default:
		payout := s.PayoutID.String()
		req.SubjectKind, req.SubjectId, req.ProviderReference = corridorv1.SubjectKind_SUBJECT_KIND_PAYOUT, payout, payout
		req.Amount = &corridorv1.Money{Currency: s.Currency, Amount: strconv.FormatInt(s.Amount, 10)}
	}
	resp, err := client.ProposeResolution(ctx, connect.NewRequest(req))
	if err != nil {
		return err
	}
	fmt.Printf("%s %s %s %s\n", strings.TrimPrefix(resp.Msg.Command.State.String(), "COMMAND_STATE_"), resp.Msg.Command.Reason, resp.Msg.ProposalId, resp.Msg.ProposalDigest)
	return nil
}

func approve(ctx context.Context, as, proposal, digest string) error {
	client, err := opsClient(as)
	if err != nil {
		return err
	}
	resp, err := client.ApproveResolution(ctx, connect.NewRequest(&corridorv1.ApproveResolutionRequest{ProposalId: proposal, ProposalDigest: digest}))
	if err != nil {
		return err
	}
	fmt.Printf("%s %s %s\n", strings.TrimPrefix(resp.Msg.Command.State.String(), "COMMAND_STATE_"), resp.Msg.Command.Reason, resp.Msg.Command.Id)
	return nil
}
