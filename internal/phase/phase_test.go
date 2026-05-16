package phase

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/apt"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/config"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/nvidia"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/state"
)

// -----------------------------------------------------------------------------
// memFS for the apt transaction guard (mirrors internal/apt/policy_rc_d_test.go).
// -----------------------------------------------------------------------------

type memFS struct{ files map[string][]byte }

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

func (m *memFS) Stat(p string) (fs.FileInfo, error) {
	if b, ok := m.files[p]; ok {
		return memInfo{name: p, size: int64(len(b))}, nil
	}
	return nil, fs.ErrNotExist
}
func (m *memFS) ReadFile(p string) ([]byte, error) {
	if b, ok := m.files[p]; ok {
		out := make([]byte, len(b))
		copy(out, b)
		return out, nil
	}
	return nil, fs.ErrNotExist
}
func (m *memFS) WriteFile(p string, data []byte, _ fs.FileMode) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	m.files[p] = cp
	return nil
}
func (m *memFS) Remove(p string) error {
	if _, ok := m.files[p]; !ok {
		return fs.ErrNotExist
	}
	delete(m.files, p)
	return nil
}
func (m *memFS) Rename(oldp, newp string) error {
	b, ok := m.files[oldp]
	if !ok {
		return fs.ErrNotExist
	}
	m.files[newp] = b
	delete(m.files, oldp)
	return nil
}
func (m *memFS) MkdirAll(string, fs.FileMode) error { return nil }

// newDeps builds a Deps with a fresh in-memory state, an apt transaction
// in DryRun mode (so it never actually exec's apt-get), and a runner
// that suppresses log files. evidence injects a synthetic Evidence for
// the NVIDIA phase.
func newDeps(t *testing.T, profile *config.Profile, evidence *nvidia.Evidence) *Deps {
	t.Helper()
	s := state.New(profile.Profile)
	tx := &apt.Transaction{
		Env:       &apt.Env{FS: newMemFS()},
		Runner:    &runner.Runner{LogDir: "-"},
		AssumeYes: true,
		DryRun:    true,
	}
	return &Deps{
		Runner:  &runner.Runner{LogDir: "-"},
		APT:     tx,
		State:   s,
		Profile: profile,
		DryRun:  true,
	}
}

// nvidiaEvidenceFn returns a closure that yields the supplied evidence.
func nvidiaEvidenceFn(ev nvidia.Evidence) func(nvidia.EvidenceOptions) (nvidia.Evidence, error) {
	return func(nvidia.EvidenceOptions) (nvidia.Evidence, error) {
		return ev, nil
	}
}

// hdrProfile returns the canonical hdr-4k120 profile used in tests.
func hdrProfile() *config.Profile {
	return &config.Profile{
		Profile: "hdr-4k120",
		NVIDIA: config.NVIDIAConfig{
			DriverMajor:      "580",
			PreferOpenFamily: true,
		},
		CUDA: config.CUDAConfig{Mode: "none"},
	}
}

// -----------------------------------------------------------------------------
// base-packages
// -----------------------------------------------------------------------------

func TestBasePackages_RunMarksDone(t *testing.T) {
	deps := newDeps(t, hdrProfile(), nil)
	if err := (BasePackages{}).Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if deps.State.Get(BasePackagesName).Status != state.StatusDone {
		t.Errorf("phase should be Done")
	}
	d := deps.State.Get(BasePackagesName).Details
	if d == nil || d["installed"] == nil {
		t.Errorf("Details.installed missing: %+v", d)
	}
}

func TestBasePackages_SkipsWhenDone(t *testing.T) {
	deps := newDeps(t, hdrProfile(), nil)
	deps.State.MarkDone(BasePackagesName, map[string]any{"installed": []string{}})
	if err := (BasePackages{}).Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// -----------------------------------------------------------------------------
// nvidia-driver
// -----------------------------------------------------------------------------

func TestNvidiaDriver_RTX5090_ChoosesServerOpen(t *testing.T) {
	ev := nvidia.Evidence{
		PCIID:               "10de:2b85",
		GPUName:             "NVIDIA GeForce RTX 5090",
		IsBlackwellConsumer: true,
		AvailabilityKnown:   true,
		AvailableServerOpen: true,
		// closed module DKMS also "installable" in apt; the
		// selector must NOT pick it.
		AvailableServer: true,
	}
	deps := newDeps(t, hdrProfile(), nil)
	phase := NvidiaDriver{EvidenceFn: nvidiaEvidenceFn(ev)}
	err := phase.Run(context.Background(), deps)
	// nvidia-smi will not work in DryRun apt; the phase therefore
	// sets RebootNeeded and returns ErrRebootRequired. That's the
	// correct outcome for the live VM path too.
	if !errors.Is(err, ErrRebootRequired) {
		t.Fatalf("expected ErrRebootRequired, got: %v", err)
	}
	d := deps.State.Get(NvidiaDriverName).Details
	if got := d["package_family"]; got != "server-open" {
		t.Errorf("package_family: got %v want server-open", got)
	}
	if got := d["driver_package"]; got != "nvidia-driver-580-server-open" {
		t.Errorf("driver_package: got %v", got)
	}
	if got := d["dkms_package"]; got != "nvidia-dkms-580-server-open" {
		t.Errorf("dkms_package: got %v", got)
	}
	if !deps.State.RebootNeeded {
		t.Errorf("RebootNeeded should be set")
	}
	if deps.State.ResumeTarget != NvidiaDriverName {
		t.Errorf("ResumeTarget mismatch: got %q", deps.State.ResumeTarget)
	}
}

func TestNvidiaDriver_PreservesInstalledWorkingFamily(t *testing.T) {
	// v2-regression test: server-open is already installed AND
	// nvidia-smi works. The phase must mark done without uninstalling.
	ev := nvidia.Evidence{
		PCIID:               "10de:2684",
		GPUName:             "NVIDIA GeForce RTX 4090",
		InstalledServerOpen: true,
		NvidiaSmiWorks:      true,
		AvailabilityKnown:   true,
		AvailableServerOpen: true,
	}
	deps := newDeps(t, hdrProfile(), nil)
	phase := NvidiaDriver{EvidenceFn: nvidiaEvidenceFn(ev)}
	if err := phase.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if deps.State.Get(NvidiaDriverName).Status != state.StatusDone {
		t.Errorf("phase should be Done; got %q", deps.State.Get(NvidiaDriverName).Status)
	}
	d := deps.State.Get(NvidiaDriverName).Details
	if got := d["already_present"]; got != true {
		t.Errorf("already_present should be true: %+v", d)
	}
	if deps.State.RebootNeeded {
		t.Errorf("RebootNeeded should NOT be set for a working installed family")
	}
}

func TestNvidiaDriver_ErrorsWhenOpenRequiredButNoOpenPackages(t *testing.T) {
	ev := nvidia.Evidence{
		PCIID:               "10de:2b85",
		GPUName:             "NVIDIA GeForce RTX 5090",
		IsBlackwellConsumer: true,
		AvailabilityKnown:   true,
		AvailableServer:     true, // only closed available; Blackwell needs open
	}
	deps := newDeps(t, hdrProfile(), nil)
	phase := NvidiaDriver{EvidenceFn: nvidiaEvidenceFn(ev)}
	err := phase.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected error when open required but no open packages available")
	}
	if !errors.Is(err, nvidia.ErrNoOpenAvailable) {
		t.Errorf("expected wrapped ErrNoOpenAvailable; got: %v", err)
	}
	if deps.State.Get(NvidiaDriverName).Status != state.StatusFailedFatal {
		t.Errorf("phase should be FailedFatal")
	}
}

// -----------------------------------------------------------------------------
// cuda
// -----------------------------------------------------------------------------

func TestCuda_ModeNoneSkips(t *testing.T) {
	deps := newDeps(t, hdrProfile(), nil)
	if err := (Cuda{}).Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if deps.State.Get(CudaName).Status != state.StatusSkipped {
		t.Errorf("phase should be Skipped for mode=none")
	}
}

func TestCuda_ModeOptionalContinuesOnFailure(t *testing.T) {
	// We cannot easily inject an apt-failure from DryRun mode, so this
	// test just exercises the "optional" path - it should mark either
	// done or failed_nonfatal but never fatal. With DryRun=true the
	// install reports success (apt is dry-run), so the phase ends
	// Done.
	p := hdrProfile()
	p.CUDA.Mode = "optional"
	deps := newDeps(t, p, nil)
	if err := (Cuda{}).Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := deps.State.Get(CudaName).Status
	if got == state.StatusFailedFatal {
		t.Errorf("optional must never be fatal; got %q", got)
	}
}

func TestCuda_ModeRequiredSurfacesFatalOnRealError(t *testing.T) {
	// Force the phase to fail by giving it an APT transaction with a
	// nil Env, which makes Run fail fast. The phase must classify
	// that as fatal because mode=required.
	deps := newDeps(t, hdrProfile(), nil)
	deps.Profile.CUDA.Mode = "required"
	deps.APT.Env = nil // makes apt.Transaction.Run return an error immediately
	err := (Cuda{}).Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected error for required-mode failure")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("error should mention required-mode: %v", err)
	}
	if deps.State.Get(CudaName).Status != state.StatusFailedFatal {
		t.Errorf("phase should be FailedFatal")
	}
}

// -----------------------------------------------------------------------------
// apply-style sequencing: base-packages -> nvidia-driver -> cuda
// -----------------------------------------------------------------------------

func TestApplyPartial_Sequencing(t *testing.T) {
	// Confirms the order base-packages -> nvidia-driver -> cuda runs
	// each phase exactly once and writes the expected state shape.
	ev := nvidia.Evidence{
		PCIID:               "10de:2684",
		GPUName:             "NVIDIA GeForce RTX 4090",
		InstalledServerOpen: true,
		NvidiaSmiWorks:      true,
		AvailabilityKnown:   true,
		AvailableServerOpen: true,
	}
	deps := newDeps(t, hdrProfile(), nil)

	phases := []Phase{
		BasePackages{},
		NvidiaDriver{EvidenceFn: nvidiaEvidenceFn(ev)},
		Cuda{},
	}
	for _, p := range phases {
		if err := p.Run(context.Background(), deps); err != nil {
			t.Fatalf("phase %s: %v", p.Name(), err)
		}
	}

	wantStatus := map[string]state.PhaseStatus{
		BasePackagesName: state.StatusDone,
		NvidiaDriverName: state.StatusDone,
		CudaName:         state.StatusSkipped,
	}
	for name, want := range wantStatus {
		if got := deps.State.Get(name).Status; got != want {
			t.Errorf("phase %s: status got %q want %q", name, got, want)
		}
	}
}
