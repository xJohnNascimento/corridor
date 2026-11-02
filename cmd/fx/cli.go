package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/xJohnNascimento/corridor/internal/failpoint"
	"github.com/xJohnNascimento/corridor/internal/ledger"
	"github.com/xJohnNascimento/corridor/internal/store"
)

const cliUsage = `usage:
  fx cli customers upsert --id ID --name NAME   (the key comes from FX_CUSTOMER_KEY; generated when unset)
  fx cli payments list --customer ID
  fx cli payments timeline --payment ID
  fx cli failpoints arm --name NAME [--subject SUBJECT] [--uses N]
  fx cli failpoints release --name NAME
  fx cli failpoints held --name NAME
  fx cli failpoints remaining --name NAME
  fx cli ops ...  (fx cli ops for its usage)`

func cli(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "ops" {
		return ops(ctx, args[1:])
	}
	if len(args) < 2 {
		return errors.New(cliUsage)
	}
	switch args[0] + " " + args[1] {
	case "customers upsert":
		return upsertCustomer(ctx, args[2:])
	case "payments list":
		return listPayments(ctx, args[2:])
	case "payments timeline":
		return paymentTimeline(ctx, args[2:])
	case "failpoints arm", "failpoints release", "failpoints held", "failpoints remaining":
		return failpointCommand(ctx, args[1], args[2:])
	}
	return errors.New(cliUsage)
}

// upsertCustomer registers a customer with a hashed key and makes sure the customer has a funds
// account in the ledger for every enabled currency. The key comes from FX_CUSTOMER_KEY, never from
// an argument that ps would show; a generated key is printed once.
func upsertCustomer(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("customers upsert", flag.ContinueOnError)
	id := flags.String("id", "", "customer id")
	name := flags.String("name", "", "customer name")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *id == "" || *name == "" {
		return errors.New(cliUsage)
	}
	env, err := load(ctx)
	if err != nil {
		return err
	}
	defer env.pool.Close()
	secrets, err := required("FX_LEDGER_URL", "FX_LEDGER_SECRET")
	if err != nil {
		return err
	}

	key := os.Getenv("FX_CUSTOMER_KEY")
	generated := key == ""
	if generated {
		random := make([]byte, 32)
		if _, err := rand.Read(random); err != nil {
			return err
		}
		key = hex.EncodeToString(random)
	}
	if err := env.store.UpsertCustomer(ctx, *id, *name, store.KeyHash(key)); err != nil {
		return err
	}
	svc := ledger.NewService(&http.Client{Timeout: 10 * time.Second}, secrets["FX_LEDGER_URL"], secrets["FX_LEDGER_SECRET"])
	for _, currency := range env.cfg.Currencies {
		account, err := ledger.EnsureAccount(ctx, svc, env.cfg.Ledger.ID, ledger.Customer(*id), "funds", currency)
		if err != nil {
			return fmt.Errorf("ensure %s funds account: %w", currency, err)
		}
		if err := env.store.SetCustomerAccount(ctx, *id, currency, account); err != nil {
			return err
		}
	}
	if generated {
		fmt.Println(key)
	}
	return nil
}

func listPayments(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("payments list", flag.ContinueOnError)
	customer := flags.String("customer", "", "customer id")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *customer == "" {
		return errors.New(cliUsage)
	}
	env, err := load(ctx)
	if err != nil {
		return err
	}
	defer env.pool.Close()
	payments, err := env.store.PaymentSummaries(ctx, *customer)
	if err != nil {
		return err
	}
	for _, p := range payments {
		fmt.Printf("%s %s %s %s %s %t\n", p.ID, p.Status, p.Outcome, p.Review, p.PayoutStatus, p.StartPending)
	}
	return nil
}

// paymentTimeline prints every event of a payment, including those only operators see.
func paymentTimeline(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("payments timeline", flag.ContinueOnError)
	payment := flags.String("payment", "", "payment id")
	if err := flags.Parse(args); err != nil {
		return err
	}
	id, err := uuid.Parse(*payment)
	if err != nil {
		return errors.New(cliUsage)
	}
	env, err := load(ctx)
	if err != nil {
		return err
	}
	defer env.pool.Close()
	events, err := env.store.Timeline(ctx, id, true)
	if err != nil {
		return err
	}
	for _, e := range events {
		fmt.Printf("%s  %-24s %s\n", e.OccurredAt.Format(time.RFC3339Nano), e.Kind, e.Detail)
	}
	return nil
}

func failpointCommand(ctx context.Context, action string, args []string) error {
	flags := flag.NewFlagSet("failpoints", flag.ContinueOnError)
	name := flags.String("name", "", "failpoint name")
	subject := flags.String("subject", "", "payment or customer id; empty matches any")
	uses := flags.Int("uses", 1, "how many times it fires")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New(cliUsage)
	}
	if !isDemo() {
		return errors.New("failpoints exist only in the demo profile")
	}
	env, err := load(ctx)
	if err != nil {
		return err
	}
	defer env.pool.Close()
	points := failpoint.New(env.pool)
	switch action {
	case "arm":
		return points.Arm(ctx, *name, *subject, *uses)
	case "release":
		return points.Release(ctx, *name)
	}
	if action == "remaining" {
		remaining, err := points.Remaining(ctx, *name)
		if err == nil {
			fmt.Println(remaining)
		}
		return err
	}
	held, err := points.Held(ctx, *name)
	if err == nil {
		fmt.Println(held)
	}
	return err
}
