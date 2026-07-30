package phase

// prebuilt.go downloads + installs prebuilt CloudDeploy compile artifacts
// (the compiled Sunshine fork binary + assets, and the patched KWin .debs)
// from a GitHub release so the sunshine-build and kwin-patch phases can skip
// the slow source compiles. The release layout (one .tar.gz per Ubuntu
// version + arch) is produced by scripts/build-prebuilt or manually:
//
//	clouddeploy-prebuilt-ubuntu2510-amd64.tar.gz
//	├── manifest.json   {sunshine_commit, kwin_version, ubuntu, arch, built_utc}
//	├── sunshine/{sunshine, assets/...}
//	└── kwin/*.deb
//
// Everything here is best-effort: a missing release, a download failure, or a
// version mismatch returns an error and the caller compiles from source.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/config"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
)

// prebuiltCacheDir is where downloaded bundles are cached so re-runs (and
// repeated phases) do not re-download.
const prebuiltCacheDir = "/var/lib/clouddeploy/prebuilt"

// PrebuiltManifest is the bundle's manifest.json.
type PrebuiltManifest struct {
	SunshineCommit string `json:"sunshine_commit"`
	KWinVersion    string `json:"kwin_version"`
	Ubuntu         string `json:"ubuntu"`
	Arch           string `json:"arch"`
	BuiltUTC       string `json:"built_utc"`

	// CUDA reports whether the bundled Sunshine was compiled WITH its
	// CUDA capture module (SUNSHINE_ENABLE_CUDA=ON).
	//
	// Pointer on purpose: "absent" must stay distinguishable from
	// "false". Bundles published before this field existed say nothing
	// about CUDA, and at least one of them (prebuilt-ubuntu2510-amd64,
	// built 2026-06-07) contains a CUDA-less binary. Because
	// deploy.use_prebuilt defaults to true, that bundle silently won
	// over every capable host: capture fell back to GPU -> RAM -> GPU,
	// pinning one core and capping a 4K120 HDR session near 40fps while
	// the deploy reported success. Treat absent as "unknown" and refuse
	// it whenever the profile requests CUDA capture.
	CUDA *bool `json:"cuda,omitempty"`
}

// HasCUDA reports whether the bundle positively declares a CUDA-enabled
// Sunshine. Absent (older bundles) and explicit false both return false.
func (m PrebuiltManifest) HasCUDA() bool { return m.CUDA != nil && *m.CUDA }

// CUDAClaim renders the manifest's CUDA field for diagnostics.
func (m PrebuiltManifest) CUDAClaim() string {
	if m.CUDA == nil {
		return "unknown (manifest predates the cuda field)"
	}
	if *m.CUDA {
		return "true"
	}
	return "false"
}

// prebuiltMustDeclareCUDA reports whether a prebuilt Sunshine must positively
// declare CUDA support. Optional CUDA still expresses a preference to use the
// host's available toolkit; only an explicit Sunshine opt-out permits a
// CUDA-less bundle.
func prebuiltMustDeclareCUDA(profile *config.Profile, sunshine config.SunshineConfig) bool {
	if profile == nil || !profile.WantsCUDA() {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(sunshine.EnableCUDA), "false")
}

// PrebuiltBundle is a downloaded + extracted bundle.
type PrebuiltBundle struct {
	Tag      string
	Dir      string
	Manifest PrebuiltManifest
}

// PrebuiltTag computes the release tag for a host Ubuntu version + arch,
// e.g. ("25.10", "amd64") -> "prebuilt-ubuntu2510-amd64".
func PrebuiltTag(ubuntuVer, arch string) string {
	v := strings.ReplaceAll(strings.TrimSpace(ubuntuVer), ".", "")
	return "prebuilt-ubuntu" + v + "-" + strings.TrimSpace(arch)
}

func prebuiltAsset(tag string) string { return "clouddeploy-" + tag + ".tar.gz" }

// hostUbuntuArch reads VERSION_ID from /etc/os-release and the dpkg
// architecture so the prebuilt tag matches the actual running host.
func hostUbuntuArch(ctx context.Context, deps *Deps) (ubuntuVer, arch string) {
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(ln, "VERSION_ID=") {
				ubuntuVer = strings.Trim(strings.TrimSpace(strings.TrimPrefix(ln, "VERSION_ID=")), "\"")
			}
		}
	}
	arch = "amd64"
	if deps != nil && deps.Runner != nil {
		res := deps.Runner.Exec(ctx, runner.CommandSpec{
			Argv: []string{"dpkg", "--print-architecture"}, LogFile: "-",
			Timeout: 10 * time.Second, DryRun: deps.DryRun,
		})
		if a := strings.TrimSpace(res.Stdout); a != "" {
			arch = a
		}
	}
	return ubuntuVer, arch
}

// ensurePrebuiltBundle downloads (cached) + extracts the prebuilt bundle for
// the host's Ubuntu version + arch and parses its manifest. Returns an error
// (caller falls back to compiling) when the release is absent or the
// download/extract/parse fails.
func ensurePrebuiltBundle(ctx context.Context, deps *Deps, repo, ubuntuVer, arch string) (*PrebuiltBundle, error) {
	if strings.TrimSpace(ubuntuVer) == "" {
		return nil, fmt.Errorf("prebuilt: could not determine host Ubuntu VERSION_ID")
	}
	tag := PrebuiltTag(ubuntuVer, arch)
	asset := prebuiltAsset(tag)
	dir := filepath.Join(prebuiltCacheDir, tag)
	tgz := dir + ".tar.gz"
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, tag, asset)

	if err := run(ctx, deps, "", []string{"install", "-d", "-m", "0755", prebuiltCacheDir}, time.Minute, true); err != nil {
		return nil, err
	}
	// Download once; reuse a non-empty cached tarball on re-runs.
	cached := false
	if fi, err := os.Stat(tgz); err == nil && fi.Size() > 0 {
		cached = true
	}
	if !cached {
		res := deps.Runner.Exec(ctx, runner.CommandSpec{
			Argv:    []string{"curl", "-fL", "--retry", "3", "--connect-timeout", "20", "-o", tgz, url},
			Sudo:    true,
			LogFile: "-",
			Timeout: 15 * time.Minute,
			DryRun:  deps.DryRun,
		})
		if res.Err != nil {
			_ = os.Remove(tgz)
			return nil, fmt.Errorf("prebuilt: download %s: %w", url, res.Err)
		}
	}
	// Always extract fresh into a clean dir.
	_ = run(ctx, deps, "", []string{"rm", "-rf", dir}, time.Minute, true)
	if err := run(ctx, deps, "", []string{"install", "-d", "-m", "0755", dir}, time.Minute, true); err != nil {
		return nil, err
	}
	if err := run(ctx, deps, "", []string{"tar", "xzf", tgz, "-C", dir}, 10*time.Minute, true); err != nil {
		return nil, fmt.Errorf("prebuilt: extract: %w", err)
	}
	mfBytes, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("prebuilt: read manifest: %w", err)
	}
	var m PrebuiltManifest
	if err := json.Unmarshal(mfBytes, &m); err != nil {
		return nil, fmt.Errorf("prebuilt: parse manifest: %w", err)
	}
	return &PrebuiltBundle{Tag: tag, Dir: dir, Manifest: m}, nil
}

// stagePrebuiltSunshine copies the prebuilt Sunshine binary + assets into the
// phase build dir (<buildDir>/build/{sunshine,assets}) so the existing
// post-build install flow (install binary, symlink, setcap, installSunshineAssets)
// runs unchanged -- just without clone/cmake/build.
func stagePrebuiltSunshine(ctx context.Context, deps *Deps, buildDir string, b *PrebuiltBundle) error {
	buildSub := filepath.Join(buildDir, "build")
	if err := run(ctx, deps, "", []string{"install", "-d", "-m", "0755", buildSub}, time.Minute, true); err != nil {
		return err
	}
	binSrc := filepath.Join(b.Dir, "sunshine", "sunshine")
	if _, err := os.Stat(binSrc); err != nil {
		return fmt.Errorf("prebuilt: sunshine binary missing in bundle: %w", err)
	}
	if err := run(ctx, deps, "", []string{"install", "-m", "0755", binSrc, filepath.Join(buildSub, "sunshine")}, time.Minute, true); err != nil {
		return fmt.Errorf("prebuilt: stage sunshine binary: %w", err)
	}
	assetsSrc := filepath.Join(b.Dir, "sunshine", "assets")
	assetsDst := filepath.Join(buildSub, "assets")
	if err := run(ctx, deps, "", []string{"bash", "-lc",
		"rm -rf " + shellQuote(assetsDst) + " && cp -a " + shellQuote(assetsSrc) + " " + shellQuote(assetsDst)},
		5*time.Minute, true); err != nil {
		return fmt.Errorf("prebuilt: stage sunshine assets: %w", err)
	}
	return nil
}

// installPrebuiltKWin installs the prebuilt patched KWin .debs (reusing the
// same apt-get flags + apt-mark hold as the from-source path) and returns the
// installed package names.
func installPrebuiltKWin(ctx context.Context, deps *Deps, b *PrebuiltBundle, hold bool) ([]string, error) {
	debs, _ := filepath.Glob(filepath.Join(b.Dir, "kwin", "*.deb"))
	if len(debs) == 0 {
		return nil, fmt.Errorf("prebuilt: no KWin .debs in bundle %s", b.Tag)
	}
	if err := run(ctx, deps, "", kwinLocalDebInstallArgs(debs), 30*time.Minute, true); err != nil {
		return nil, fmt.Errorf("prebuilt: install KWin .debs: %w", err)
	}
	pkgs := packageNamesFromDebs(debs)
	if hold && len(pkgs) > 0 {
		if err := run(ctx, deps, "", append([]string{"apt-mark", "hold"}, pkgs...), 2*time.Minute, true); err != nil {
			return pkgs, fmt.Errorf("prebuilt: apt-mark hold KWin: %w", err)
		}
	}
	return pkgs, nil
}
