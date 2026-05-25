package apt

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
)

// txEnvironment composes a fresh in-memory FS + a runner that never
// actually exec's anything (DryRun is per-spec; tests use DryRun=true
// to short-circuit). Used by all transaction tests.
func txEnvironment(t *testing.T) (*Transaction, *memFS) {
	t.Helper()
	mfs := newMemFS()
	tx := &Transaction{
		Env:       &Env{FS: mfs},
		Runner:    &runner.Runner{LogDir: "-"},
		AssumeYes: true,
		DryRun:    true, // never invoke real apt during tests
	}
	return tx, mfs
}

// TestTransaction_InstallsAndRestoresPolicyRcD covers the happy path:
// no existing /usr/sbin/policy-rc.d, the transaction creates ours,
// runs the callback, and removes ours on completion.
func TestTransaction_InstallsAndRestoresPolicyRcD(t *testing.T) {
	tx, mfs := txEnvironment(t)
	called := false
	err := tx.Run(context.Background(), func(tc *TxContext) error {
		called = true
		// While the callback runs, ours is in place.
		if !mfs.has(PolicyRcDPath) {
			t.Errorf("policy-rc.d should be installed during callback")
		}
		body, _ := mfs.ReadFile(PolicyRcDPath)
		if !strings.Contains(string(body), clouddeployMarker) {
			t.Errorf("installed body should be CloudDeploy-owned, got: %s", body)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Transaction.Run: %v", err)
	}
	if !called {
		t.Fatalf("callback was not invoked")
	}
	if mfs.has(PolicyRcDPath) {
		t.Errorf("policy-rc.d should be cleaned up after Run returns")
	}
}

// TestTransaction_PreservesExistingPolicyRcD covers the operator's
// pre-existing /usr/sbin/policy-rc.d case: we back it up, run, and
// restore the original on completion.
func TestTransaction_PreservesExistingPolicyRcD(t *testing.T) {
	tx, mfs := txEnvironment(t)
	original := []byte("#!/bin/sh\n# OPERATOR'S OWN; DO NOT TOUCH\nexit 0\n")
	if err := mfs.WriteFile(PolicyRcDPath, original, 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := tx.Run(context.Background(), func(tc *TxContext) error {
		// During callback the original should be at the backup path.
		bak, err := mfs.ReadFile(policyRcDBackup)
		if err != nil {
			t.Errorf("backup not present during callback: %v", err)
		}
		if string(bak) != string(original) {
			t.Errorf("backup mismatch")
		}
		return nil
	}); err != nil {
		t.Fatalf("Transaction.Run: %v", err)
	}
	// After return, original is back.
	got, err := mfs.ReadFile(PolicyRcDPath)
	if err != nil {
		t.Fatalf("restored policy-rc.d missing: %v", err)
	}
	if string(got) != string(original) {
		t.Errorf("original not restored")
	}
	if _, err := mfs.Stat(policyRcDBackup); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("backup should be gone after Run")
	}
}

// TestTransaction_CallbackErrorPropagates: when the callback errors,
// Run should propagate AND still restore policy-rc.d.
func TestTransaction_CallbackErrorPropagates(t *testing.T) {
	tx, mfs := txEnvironment(t)
	sentinel := errors.New("callback exploded")
	err := tx.Run(context.Background(), func(tc *TxContext) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("expected callback error to propagate, got: %v", err)
	}
	if mfs.has(PolicyRcDPath) {
		t.Errorf("policy-rc.d should be restored even on callback error")
	}
}

// TestTransaction_DpkgRepairCallsConfigureAll - the real dpkg --audit
// command is gated behind the runner; we can't easily simulate
// "audit reports errors" without a fake runner. This test asserts the
// no-op happy path: when audit returns empty (the runner is dry-run,
// so Stdout is empty), RepairIfNeeded does nothing and returns nil.
func TestTxContext_RepairIfNeeded_NoOp(t *testing.T) {
	tx, _ := txEnvironment(t)
	if err := tx.Run(context.Background(), func(tc *TxContext) error {
		return tc.RepairIfNeeded(context.Background())
	}); err != nil {
		t.Errorf("RepairIfNeeded should be a no-op in DryRun: %v", err)
	}
}

func TestFixBrokenArgvUsesNoninteractiveOverwriteOptions(t *testing.T) {
	got := strings.Join(fixBrokenArgv([]string{"-y"}), " ")
	for _, want := range []string{
		"apt-get -y",
		"Dpkg::Options::=--force-confdef",
		"Dpkg::Options::=--force-confold",
		"Dpkg::Options::=--force-overwrite",
		"-f install",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("fix-broken argv missing %q: %s", want, got)
		}
	}
}

// TestTxContext_InstallEmpty - Install([]) is a no-op.
func TestTxContext_InstallEmpty(t *testing.T) {
	tx, _ := txEnvironment(t)
	if err := tx.Run(context.Background(), func(tc *TxContext) error {
		return tc.Install(context.Background(), nil)
	}); err != nil {
		t.Errorf("Install(nil) should be a no-op: %v", err)
	}
}

func TestInspectPolicyRcD_NoFile(t *testing.T) {
	mfs := newMemFS()
	env := &Env{FS: mfs}
	st, err := InspectPolicyRcD(env)
	if err != nil {
		t.Fatalf("InspectPolicyRcD: %v", err)
	}
	if st.Exists {
		t.Errorf("expected Exists=false on empty FS")
	}
	if st.IsClouddeploy {
		t.Errorf("expected IsClouddeploy=false")
	}
}

func TestInspectPolicyRcD_OurOwn(t *testing.T) {
	mfs := newMemFS()
	_ = mfs.WriteFile(PolicyRcDPath, []byte(policyRcDBody), 0o755)
	env := &Env{FS: mfs}
	st, err := InspectPolicyRcD(env)
	if err != nil {
		t.Fatalf("InspectPolicyRcD: %v", err)
	}
	if !st.Exists {
		t.Errorf("expected Exists=true")
	}
	if !st.IsClouddeploy {
		t.Errorf("expected IsClouddeploy=true (we wrote our body)")
	}
}

func TestInspectPolicyRcD_OperatorOwned(t *testing.T) {
	mfs := newMemFS()
	_ = mfs.WriteFile(PolicyRcDPath, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	env := &Env{FS: mfs}
	st, err := InspectPolicyRcD(env)
	if err != nil {
		t.Fatalf("InspectPolicyRcD: %v", err)
	}
	if !st.Exists {
		t.Errorf("expected Exists=true")
	}
	if st.IsClouddeploy {
		t.Errorf("operator-owned policy-rc.d should NOT be classified as CloudDeploy")
	}
}

func TestTailLines(t *testing.T) {
	got := tailLines("a\nb\nc\nd\ne\n", 3)
	if got != "c\nd\ne" {
		t.Errorf("got %q want %q", got, "c\nd\ne")
	}
	if tailLines("", 3) != "" {
		t.Errorf("empty should stay empty")
	}
	if tailLines("only\n", 3) != "only" {
		t.Errorf("short input should pass through")
	}
}

// memFS already has has() defined via test_helpers; expose it here
// for the transaction tests too.
func (m *memFS) has(path string) bool {
	_, ok := m.files[path]
	return ok
}
