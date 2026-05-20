package phase

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/config"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/state"
)

func desktopProfile() *config.Profile {
	return &config.Profile{
		Profile: "test",
		NVIDIA:  config.NVIDIAConfig{DriverMajor: "580"},
		CUDA:    config.CUDAConfig{Mode: "none"},
		Desktop: config.DesktopConfig{
			User:         "cloudgamer",
			EnableLinger: true,
			Groups:       []string{"video", "render", "input", "audio", "systemd-journal"},
			Shell:        "/bin/bash",
		},
	}
}

func headlessUserDeps(t *testing.T) *Deps {
	t.Helper()
	d := newDeps(t, desktopProfile(), nil)
	d.StatePath = filepath.Join(t.TempDir(), "state.json")
	d.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	d.DryRun = false
	return d
}

// -----------------------------------------------------------------------------
// happy path
// -----------------------------------------------------------------------------

func TestHeadlessUser_CreatesMissingUser(t *testing.T) {
	deps := headlessUserDeps(t)

	createCalls := 0
	groupCalls := [][]string{}
	lingerCalls := 0
	lookupCalls := 0
	chownCalls := 0

	created := false
	ph := HeadlessUser{
		LookupFn: func(_ context.Context, _ *Deps, name string) (UserInfo, error) {
			lookupCalls++
			if !created {
				return UserInfo{}, nil // doesn't exist yet
			}
			return UserInfo{
				UID: "1001", GID: "1001",
				Home: "/home/cloudgamer", Shell: "/bin/bash",
				Groups: []string{"cloudgamer"}, // primary group only at first
			}, nil
		},
		CreateUserFn: func(_ context.Context, _ *Deps, name, shell string) error {
			createCalls++
			if name != "cloudgamer" {
				t.Errorf("create: name=%q want cloudgamer", name)
			}
			if shell != "/bin/bash" {
				t.Errorf("create: shell=%q want /bin/bash", shell)
			}
			created = true
			return nil
		},
		AddGroupsFn: func(_ context.Context, _ *Deps, name string, groups []string) error {
			groupCalls = append(groupCalls, groups)
			return nil
		},
		EnableLingerFn: func(_ context.Context, _ *Deps, name string, enable bool) error {
			lingerCalls++
			if !enable {
				t.Errorf("linger: enable=false want true")
			}
			return nil
		},
		ChownHomeFn: func(_ context.Context, _ *Deps, name, gid, home string) error {
			chownCalls++
			return nil
		},
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := deps.State.Get(HeadlessUserName).Status; got != state.StatusDone {
		t.Errorf("status: got %q want done", got)
	}
	if createCalls != 1 {
		t.Errorf("CreateUserFn calls: got %d want 1", createCalls)
	}
	if lookupCalls < 2 {
		t.Errorf("LookupFn calls: got %d want >=2", lookupCalls)
	}
	if len(groupCalls) != 1 {
		t.Errorf("AddGroupsFn calls: got %d want 1 (the diff)", len(groupCalls))
	} else {
		got := groupCalls[0]
		want := []string{"video", "render", "input", "audio", "systemd-journal"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("AddGroupsFn groups: got %v want %v", got, want)
		}
	}
	if lingerCalls != 1 {
		t.Errorf("EnableLingerFn calls: got %d want 1", lingerCalls)
	}
	if chownCalls != 1 {
		t.Errorf("ChownHomeFn calls: got %d want 1", chownCalls)
	}
	d := deps.State.Get(HeadlessUserName).Details
	if d["user"] != "cloudgamer" {
		t.Errorf("user: got %v want cloudgamer", d["user"])
	}
	if d["uid"] != "1001" {
		t.Errorf("uid: got %v want 1001", d["uid"])
	}
	if d["created_or_existing"] != "created" {
		t.Errorf("created_or_existing: got %v want created", d["created_or_existing"])
	}
	if d["linger_enabled"] != true {
		t.Errorf("linger_enabled: got %v want true", d["linger_enabled"])
	}
}

// -----------------------------------------------------------------------------
// existing user
// -----------------------------------------------------------------------------

func TestHeadlessUser_ExistingUserDoesNotCreate_OnlyAddsMissingGroups(t *testing.T) {
	deps := headlessUserDeps(t)

	createCalls := 0
	groupCalls := [][]string{}
	ph := HeadlessUser{
		LookupFn: func(_ context.Context, _ *Deps, name string) (UserInfo, error) {
			// User already exists and is already in some target groups.
			return UserInfo{
				UID: "1002", GID: "1002",
				Home: "/home/cloudgamer", Shell: "/bin/bash",
				Groups: []string{"cloudgamer", "video", "audio"},
			}, nil
		},
		CreateUserFn: func(context.Context, *Deps, string, string) error {
			createCalls++
			return nil
		},
		AddGroupsFn: func(_ context.Context, _ *Deps, _ string, groups []string) error {
			groupCalls = append(groupCalls, groups)
			return nil
		},
		EnableLingerFn: func(context.Context, *Deps, string, bool) error { return nil },
		ChownHomeFn:    func(context.Context, *Deps, string, string, string) error { return nil },
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if createCalls != 0 {
		t.Errorf("CreateUserFn must not run when user exists; got %d calls", createCalls)
	}
	if len(groupCalls) != 1 {
		t.Fatalf("AddGroupsFn: got %d calls want 1", len(groupCalls))
	}
	// Want = [render, input, systemd-journal] (the missing ones).
	want := []string{"render", "input", "systemd-journal"}
	if !reflect.DeepEqual(groupCalls[0], want) {
		t.Errorf("AddGroupsFn groups: got %v want %v", groupCalls[0], want)
	}
	d := deps.State.Get(HeadlessUserName).Details
	if d["created_or_existing"] != "existing" {
		t.Errorf("created_or_existing: got %v want existing", d["created_or_existing"])
	}
}

// -----------------------------------------------------------------------------
// linger off
// -----------------------------------------------------------------------------

func TestHeadlessUser_LingerDisabled_DoesNotCallLoginctl(t *testing.T) {
	deps := headlessUserDeps(t)
	deps.Profile.Desktop.EnableLinger = false

	lingerCalls := 0
	ph := HeadlessUser{
		LookupFn: func(context.Context, *Deps, string) (UserInfo, error) {
			return UserInfo{
				UID: "1001", GID: "1001",
				Home: "/home/cloudgamer", Shell: "/bin/bash",
				Groups: []string{"cloudgamer", "video", "render", "input", "audio", "systemd-journal"},
			}, nil
		},
		EnableLingerFn: func(context.Context, *Deps, string, bool) error {
			lingerCalls++
			return nil
		},
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lingerCalls != 0 {
		t.Errorf("EnableLingerFn should not run when linger disabled; got %d", lingerCalls)
	}
	d := deps.State.Get(HeadlessUserName).Details
	if d["linger_enabled"] != false {
		t.Errorf("linger_enabled: got %v want false", d["linger_enabled"])
	}
}

// -----------------------------------------------------------------------------
// fatal paths
// -----------------------------------------------------------------------------

func TestHeadlessUser_UseraddFailureFailsFatal(t *testing.T) {
	deps := headlessUserDeps(t)
	ph := HeadlessUser{
		LookupFn: func(context.Context, *Deps, string) (UserInfo, error) {
			return UserInfo{}, nil
		},
		CreateUserFn: func(context.Context, *Deps, string, string) error {
			return errors.New("useradd: exit 1")
		},
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal on useradd failure")
	}
	if got := deps.State.Get(HeadlessUserName).Status; got != state.StatusFailedFatal {
		t.Errorf("status: got %q want failed_fatal", got)
	}
}

// -----------------------------------------------------------------------------
// pure helpers
// -----------------------------------------------------------------------------

func TestMissingGroups(t *testing.T) {
	have := []string{"video", "audio", "cloudgamer"}
	want := []string{"video", "render", "input", "audio", "systemd-journal"}
	got := missingGroups(have, want)
	expected := []string{"render", "input", "systemd-journal"}
	if !reflect.DeepEqual(got, expected) {
		t.Errorf("missingGroups: got %v want %v", got, expected)
	}
}

func TestParseUIDForState(t *testing.T) {
	cases := map[string]int{
		"1001": 1001,
		"0":    0,
		"":     0,
		"abc":  0,
		"-1":   0,
	}
	for in, want := range cases {
		if got := ParseUIDForState(in); got != want {
			t.Errorf("ParseUIDForState(%q): got %d want %d", in, got, want)
		}
	}
}
