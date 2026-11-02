package main

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"
)

type scenario struct {
	id    string
	title string
	run   func(ctx context.Context, s *stack) error
}

// catalogue lists the scenarios in the order they run and print.
var catalogue = []scenario{
	{"S01", "Happy path, once per enabled corridor", happyPath},
	{"S02", "Insufficient customer funds", insufficientFunds},
	{"S03", "Trade rejected", tradeRejected},
	{"S04", "Payout failed by the rail", payoutFailed},
	{"S05", "Cancel in the payout window", cancelInWindow},
	{"S06", "Same key, concurrent and changed", sameKey},
	{"S07", "API dies after admission", apiDiesAfterAdmission},
	{"S08", "Ledger commits, reply lost", ledgerReplyLost},
	{"S09", "Trade fills, reply lost, worker dies", tradeReplyLost},
	{"S10", "Payout accepted, reply lost, worker dies", payoutReplyLost},
	{"S11", "Duplicate, reordered, omitted and false hints", hints},
	{"S12", "Cancel races submission", cancelRacesSubmission},
	{"S13", "Late create races the fence", lateCreate},
	{"S14", "Provider liquidity wait and contention", liquidityWait},
	{"S15", "Cancel during the liquidity wait, reserve reply lost", cancelDuringLiquidityWait},
	{"S16", "Fill differences within tolerance", fillDifferences},
	{"S17", "Fill beyond tolerance", fillBeyondTolerance},
	{"S18", "Status API lags the statement", statusLag},
	{"S19", "Payout stuck but cancellable", stuckPayout},
	{"S20", "Wrong evidence, same person, superseded proposal", brokenApprovals},
	{"S21", "Dispatcher dies after an accepted decision", decisionSurvivesACrash},
	{"S24", "Checker catches bad data", checkerCatchesBadData},
	{"S25", "Incompatible workflow change", incompatibleWorkflowChange},
}

func find(id string) (scenario, bool) {
	i := slices.IndexFunc(catalogue, func(sc scenario) bool { return sc.id == id })
	if i < 0 {
		return scenario{}, false
	}
	return catalogue[i], true
}

// selectScenarios returns the scenarios named by a comma-separated list, in catalogue order, or all
// of them for an empty list. An id that does not exist is an error: running nothing and exiting 0
// would read as a pass.
func selectScenarios(only string) ([]scenario, error) {
	if strings.TrimSpace(only) == "" {
		return catalogue, nil
	}
	wanted := map[string]bool{}
	var unknown []string
	for id := range strings.SplitSeq(only, ",") {
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		if _, ok := find(id); !ok {
			unknown = append(unknown, id)
		}
		wanted[id] = true
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown scenario ids %s; run with --list", strings.Join(unknown, ", "))
	}
	return slices.DeleteFunc(slices.Clone(catalogue), func(sc scenario) bool { return !wanted[sc.id] }), nil
}

func listScenarios() {
	for _, sc := range catalogue {
		fmt.Printf("%s  %s\n", sc.id, sc.title)
	}
}

// runScenarios runs scenarios one at a time, for readability; each creates its own customers, so
// no two share a balance.
func runScenarios(ctx context.Context, s *stack, selected []scenario, verify bool) error {
	failed := 0
	for _, sc := range selected {
		started := time.Now()
		err := sc.run(ctx, s)
		if err == nil && verify {
			err = verifyStack(ctx)
		}
		result := "PASS"
		if err != nil {
			result, failed = "FAIL", failed+1
		}
		fmt.Printf("%s  %-4s %6.1fs  %s\n", sc.id, result, time.Since(started).Seconds(), sc.title)
		if err != nil {
			fmt.Printf("      %v\n", err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d scenarios failed", failed)
	}
	return nil
}

// verifyStack runs V1 to V11 against the live stack after a scenario.
func verifyStack(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "go", "run", "./cmd/verify")
	cmd.Dir = "../verification"
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("verification after the scenario: %w\n%s", err, out)
	}
	return nil
}
