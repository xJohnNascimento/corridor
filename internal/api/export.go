package api

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"log/slog"
	"math"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	corridorv1 "github.com/xJohnNascimento/corridor/api/corridor/v1"
	"github.com/xJohnNascimento/corridor/internal/money"
	"github.com/xJohnNascimento/corridor/internal/store"
)

const exportSchemaVersion = 1

type exporter struct {
	stream *connect.ServerStream[corridorv1.ExportSnapshotResponse]
	hash   hash.Hash
	counts map[string]int64
}

// ExportSnapshot streams payments, trades, evidence, ledger intents, commands and proposals from
// one read-only snapshot, framed like the ledger's export: a trailer with counts and a SHA-256
// over every record, so the verifier can prove it received everything.
func (s opsService) ExportSnapshot(ctx context.Context, _ *connect.Request[corridorv1.ExportSnapshotRequest], stream *connect.ServerStream[corridorv1.ExportSnapshotResponse]) error {
	if actorFrom(ctx).Kind != ActorAuditor {
		return failure(corridorv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED, "the auditor key is required")
	}
	if s.audit == nil {
		return unavailable(ctx, errors.New("no audit store is configured"))
	}
	e := &exporter{stream: stream, hash: sha256.New(), counts: map[string]int64{}}
	status := corridorv1.SnapshotStatus_SNAPSHOT_STATUS_COMPLETE
	err := s.audit.Snapshot(ctx, func(tx pgx.Tx) error {
		var capturedAt time.Time
		if err := tx.QueryRow(ctx, "select now()").Scan(&capturedAt); err != nil {
			return err
		}
		header := &corridorv1.SnapshotHeader{SchemaVersion: exportSchemaVersion, SourceId: "corridor", DatasetId: s.datasetID, CapturedAt: timestamppb.New(capturedAt)}
		if err := e.emit("", &corridorv1.ExportSnapshotResponse{Record: &corridorv1.ExportSnapshotResponse_Header{Header: header}}); err != nil {
			return err
		}
		for _, step := range []func(context.Context, pgx.Tx) error{e.payments, e.trades, e.evidence, e.intents, e.commands, e.proposals} {
			if err := step(ctx, tx); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		slog.ErrorContext(ctx, "export snapshot", "error", err)
		status = corridorv1.SnapshotStatus_SNAPSHOT_STATUS_INCOMPLETE
	}
	return stream.Send(&corridorv1.ExportSnapshotResponse{Record: &corridorv1.ExportSnapshotResponse_Trailer{Trailer: &corridorv1.SnapshotTrailer{
		Status: status, Counts: e.counts, ContentHash: hex.EncodeToString(e.hash.Sum(nil)),
	}}})
}

func (e *exporter) emit(kind string, record *corridorv1.ExportSnapshotResponse) error {
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
	e.hash.Write(length[:])
	e.hash.Write(encoded)
	if kind != "" {
		e.counts[kind]++
	}
	return e.stream.Send(record)
}

func each(ctx context.Context, tx pgx.Tx, query string, scan func(pgx.Rows) error) error {
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (e *exporter) payments(ctx context.Context, tx pgx.Tx) error {
	return each(ctx, tx, `
		select p.id, p.status, coalesce(p.outcome_reason, ''), p.review_open, coalesce(p.review_reason, ''), p.quote_id,
		       q.sell_currency, q.sell_amount, q.fee, q.buy_currency, q.buy, q.customer_rate_e8, p.beneficiary_id,
		       o.id, o.status, o.amount, o.currency, coalesce(o.provider_payout_id, ''), coalesce(o.returned_amount, 0), o.submission_started_at, p.accepted_at, p.updated_at
		from fx.payment p join fx.payout o on o.payment_id = p.id join fx.quote q on q.id = p.quote_id
		order by p.id`,
		func(rows pgx.Rows) error {
			var p store.Payment
			err := rows.Scan(&p.ID, &p.Status, &p.OutcomeReason, &p.ReviewOpen, &p.ReviewReason, &p.QuoteID,
				&p.Quote.SellCurrency, &p.Quote.SellAmount, &p.Quote.Fee, &p.Quote.BuyCurrency, &p.Quote.Buy, &p.Quote.CustomerRateE8, &p.BeneficiaryID,
				&p.Payout.ID, &p.Payout.Status, &p.Payout.Amount, &p.Payout.Currency, &p.Payout.ProviderPayoutID, &p.Payout.ReturnedAmount,
				&p.Payout.SubmissionStartedAt, &p.AcceptedAt, &p.UpdatedAt)
			if err != nil {
				return err
			}
			message := paymentMessage(p)
			if p.Payout.SubmissionStartedAt != nil {
				message.Payout.SubmissionStartedAt = timestamppb.New(*p.Payout.SubmissionStartedAt)
			}
			return e.emit("payment", &corridorv1.ExportSnapshotResponse{Record: &corridorv1.ExportSnapshotResponse_Payment{Payment: message}})
		})
}

func (e *exporter) trades(ctx context.Context, tx pgx.Tx) error {
	return each(ctx, tx, `
		select payment_id::text, reference, status, sell_currency, sell_amount, buy_currency, coalesce(delivered_amount, 0),
		       coalesce(provider_trade_id, ''), coalesce(reservation_id::text, ''), coalesce(entry_id::text, '')
		from fx.trade order by payment_id`,
		func(rows pgx.Rows) error {
			var (
				t               corridorv1.TradeRecord
				sell, buy       string
				sold, delivered int64
			)
			if err := rows.Scan(&t.PaymentId, &t.ProviderReference, &t.Status, &sell, &sold, &buy, &delivered, &t.ProviderTradeId, &t.ReservationId, &t.EntryId); err != nil {
				return err
			}
			t.Id = t.PaymentId
			t.Sold = &corridorv1.Money{Currency: sell, Amount: money.Format(sold)}
			t.Delivered = &corridorv1.Money{Currency: buy, Amount: money.Format(delivered)}
			return e.emit("trade", &corridorv1.ExportSnapshotResponse{Record: &corridorv1.ExportSnapshotResponse_Trade{Trade: &t}})
		})
}

func (e *exporter) evidence(ctx context.Context, tx pgx.Tx) error {
	return each(ctx, tx, `
		select id::text, source, reference, outcome, coalesce(currency, ''), coalesce(amount, 0), coalesce(sequence, 0), encode(digest, 'hex'), operation
		from fx.provider_evidence order by id`,
		func(rows pgx.Rows) error {
			var (
				r        corridorv1.EvidenceRecord
				currency string
				amount   int64
			)
			if err := rows.Scan(&r.Id, &r.SourceKind, &r.ProviderReference, &r.Outcome, &currency, &amount, &r.Sequence, &r.Digest, &r.Operation); err != nil {
				return err
			}
			r.Amount = &corridorv1.Money{Currency: currency, Amount: money.Format(amount)}
			return e.emit("evidence", &corridorv1.ExportSnapshotResponse{Record: &corridorv1.ExportSnapshotResponse_Evidence{Evidence: &r}})
		})
}

func (e *exporter) intents(ctx context.Context, tx pgx.Tx) error {
	return each(ctx, tx, `
		select namespace, reference, operation, payment_id::text, encode(request_hash, 'hex'), coalesce(observed::text, '')
		from fx.ledger_intent order by namespace, reference`,
		func(rows pgx.Rows) error {
			var r corridorv1.LedgerIntentRecord
			if err := rows.Scan(&r.Namespace, &r.Reference, &r.Operation, &r.PaymentId, &r.RequestHash, &r.ObservedResult); err != nil {
				return err
			}
			return e.emit("ledger_intent", &corridorv1.ExportSnapshotResponse{Record: &corridorv1.ExportSnapshotResponse_LedgerIntent{LedgerIntent: &r}})
		})
}

func (e *exporter) commands(ctx context.Context, tx pgx.Tx) error {
	return each(ctx, tx, `
		select id::text, operation, coalesce(subject_id, ''), state, coalesce(reason, ''), effect_ids, received_at, accepted_at, applied_at
		from fx.product_command order by id`,
		func(rows pgx.Rows) error {
			var (
				c                 corridorv1.Command
				operation, state  string
				received          time.Time
				accepted, applied *time.Time
			)
			if err := rows.Scan(&c.Id, &operation, &c.SubjectId, &state, &c.Reason, &c.EffectIds, &received, &accepted, &applied); err != nil {
				return err
			}
			c.Operation = corridorv1.CommandOperation(corridorv1.CommandOperation_value["COMMAND_OPERATION_"+operation])
			c.State = corridorv1.CommandState(corridorv1.CommandState_value["COMMAND_STATE_"+state])
			c.ReceivedAt = timestamppb.New(received)
			if accepted != nil {
				c.AcceptedAt = timestamppb.New(*accepted)
			}
			if applied != nil {
				c.AppliedAt = timestamppb.New(*applied)
			}
			return e.emit("command", &corridorv1.ExportSnapshotResponse{Record: &corridorv1.ExportSnapshotResponse_Command{Command: &c}})
		})
}

func (e *exporter) proposals(ctx context.Context, tx pgx.Tx) error {
	return each(ctx, tx, `
		select id::text, payment_id::text, subject_kind, subject_id, review_reason, review_revision, decision, provider_reference,
		       coalesce(currency, ''), coalesce(amount, 0), coalesce(evidence_id::text, ''), coalesce(encode(evidence_digest, 'hex'), ''),
		       maker, proposal_revision, encode(digest, 'hex'), coalesce(approver, ''), coalesce(approval_command_id::text, '')
		from fx.resolution_proposal order by id`,
		func(rows pgx.Rows) error {
			var r corridorv1.ProposalRecord
			if err := rows.Scan(&r.Id, &r.PaymentId, &r.SubjectKind, &r.SubjectId, &r.ReviewReason, &r.ReviewRevision, &r.Decision, &r.ProviderReference,
				&r.Currency, &r.Amount, &r.EvidenceId, &r.EvidenceDigest, &r.Maker, &r.ProposalRevision, &r.Digest, &r.Approver, &r.ApprovalCommandId); err != nil {
				return err
			}
			return e.emit("proposal", &corridorv1.ExportSnapshotResponse{Record: &corridorv1.ExportSnapshotResponse_Proposal{Proposal: &r}})
		})
}
