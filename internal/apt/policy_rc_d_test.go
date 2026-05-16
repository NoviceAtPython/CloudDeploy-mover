package apt

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"
)

// memFS is a tiny in-memory FS implementation that satisfies the FS
// interface. Enough surface area for the three policy-rc.d scenarios
// without bringing in afero.
type memFS struct {
	files map[string][]byte
}

func newMemFS() *memFS { return &memFS{files: map[string][]byte{}} }

type memInfo struct {
	name string
	size int64
}

func (m memInfo) Name() string       { return m.name }
func (m memInfo) Size() int64        { return m.size }
func (m memInfo) Mode() fs.FileMode  { return 0o755 }
func (m memInfo) ModTime() time.Time { return time.Time{} }
func (m memInfo) IsDir() bool        { return false }
func (m memInfo) Sys() any           { return nil }

func (m *memFS) Stat(path string) (fs.FileInfo, error) {
	if b, ok := m.files[path]; ok {
		return memInfo{name: path, size: int64(len(b))}, nil
	}
	return nil, fs.ErrNotExist
}
func (m *memFS) ReadFile(path string) ([]byte, error) {
	if b, ok := m.files[path]; ok {
		out := make([]byte, len(b))
		copy(out, b)
		return out, nil
	}
	return nil, fs.ErrNotExist
}
func (m *memFS) WriteFile(path string, data []byte, _ fs.FileMode) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	m.files[path] = cp
	return nil
}
func (m *memFS) Remove(path string) error {
	if _, ok := m.files[path]; !ok {
		return fs.ErrNotExist
	}
	delete(m.files, path)
	return nil
}
func (m *memFS) Rename(oldpath, newpath string) error {
	b, ok := m.files[oldpath]
	if !ok {
		return fs.ErrNotExist
	}
	m.files[newpath] = b
	delete(m.files, oldpath)
	return nil
}
func (m *memFS) MkdirAll(string, fs.FileMode) error { return nil }

func TestInstallPolicyRcD_NoExistingFile(t *testing.T) {
	mfs := newMemFS()
	env := &Env{FS: mfs}

	guard, err := InstallPolicyRcD(env)
	if err != nil {
		t.Fatalf("InstallPolicyRcD: %v", err)
	}
	if guard.Kind() != GuardCreated {
		t.Errorf("kind: got %v want GuardCreated", guard.Kind())
	}
	got, err := mfs.ReadFile(PolicyRcDPath)
	if err != nil {
		t.Fatalf("policy-rc.d not written: %v", err)
	}
	if !strings.Contains(string(got), clouddeployMarker) {
		t.Errorf("written body missing CloudDeploy marker: %s", got)
	}
	if !strings.Contains(string(got), "exit 101") {
		t.Errorf("written body must exit 101: %s", got)
	}
	// Restore -> file gone.
	if err := guard.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := mfs.Stat(PolicyRcDPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Restore should have removed policy-rc.d; stat err=%v", err)
	}
}

func TestInstallPolicyRcD_ExistingNonClouddeployFile(t *testing.T) {
	mfs := newMemFS()
	original := []byte("#!/bin/sh\n# operator's own policy-rc.d - DO NOT TOUCH\nexit 0\n")
	if err := mfs.WriteFile(PolicyRcDPath, original, 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	env := &Env{FS: mfs}

	guard, err := InstallPolicyRcD(env)
	if err != nil {
		t.Fatalf("InstallPolicyRcD: %v", err)
	}
	if guard.Kind() != GuardBackedUp {
		t.Errorf("kind: got %v want GuardBackedUp", guard.Kind())
	}
	// Backup file present with original contents.
	got, err := mfs.ReadFile(policyRcDBackup)
	if err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	if string(got) != string(original) {
		t.Errorf("backup mismatch:\n got: %s\nwant: %s", got, original)
	}
	// Our policy-rc.d is now in place.
	body, _ := mfs.ReadFile(PolicyRcDPath)
	if !strings.Contains(string(body), clouddeployMarker) {
		t.Errorf("our policy-rc.d should be installed: %s", body)
	}

	if err := guard.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	// After restore: original is back, backup gone, no CloudDeploy file.
	restored, err := mfs.ReadFile(PolicyRcDPath)
	if err != nil {
		t.Fatalf("restored policy-rc.d missing: %v", err)
	}
	if string(restored) != string(original) {
		t.Errorf("restored file mismatch:\n got: %s\nwant: %s", restored, original)
	}
	if _, err := mfs.Stat(policyRcDBackup); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("backup file should be gone after restore; stat err=%v", err)
	}
}

func TestInstallPolicyRcD_StaleClouddeployFile(t *testing.T) {
	mfs := newMemFS()
	// Simulate a previous CloudDeploy run that wrote our file and
	// then crashed before calling Restore. The body matches what we
	// would write today.
	if err := mfs.WriteFile(PolicyRcDPath, []byte(policyRcDBody), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	env := &Env{FS: mfs}

	guard, err := InstallPolicyRcD(env)
	if err != nil {
		t.Fatalf("InstallPolicyRcD: %v", err)
	}
	if guard.Kind() != GuardAdoptedStale {
		t.Errorf("kind: got %v want GuardAdoptedStale", guard.Kind())
	}
	// We did NOT create a backup (there was nothing to back up).
	if _, err := mfs.Stat(policyRcDBackup); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("no backup should exist for stale CloudDeploy file; stat err=%v", err)
	}

	if err := guard.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := mfs.Stat(PolicyRcDPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Restore should have removed the stale CloudDeploy file; stat err=%v", err)
	}
}

func TestRestoreIsTolerantOfDoubleCall(t *testing.T) {
	mfs := newMemFS()
	env := &Env{FS: mfs}

	guard, err := InstallPolicyRcD(env)
	if err != nil {
		t.Fatalf("InstallPolicyRcD: %v", err)
	}
	if err := guard.Restore(); err != nil {
		t.Fatalf("Restore (1): %v", err)
	}
	if err := guard.Restore(); err != nil {
		t.Errorf("Restore (2) should be a no-op, got: %v", err)
	}
}

func TestIsClouddeployBody(t *testing.T) {
	if !isClouddeployBody([]byte(policyRcDBody)) {
		t.Error("our own body should be detected as CloudDeploy-owned")
	}
	if isClouddeployBody([]byte("#!/bin/sh\nexit 0\n")) {
		t.Error("a non-CloudDeploy body should not be detected as ours")
	}
	if isClouddeployBody(nil) {
		t.Error("empty body should not be detected as ours")
	}
}
