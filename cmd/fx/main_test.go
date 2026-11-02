package main

import (
	"slices"
	"testing"
)

func TestOperatorsComeFromTheirListAndKeys(t *testing.T) {
	t.Setenv("FX_OPERATORS", " Alice, bob ,alice,")
	t.Setenv("FX_OPERATOR_KEY_ALICE", "alice-key")
	t.Setenv("FX_OPERATOR_KEY_BOB", "bob-key")
	got, err := operatorsFromEnv()
	want := []operator{{"alice", "alice-key"}, {"bob", "bob-key"}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("operators %v, %v; want %v", got, err, want)
	}
}

func TestAnOperatorWithoutAKeyRefusesToStart(t *testing.T) {
	t.Setenv("FX_OPERATORS", "alice,bob")
	t.Setenv("FX_OPERATOR_KEY_ALICE", "alice-key")
	t.Setenv("FX_OPERATOR_KEY_BOB", "")
	if got, err := operatorsFromEnv(); err == nil {
		t.Fatalf("operators %v without bob's key", got)
	}
}

func TestNoOperatorsRefusesToStart(t *testing.T) {
	t.Setenv("FX_OPERATORS", " , ")
	if got, err := operatorsFromEnv(); err == nil {
		t.Fatalf("operators %v from an empty list", got)
	}
}

func TestOnlyTheLocalEnvironmentAcceptsTheDemoProfile(t *testing.T) {
	t.Setenv("FX_CONFIG_FILE", "../../deploy/config.yaml")
	t.Setenv("FX_PROFILE", "demo")
	t.Setenv("FX_DATABASE_URL", "")
	for _, environment := range []string{"", "production", "sandbox"} {
		t.Setenv("FX_ENVIRONMENT", environment)
		if _, err := load(t.Context()); err == nil || err.Error() != "the demo profile runs only with FX_ENVIRONMENT=local" {
			t.Errorf("FX_ENVIRONMENT=%q: %v, want the demo profile refused", environment, err)
		}
	}
	t.Setenv("FX_ENVIRONMENT", "local")
	if _, err := load(t.Context()); err == nil || err.Error() != "FX_DATABASE_URL is required" {
		t.Errorf("the local environment: %v, want it to pass the guard and stop at the missing database", err)
	}
}
