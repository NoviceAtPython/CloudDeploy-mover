package phase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/apt"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/config"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/state"
	sunshinecodec "github.com/NoviceAtPython/CloudDeploy-mover/internal/sunshine"
)

const (
	SunshineBuildName      = "sunshine_build"
	SunshineConfigName     = "sunshine_config"
	TailscaleName          = "tailscale"
	PipeWireAudioName      = "pipewire_audio"
	StreamingServicesName  = "streaming_services"
	StreamValidateName     = "stream_validate"
	OptionalAppsName       = "optional_apps"
	defaultSunshineConfRel = ".config/sunshine/sunshine.conf"
)

var sunshineBuildDeps = []string{
	"git", "cmake", "ninja-build", "build-essential", "pkg-config", "python3", "nodejs", "npm",
	"libssl-dev", "libcurl4-openssl-dev", "libcap-dev", "libdrm-dev", "libevdev-dev", "libgbm-dev",
	"libminiupnpc-dev", "libnotify-dev", "libnuma-dev", "libopus-dev", "libpulse-dev", "libva-dev",
	"libvdpau-dev", "libwayland-dev", "libx11-dev", "libxcb1-dev", "libxcb-shm0-dev",
	"libxcb-xfixes0-dev", "libxfixes-dev", "libxrandr-dev", "libxtst-dev", "libsystemd-dev",
	"libudev-dev", "libayatana-appindicator3-dev", "libavcodec-dev", "libavdevice-dev",
	"libavfilter-dev", "libavformat-dev", "libavutil-dev", "libswscale-dev", "libswresample-dev",
	"libboost-filesystem-dev", "libboost-log-dev", "libboost-program-options-dev", "libboost-system-dev",
	"libboost-thread-dev", "libvulkan-dev", "vulkan-tools", "vulkan-validationlayers", "glslang-tools",
	"glslc", "libpipewire-0.3-dev", "doxygen", "graphviz",
}

type SunshineBuild struct {
	BuildDeps     []string
	DoxygenFn     func(context.Context, *Deps, config.SunshineConfig) (doxygenInfo, error)
	CUDAToolkitFn func(context.Context, *Deps, config.SunshineConfig) cudaToolchain
	SetcapFn      func(context.Context, *Deps, string) error
}

func (SunshineBuild) Name() string { return SunshineBuildName }

func (p SunshineBuild) Run(ctx context.Context, deps *Deps) error {
	log := logger(deps)
	if shouldSkip(deps.State, SunshineBuildName) {
		log.Info("phase sunshine-build: already done; skipping")
		return nil
	}
	deps.State.MarkRunning(SunshineBuildName)
	_ = deps.PersistState()

	cfg := deps.Profile.EffectiveSunshine()
	details := map[string]any{
		"source":      cfg.Source,
		"fork_repo":   cfg.ForkRepo,
		"fork_branch": cfg.ForkBranch,
		"fork_commit": cfg.ForkCommit,
		"build_dir":   cfg.BuildDir,
		"install_bin": cfg.InstallBin,
		"build_jobs":  cfg.BuildJobs,
	}
	if strings.EqualFold(cfg.Source, "deb") {
		err := deps.APT.Run(ctx, func(tc *apt.TxContext) error {
			return tc.Install(ctx, []string{"sunshine"})
		})
		if err != nil {
			return failPhase(deps, SunshineBuildName, details, "install packaged Sunshine", err, true)
		}
		details["install_method"] = "deb"
		details["binary"] = "/usr/bin/sunshine"
		deps.State.MarkDone(SunshineBuildName, details)
		_ = deps.PersistState()
		return nil
	}

	buildDeps := p.BuildDeps
	if len(buildDeps) == 0 {
		buildDeps = sunshineBuildDeps
	}
	if err := deps.APT.Run(ctx, func(tc *apt.TxContext) error {
		return tc.Install(ctx, buildDeps)
	}); err != nil {
		return failPhase(deps, SunshineBuildName, details, "install Sunshine build deps", err, true)
	}

	if deps.DryRun {
		details["dry_run"] = true
		deps.State.MarkDone(SunshineBuildName, details)
		_ = deps.PersistState()
		return nil
	}

	if _, err := os.Stat(filepath.Join(cfg.BuildDir, ".git")); err != nil {
		if err := run(ctx, deps, "", []string{"git", "clone", "--recursive", cfg.ForkRepo, cfg.BuildDir}, 30*time.Minute, true); err != nil {
			return failPhase(deps, SunshineBuildName, details, "clone Sunshine fork", err, true)
		}
	} else {
		if err := run(ctx, deps, cfg.BuildDir, []string{"git", "fetch", "--all", "--tags", "--prune"}, 15*time.Minute, true); err != nil {
			return failPhase(deps, SunshineBuildName, details, "fetch Sunshine fork", err, true)
		}
	}
	checkout := cfg.ForkCommit
	if strings.TrimSpace(checkout) == "" {
		checkout = cfg.ForkBranch
	}
	if err := run(ctx, deps, cfg.BuildDir, []string{"git", "checkout", checkout}, 5*time.Minute, true); err != nil {
		return failPhase(deps, SunshineBuildName, details, "checkout Sunshine commit", err, true)
	}
	head, _ := output(ctx, deps, cfg.BuildDir, []string{"git", "rev-parse", "HEAD"}, 30*time.Second, false)
	details["commit"] = strings.TrimSpace(head)

	doxFn := p.DoxygenFn
	if doxFn == nil {
		doxFn = ensureSunshineDoxygen
	}
	dox, err := doxFn(ctx, deps, cfg)
	if err != nil {
		return failPhase(deps, SunshineBuildName, details, "resolve Doxygen", err, true)
	}
	details["doxygen_path"] = dox.Path
	details["doxygen_version"] = dox.Version
	details["doxygen_source"] = dox.Source
	if dox.SHA256 != "" {
		details["doxygen_sha256"] = dox.SHA256
	}
	cudaFn := p.CUDAToolkitFn
	if cudaFn == nil {
		cudaFn = discoverSunshineCUDA
	}
	cuda := cudaFn(ctx, deps, cfg)
	details["sunshine_cuda_mode"] = cfg.EnableCUDA
	details["cuda_detected"] = cuda.Found
	details["cuda_nvcc"] = cuda.NVCC
	details["cuda_root"] = cuda.Root
	cmakeArgs := sunshineCMakeArgs(cfg, dox.Path, cuda, false)
	if err := run(ctx, deps, cfg.BuildDir, cmakeArgs, 30*time.Minute, true); err != nil {
		if shouldRetrySunshineWithoutCUDA(cfg, cuda) {
			details["fallback_to_no_cuda"] = true
			cmakeArgs = sunshineCMakeArgs(cfg, dox.Path, cudaToolchain{}, true)
			if retryErr := run(ctx, deps, cfg.BuildDir, cmakeArgs, 30*time.Minute, true); retryErr != nil {
				return failPhase(deps, SunshineBuildName, details, "configure Sunshine without CUDA fallback", retryErr, true)
			}
		} else {
			return failPhase(deps, SunshineBuildName, details, "configure Sunshine", err, true)
		}
	}
	details["cmake_args"] = strings.Join(cmakeArgs, " ")
	if err := run(ctx, deps, cfg.BuildDir, []string{"cmake", "--build", "build", "--target", "sunshine", "-j", strconv.Itoa(cfg.BuildJobs)}, 90*time.Minute, true); err != nil {
		return failPhase(deps, SunshineBuildName, details, "build Sunshine", err, true)
	}
	built := filepath.Join(cfg.BuildDir, "build", "sunshine")
	details["built_binary"] = built
	if err := run(ctx, deps, "", []string{"install", "-m", "0755", built, cfg.InstallBin}, time.Minute, true); err != nil {
		return failPhase(deps, SunshineBuildName, details, "install Sunshine binary", err, true)
	}
	_ = run(ctx, deps, "", []string{"ln", "-sf", cfg.InstallBin, "/usr/local/bin/sunshine"}, time.Minute, true)
	if err := validateExecutable(cfg.InstallBin); err != nil {
		return failPhase(deps, SunshineBuildName, details, "validate installed Sunshine binary", err, true)
	}
	setcapFn := p.SetcapFn
	if setcapFn == nil {
		setcapFn = applySunshineSetcap
	}
	setcapTarget := cfg.InstallBin
	if realTarget, err := filepath.EvalSymlinks(cfg.InstallBin); err == nil && strings.TrimSpace(realTarget) != "" {
		setcapTarget = realTarget
	}
	attemptSunshineSetcap(ctx, deps, details, setcapTarget, setcapFn, log)
	assets, err := installSunshineAssets(ctx, deps, cfg.BuildDir)
	if err != nil {
		return failPhase(deps, SunshineBuildName, details, "install Sunshine runtime assets", err, true)
	}
	details["assets_apps_json"] = assets.AppsJSON
	details["assets_web_index"] = assets.WebIndex
	details["web_asset_mode"] = assets.WebAssetMode
	details["web_raw_markers_present"] = assets.WebRawMarkers
	details["web_build_attempted"] = assets.WebBuildAttempted
	if assets.WebBuildSource != "" {
		details["web_build_source"] = assets.WebBuildSource
	}
	if assets.WebBuildError != "" {
		details["web_build_error"] = assets.WebBuildError
	}
	ver, _ := output(ctx, deps, "", []string{cfg.InstallBin, "--version"}, 20*time.Second, false)
	details["version"] = strings.TrimSpace(ver)
	details["install_method"] = "fork"
	details["binary"] = cfg.InstallBin
	details["installed_binary"] = cfg.InstallBin
	details["symlinked_binary"] = "/usr/local/bin/sunshine"
	deps.State.MarkDone(SunshineBuildName, details)
	_ = deps.PersistState()
	log.Info("phase sunshine-build: done", "binary", cfg.InstallBin)
	return nil
}

func applySunshineSetcap(ctx context.Context, deps *Deps, target string) error {
	return run(ctx, deps, "", []string{"setcap", "cap_sys_admin,cap_net_bind_service,cap_sys_nice+ep", target}, time.Minute, true)
}

func attemptSunshineSetcap(ctx context.Context, deps *Deps, details map[string]any, target string, fn func(context.Context, *Deps, string) error, log *slog.Logger) {
	details["setcap_attempted"] = true
	details["setcap_target"] = target
	if err := fn(ctx, deps, target); err != nil {
		// The systemd service carries AmbientCapabilities and
		// CapabilityBoundingSet. Live VM regression: setcap failed
		// with "Invalid file '/usr/sbin/setcap' for capability
		// operation" on an otherwise working install. Keep the
		// build moving and let streaming-services/stream-validate
		// prove runtime capture with service-level caps.
		details["setcap_success"] = false
		details["setcap_error"] = err.Error()
		details["setcap_nonfatal"] = true
		if log != nil {
			log.Warn("phase sunshine-build: setcap failed nonfatally; relying on service-level capabilities", "target", target, "err", err)
		}
		return
	}
	details["setcap_success"] = true
	details["setcap_nonfatal"] = false
}

type sunshineAssetInfo struct {
	AppsJSON          bool
	WebIndex          bool
	WebAssetMode      string
	FilesystemWeb     string
	WebRawMarkers     bool
	WebBuildAttempted bool
	WebBuildSource    string
	WebBuildError     string
}

func installSunshineAssets(ctx context.Context, deps *Deps, buildDir string) (sunshineAssetInfo, error) {
	info := sunshineAssetInfo{WebAssetMode: "unknown"}
	src := filepath.Join(buildDir, "build", "assets")
	if err := run(ctx, deps, "", []string{"install", "-d", "-m", "0755", "/usr/local/assets"}, time.Minute, true); err != nil {
		return info, err
	}
	if err := run(ctx, deps, "", []string{"bash", "-lc", "cp -a " + shellQuote(src) + "/. /usr/local/assets/"}, 5*time.Minute, true); err != nil {
		return info, err
	}
	_ = run(ctx, deps, "", []string{"bash", "-lc", "if [ -d /usr/share/sunshine/web ]; then mkdir -p /usr/local/assets/web && cp -a /usr/share/sunshine/web/. /usr/local/assets/web/; fi"}, 2*time.Minute, true)
	ensureBuiltSunshineWebAssets(ctx, deps, buildDir, &info)
	if _, err := os.Stat("/usr/local/assets/apps.json"); err != nil {
		return info, fmt.Errorf("/usr/local/assets/apps.json missing after asset install")
	}
	info.AppsJSON = true
	if _, err := os.Stat("/usr/local/assets/web/index.html"); err == nil {
		info.WebIndex = true
		info.WebRawMarkers = sunshineWebRawMarkers("/usr/local/assets/web")
		if info.WebRawMarkers {
			info.WebAssetMode = "raw-source"
		} else {
			info.WebAssetMode = "filesystem"
		}
		info.FilesystemWeb = "/usr/local/assets/web/index.html"
	} else {
		// Some Sunshine fork builds serve the UI from embedded or
		// differently laid-out assets. Do not fail the build here;
		// stream_validate performs an actual HTTPS Web UI probe and
		// records whether the UI responds.
		info.WebAssetMode = "embedded-or-unknown"
	}
	return info, nil
}

func ensureBuiltSunshineWebAssets(ctx context.Context, deps *Deps, buildDir string, info *sunshineAssetInfo) {
	needsBuild := sunshineWebRawMarkers("/usr/local/assets/web") || !pathExists("/usr/local/assets/web/index.html")
	if info == nil || deps.DryRun || !needsBuild {
		return
	}
	foundPackage := false
	for _, candidate := range []string{
		filepath.Join(buildDir, "src_assets", "common", "assets", "web"),
		filepath.Join(buildDir, "src_assets", "common", "web"),
		filepath.Join(buildDir, "src_assets", "web"),
		filepath.Join(buildDir, "web"),
	} {
		if _, err := os.Stat(filepath.Join(candidate, "package.json")); err != nil {
			continue
		}
		foundPackage = true
		info.WebBuildAttempted = true
		info.WebBuildSource = candidate
		if err := buildAndInstallSunshineWeb(ctx, deps, candidate); err != nil {
			info.WebBuildError = err.Error()
			continue
		}
		if !sunshineWebRawMarkers("/usr/local/assets/web") {
			info.WebBuildError = ""
			return
		}
		info.WebBuildError = "web build completed but runtime assets still contain raw Vue/template markers"
	}
	if !foundPackage && info.WebBuildError == "" {
		info.WebBuildError = "runtime web assets are missing or raw, but no Sunshine web package.json was found under known source locations"
	}
}

func buildAndInstallSunshineWeb(ctx context.Context, deps *Deps, src string) error {
	if _, err := os.Stat(filepath.Join(src, "package-lock.json")); err == nil {
		if err := run(ctx, deps, src, []string{"npm", "ci"}, 15*time.Minute, true); err != nil {
			return err
		}
	} else if err := run(ctx, deps, src, []string{"npm", "install"}, 15*time.Minute, true); err != nil {
		return err
	}
	if err := run(ctx, deps, src, []string{"npm", "run", "build"}, 20*time.Minute, true); err != nil {
		return err
	}
	for _, out := range []string{"dist", "build", "out"} {
		p := filepath.Join(src, out)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			if err := run(ctx, deps, "", []string{"install", "-d", "-m", "0755", "/usr/local/assets/web"}, time.Minute, true); err != nil {
				return err
			}
			return run(ctx, deps, "", []string{"bash", "-lc", "cp -a " + shellQuote(p) + "/. /usr/local/assets/web/"}, 5*time.Minute, true)
		}
	}
	return fmt.Errorf("npm build succeeded in %s but no dist/build/out directory was found", src)
}

func sunshineWebRawMarkers(root string) bool {
	for _, rel := range []string{"index.html", "main.js", "main.ts", "src/main.js", "src/main.ts"} {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err == nil && containsRawWebMarker(string(b)) {
			return true
		}
	}
	raw := false
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || raw || d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".html" && ext != ".js" && ext != ".ts" && ext != ".vue" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err == nil && containsRawWebMarker(string(b)) {
			raw = true
		}
		return nil
	})
	return raw
}

func containsRawWebMarker(s string) bool {
	return strings.Contains(s, "<%- header %>") ||
		strings.Contains(s, "import { createApp } from 'vue'") ||
		strings.Contains(s, `import { createApp } from "vue"`) ||
		strings.Contains(s, ".vue'") ||
		strings.Contains(s, `.vue"`) ||
		strings.Contains(s, "{{ $t(")
}

type doxygenInfo struct {
	Path    string
	Version string
	Source  string
	SHA256  string
}

type cudaToolchain struct {
	Found bool
	NVCC  string
	Root  string
}

func ensureSunshineDoxygen(ctx context.Context, deps *Deps, cfg config.SunshineConfig) (doxygenInfo, error) {
	if deps.DryRun {
		return doxygenInfo{Path: "/usr/local/bin/doxygen", Version: cfg.DoxygenVersion, Source: "dry-run"}, nil
	}
	if cmd, err := resolveDoxygen(); err == nil {
		ver, _ := output(ctx, deps, "", []string{cmd.Path, "--version"}, 15*time.Second, false)
		version := strings.TrimSpace(strings.Split(strings.TrimSpace(ver), "\n")[0])
		if versionAtLeast(version, "1.10.0") {
			return doxygenInfo{Path: cmd.Path, Version: version, Source: "system"}, nil
		}
	}
	if strings.TrimSpace(cfg.DoxygenURL) == "" || strings.TrimSpace(cfg.DoxygenSHA256) == "" {
		return doxygenInfo{}, fmt.Errorf("system doxygen is missing/too old and no upstream doxygen URL/SHA configured")
	}
	tmp := "/tmp/doxygen-" + cfg.DoxygenVersion + ".linux.bin.tar.gz"
	if err := run(ctx, deps, "", []string{"curl", "-fL", "--retry", "3", "-o", tmp, cfg.DoxygenURL}, 10*time.Minute, true); err != nil {
		return doxygenInfo{}, err
	}
	got, err := sha256File(tmp)
	if err != nil {
		return doxygenInfo{}, err
	}
	if !strings.EqualFold(got, cfg.DoxygenSHA256) {
		return doxygenInfo{}, fmt.Errorf("doxygen tarball sha256 mismatch: got %s want %s", got, cfg.DoxygenSHA256)
	}
	parent := filepath.Dir(cfg.DoxygenInstallDir)
	if err := run(ctx, deps, "", []string{"install", "-d", "-m", "0755", parent}, time.Minute, true); err != nil {
		return doxygenInfo{}, err
	}
	if err := run(ctx, deps, "", []string{"bash", "-lc", "rm -rf " + shellQuote(cfg.DoxygenInstallDir) + " && tar -C " + shellQuote(parent) + " -xzf " + shellQuote(tmp)}, 5*time.Minute, true); err != nil {
		return doxygenInfo{}, err
	}
	candidates := []string{
		filepath.Join(cfg.DoxygenInstallDir, "bin", "doxygen"),
		filepath.Join(parent, "doxygen-"+cfg.DoxygenVersion, "bin", "doxygen"),
	}
	doxygenPath := ""
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			doxygenPath = c
			break
		}
	}
	if doxygenPath == "" {
		return doxygenInfo{}, fmt.Errorf("upstream Doxygen extracted but no doxygen binary found under %s", parent)
	}
	if err := run(ctx, deps, "", []string{"ln", "-sf", doxygenPath, "/usr/local/bin/doxygen"}, time.Minute, true); err != nil {
		return doxygenInfo{}, err
	}
	ver, _ := output(ctx, deps, "", []string{doxygenPath, "--version"}, 15*time.Second, false)
	return doxygenInfo{Path: doxygenPath, Version: strings.TrimSpace(ver), Source: "upstream", SHA256: got}, nil
}

func discoverSunshineCUDA(ctx context.Context, deps *Deps, cfg config.SunshineConfig) cudaToolchain {
	if strings.EqualFold(strings.TrimSpace(cfg.EnableCUDA), "false") {
		return cudaToolchain{}
	}
	nvcc := ""
	root := strings.TrimSpace(cfg.CUDARoot)
	if root != "" {
		c := filepath.Join(root, "bin", "nvcc")
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			nvcc = c
		}
	}
	if nvcc == "" {
		if cmd, err := resolveNVCC(); err == nil {
			nvcc = cmd.Path
		}
	}
	if nvcc == "" {
		return cudaToolchain{}
	}
	if root == "" {
		root = filepath.Dir(filepath.Dir(nvcc))
	}
	return cudaToolchain{Found: true, NVCC: nvcc, Root: root}
}

func sunshineCMakeArgs(cfg config.SunshineConfig, doxygenPath string, cuda cudaToolchain, forceNoCUDA bool) []string {
	args := []string{"cmake", "-S", ".", "-B", "build", "-G", "Ninja", "-DCMAKE_BUILD_TYPE=Release"}
	if strings.TrimSpace(doxygenPath) != "" {
		args = append(args, "-DDOXYGEN_EXECUTABLE="+doxygenPath)
	}
	mode := strings.ToLower(strings.TrimSpace(cfg.EnableCUDA))
	if mode == "" {
		mode = "auto"
	}
	enableCUDA := !forceNoCUDA && mode != "false" && cuda.Found
	if mode == "true" && !forceNoCUDA {
		enableCUDA = true
	}
	if enableCUDA {
		args = append(args, "-DSUNSHINE_ENABLE_CUDA=ON")
		if cuda.Root != "" {
			args = append(args, "-DCUDAToolkit_ROOT="+cuda.Root)
		}
		if cuda.NVCC != "" {
			args = append(args, "-DCMAKE_CUDA_COMPILER="+cuda.NVCC)
		}
		if !cfg.CUDAFailOnMissing {
			args = append(args, "-DCUDA_FAIL_ON_MISSING=OFF")
		}
	} else {
		args = append(args, "-DSUNSHINE_ENABLE_CUDA=OFF", "-DCUDA_FAIL_ON_MISSING=OFF")
	}
	return args
}

func shouldRetrySunshineWithoutCUDA(cfg config.SunshineConfig, cuda cudaToolchain) bool {
	mode := strings.ToLower(strings.TrimSpace(cfg.EnableCUDA))
	if mode == "true" || cfg.CUDAFailOnMissing {
		return false
	}
	return cuda.Found
}

func versionAtLeast(got, want string) bool {
	g := versionParts(got)
	w := versionParts(want)
	for len(g) < 3 {
		g = append(g, 0)
	}
	for len(w) < 3 {
		w = append(w, 0)
	}
	for i := 0; i < 3; i++ {
		if g[i] > w[i] {
			return true
		}
		if g[i] < w[i] {
			return false
		}
	}
	return true
}

func versionParts(s string) []int {
	re := regexp.MustCompile(`[0-9]+`)
	raw := re.FindAllString(s, 3)
	out := make([]int, 0, len(raw))
	for _, r := range raw {
		n, _ := strconv.Atoi(r)
		out = append(out, n)
	}
	return out
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func validateExecutable(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return fmt.Errorf("%s is a directory", path)
	}
	if st.Mode()&0o111 == 0 {
		return fmt.Errorf("%s exists but is not executable", path)
	}
	return nil
}

type SunshineConfigPhase struct {
	ConfigPath string
}

func (SunshineConfigPhase) Name() string { return SunshineConfigName }

func (p SunshineConfigPhase) Run(ctx context.Context, deps *Deps) error {
	log := logger(deps)
	if shouldSkip(deps.State, SunshineConfigName) {
		log.Info("phase sunshine-config: already done; skipping")
		return nil
	}
	deps.State.MarkRunning(SunshineConfigName)
	_ = deps.PersistState()
	desk := deps.Profile.EffectiveDesktop()
	cfg := deps.Profile.EffectiveSunshine()
	uid := uidFromState(deps)
	home := "/home/" + desk.User
	drm := selectedDRMDevice(deps)
	if drm == "" {
		drm = "/dev/dri/card1"
	}
	path := strings.TrimSpace(p.ConfigPath)
	if path == "" {
		path = strings.TrimSpace(cfg.ConfigPath)
	}
	if path == "" {
		path = home + "/" + defaultSunshineConfRel
	}
	origins := sunshineCSRFOrigins(deps, tailscaleIPFromState(deps))
	body := renderSunshineConfig(cfg, drm, origins)
	details := map[string]any{"path": path, "adapter_name": drm, "csrf_allowed_origins": origins, "uid": uid}
	if !deps.DryRun {
		configDir := filepath.Dir(path)
		credsDir := filepath.Join(configDir, "credentials")
		if err := os.MkdirAll(credsDir, 0o700); err != nil {
			return failPhase(deps, SunshineConfigName, details, "create Sunshine config dir", err, true)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			return failPhase(deps, SunshineConfigName, details, "write Sunshine config", err, true)
		}
		for _, cmd := range [][]string{
			{"chown", "-R", desk.User + ":" + desk.User, configDir},
			{"chmod", "0700", configDir},
			{"chmod", "0700", credsDir},
			{"chmod", "0600", path},
			{"runuser", "-u", desk.User, "--", "test", "-w", configDir},
			{"runuser", "-u", desk.User, "--", "mkdir", "-p", credsDir},
		} {
			if err := run(ctx, deps, "", cmd, time.Minute, true); err != nil {
				return failPhase(deps, SunshineConfigName, details, "fix/validate Sunshine config ownership", err, true)
			}
		}
		details["credentials_dir"] = credsDir
	}
	if strings.Contains(body, "\nhdr =") || strings.Contains(body, "\nfps =") || strings.Contains(body, "\nresolutions =") {
		return failPhase(deps, SunshineConfigName, details, "invalid Sunshine config keys", fmt.Errorf("generated sunshine.conf contains invalid hdr/fps/resolutions keys"), true)
	}
	deps.State.MarkDone(SunshineConfigName, details)
	_ = deps.PersistState()
	return nil
}

func renderSunshineConfig(cfg config.SunshineConfig, drm string, origins []string) string {
	encoder := strings.TrimSpace(cfg.Encoder)
	if encoder == "" {
		encoder = "nvenc"
	}
	capture := strings.TrimSpace(cfg.Capture)
	if capture == "" {
		capture = "kms"
	}
	// AV1-first ordering and the Main10 mode bits come from
	// SunshineConfig now (live-VM regression fix). Defaults are
	// av1_mode=3 + hevc_mode=3 - HDR Main10 advertised on both
	// branches so Moonlight has a real HDR path to negotiate. The
	// profile validator already gates HDR profiles against modes
	// below 3, so anything that reaches here is internally
	// consistent.
	av1Mode := fmt.Sprintf("%d", cfg.Av1ModeValue())
	hevcMode := fmt.Sprintf("%d", cfg.HevcModeValue())
	var b strings.Builder
	b.WriteString("min_log_level = debug\n")
	b.WriteString("capture = " + capture + "\n")
	b.WriteString("encoder = " + encoder + "\n")
	b.WriteString("adapter_name = " + drm + "\n")
	b.WriteString("stream_audio = disabled\n")
	b.WriteString("address_family = ipv4\n")
	b.WriteString("ping_timeout = 60000\n")
	b.WriteString("hevc_mode = " + hevcMode + "\n")
	b.WriteString("av1_mode = " + av1Mode + "\n")
	if len(origins) > 0 {
		b.WriteString("csrf_allowed_origins = " + strings.Join(origins, ",") + "\n")
	}
	return b.String()
}

type Tailscale struct {
	IPFn func(context.Context, *Deps) (string, error)
}

func (Tailscale) Name() string { return TailscaleName }

func (p Tailscale) Run(ctx context.Context, deps *Deps) error {
	log := logger(deps)
	cfg := deps.Profile.EffectiveTailscale()
	key := strings.TrimSpace(os.Getenv(cfg.AuthKeyEnv))
	if prev := deps.State.Get(TailscaleName); prev != nil {
		switch prev.Status {
		case state.StatusDone:
			log.Info("phase tailscale: already done; skipping")
			return nil
		case state.StatusSkipped:
			if !cfg.EnabledValue() || key == "" {
				log.Info("phase tailscale: already skipped; no auth key available")
				return nil
			}
			log.Info("phase tailscale: previous skip recovered because auth key is now present")
		}
	}
	deps.State.MarkRunning(TailscaleName)
	_ = deps.PersistState()
	details := map[string]any{
		"enabled":             cfg.EnabledValue(),
		"authkey_env":         cfg.AuthKeyEnv,
		"authkey_env_present": key != "",
		"secrets_env_present": pathExists("/etc/clouddeploy/secrets.env"),
		"secrets_env_loaded":  key != "",
	}
	if !cfg.EnabledValue() {
		deps.State.MarkSkipped(TailscaleName, "tailscale.enabled=false")
		deps.State.Get(TailscaleName).Details = details
		_ = deps.PersistState()
		return nil
	}
	if key == "" {
		details["authkey_present"] = false
		deps.State.MarkSkipped(TailscaleName, cfg.AuthKeyEnv+" missing")
		deps.State.Get(TailscaleName).Details = details
		_ = deps.PersistState()
		return nil
	}
	if strings.ContainsAny(key, " \t\r\n") {
		return failPhase(deps, TailscaleName, details, "invalid Tailscale authkey", fmt.Errorf("%s must be a single-line key without whitespace", cfg.AuthKeyEnv), true)
	}
	details["authkey_present"] = true
	installMethod := "apt"
	if err := deps.APT.Run(ctx, func(tc *apt.TxContext) error { return tc.Install(ctx, []string{"tailscale"}) }); err != nil {
		details["apt_install_error"] = err.Error()
		installMethod = "tailscale-install-sh"
		if ierr := run(ctx, deps, "", []string{"bash", "-lc", "curl -fsSL https://tailscale.com/install.sh | sh"}, 5*time.Minute, true); ierr != nil {
			return failPhase(deps, TailscaleName, details, "install tailscale", fmt.Errorf("apt failed: %v; install.sh failed: %w", err, ierr), true)
		}
	}
	details["install_method"] = installMethod
	_ = run(ctx, deps, "", []string{"systemctl", "enable", "--now", "tailscaled"}, time.Minute, true)
	details["tailscaled_active"] = run(ctx, deps, "", []string{"systemctl", "is-active", "--quiet", "tailscaled"}, 15*time.Second, false) == nil
	args := []string{"tailscale", "up", "--authkey", key}
	if cfg.SSH {
		args = append(args, "--ssh")
	}
	var res runner.Result
	for attempt := 1; attempt <= 3; attempt++ {
		res = deps.Runner.Exec(ctx, runner.CommandSpec{
			Argv: args, Sudo: true, Timeout: 2 * time.Minute, DryRun: deps.DryRun,
			RedactArgs: []int{3}, LogFile: "-",
		})
		if res.Err == nil {
			break
		}
		time.Sleep(time.Duration(attempt) * 2 * time.Second)
	}
	details["tailscale_up_exit"] = res.ExitCode
	if res.Stderr != "" {
		details["tailscale_up_stderr_tail"] = lastLines(res.Stderr, 6)
	}
	if res.Err != nil {
		return failPhase(deps, TailscaleName, details, "tailscale up", res.Err, true)
	}
	status, _ := output(ctx, deps, "", []string{"tailscale", "status"}, 15*time.Second, false)
	details["tailscale_status_excerpt"] = lastLines(status, 8)
	ipFn := p.IPFn
	customIPFn := ipFn != nil
	if ipFn == nil {
		ipFn = func(ctx context.Context, deps *Deps) (string, error) {
			return output(ctx, deps, "", []string{"tailscale", "ip", "-4"}, 15*time.Second, false)
		}
	}
	ip, _ := ipFn(ctx, deps)
	ip = strings.TrimSpace(strings.Split(strings.TrimSpace(ip), "\n")[0])
	if deps.DryRun && ip == "" && !customIPFn {
		ip = "100.64.0.1"
	}
	details["tailscale_ip"] = ip
	if ip == "" {
		return failPhase(deps, TailscaleName, details, "tailscale ip missing", fmt.Errorf("tailscale up returned success but `tailscale ip -4` returned no address"), true)
	}
	if ip != "" {
		updateSunshineCSRF(ctx, deps, ip)
		details["sunshine_try_restart_after_csrf"] = run(ctx, deps, "", []string{"systemctl", "try-restart", "sunshine-headless.service"}, time.Minute, true) == nil
	}
	deps.State.MarkDone(TailscaleName, details)
	_ = deps.PersistState()
	return nil
}

type PipeWireAudio struct{}

func (PipeWireAudio) Name() string { return PipeWireAudioName }

func (p PipeWireAudio) Run(ctx context.Context, deps *Deps) error {
	log := logger(deps)
	if shouldSkip(deps.State, PipeWireAudioName) {
		log.Info("phase pipewire-audio: already done; skipping")
		return nil
	}
	deps.State.MarkRunning(PipeWireAudioName)
	_ = deps.PersistState()
	cfg := deps.Profile.EffectiveAudio()
	details := map[string]any{"enabled": cfg.EnabledValue(), "virtual_sink": cfg.VirtualSink}
	if !cfg.EnabledValue() {
		deps.State.MarkSkipped(PipeWireAudioName, "audio.enabled=false")
		deps.State.Get(PipeWireAudioName).Details = details
		_ = deps.PersistState()
		return nil
	}
	if err := deps.APT.Run(ctx, func(tc *apt.TxContext) error {
		return tc.Install(ctx, []string{"pipewire", "pipewire-pulse", "wireplumber", "pulseaudio-utils"})
	}); err != nil {
		return failPhase(deps, PipeWireAudioName, details, "install PipeWire", err, true)
	}
	desk := deps.Profile.EffectiveDesktop()
	uid := uidFromState(deps)
	_ = runAsDesktop(ctx, deps, desk.User, uid, []string{"systemctl", "--user", "enable", "--now", "pipewire", "pipewire-pulse", "wireplumber"}, time.Minute)
	_ = runAsDesktop(ctx, deps, desk.User, uid, []string{"pactl", "load-module", "module-null-sink", "sink_name=" + cfg.VirtualSink, "sink_properties=device.description=CloudDeploy"}, time.Minute)
	wpctl, _ := outputAsDesktop(ctx, deps, desk.User, uid, []string{"wpctl", "status"}, 15*time.Second)
	pactl, _ := outputAsDesktop(ctx, deps, desk.User, uid, []string{"pactl", "list", "short", "sinks"}, 15*time.Second)
	sources, _ := outputAsDesktop(ctx, deps, desk.User, uid, []string{"pactl", "list", "short", "sources"}, 15*time.Second)
	info, _ := outputAsDesktop(ctx, deps, desk.User, uid, []string{"pactl", "info"}, 15*time.Second)
	details["wpctl_seen"] = strings.TrimSpace(wpctl) != ""
	details["pactl_sinks"] = strings.TrimSpace(pactl)
	details["pactl_sources"] = strings.TrimSpace(sources)
	details["pactl_info_excerpt"] = lastLines(info, 8)
	details["virtual_sink_seen"] = strings.Contains(pactl, cfg.VirtualSink)
	if !deps.DryRun && strings.TrimSpace(wpctl) == "" && strings.TrimSpace(pactl) == "" {
		return failPhase(deps, PipeWireAudioName, details, "PipeWire validation failed", fmt.Errorf("neither wpctl nor pactl returned audio devices"), true)
	}
	deps.State.MarkDone(PipeWireAudioName, details)
	_ = deps.PersistState()
	return nil
}

type StreamingServices struct{}

func (StreamingServices) Name() string { return StreamingServicesName }

func (p StreamingServices) Run(ctx context.Context, deps *Deps) error {
	if shouldSkip(deps.State, StreamingServicesName) {
		logger(deps).Info("phase streaming-services: already done; skipping")
		return nil
	}
	deps.State.MarkRunning(StreamingServicesName)
	_ = deps.PersistState()
	desk := deps.Profile.EffectiveDesktop()
	cfg := deps.Profile.EffectiveSunshine()
	uid := uidFromState(deps)
	conf := sunshineConfigPath(deps)
	unit := renderSunshineService(desk.User, uid, cfg.InstallBin, conf, cfg)
	backend := selectedCaptureBackend(deps)
	details := map[string]any{
		"selected_backend":          backend,
		"compositor_service":        "kwin-realvt.service",
		"sunshine_service":          "sunshine-headless.service",
		"config":                    conf,
		"hevc_mode":                 cfg.HevcModeValue(),
		"av1_mode":                  cfg.Av1ModeValue(),
		"advertises_hevc_main10":    cfg.AdvertisesHEVCMain10(),
		"advertises_av1_main10":     cfg.AdvertisesAV1Main10(),
		"force_av1_hdr10_env":       cfg.ForceAV1HDR10,
		"synthesize_hdr10_metadata": cfg.SynthesizeHDR10Metadata,
	}
	if backend != "" && backend != BackendWaylandKMSNVENCHDR {
		return failPhase(deps, StreamingServicesName, details, "unsupported selected capture backend", fmt.Errorf("selected backend %s does not have a v3 production service renderer yet", backend), true)
	}
	if !deps.DryRun {
		if err := os.WriteFile("/etc/systemd/system/sunshine-headless.service", []byte(unit), 0o644); err != nil {
			return failPhase(deps, StreamingServicesName, details, "write sunshine-headless.service", err, true)
		}
		if err := writeResetStreamingHelper(deps, cfg.InstallBin, conf); err != nil {
			return failPhase(deps, StreamingServicesName, details, "write reset helper", err, true)
		}
		if err := writeSunshineWatchdog(); err != nil {
			return failPhase(deps, StreamingServicesName, details, "write watchdog", err, true)
		}
		if err := run(ctx, deps, "", []string{"systemctl", "daemon-reload"}, time.Minute, true); err != nil {
			return failPhase(deps, StreamingServicesName, details, "daemon-reload", err, true)
		}
		_ = run(ctx, deps, "", []string{"systemctl", "enable", "kwin-realvt.service", "sunshine-headless.service", "clouddeploy-watch-streaming.timer"}, time.Minute, true)
		if err := ensureKWinReady(ctx, deps, desk.User, uid); err != nil {
			return failPhase(deps, StreamingServicesName, details, "KWin not ready", err, true)
		}
		_ = run(ctx, deps, "", []string{forceKwinModeScriptPath}, 90*time.Second, true)
		ensureUinput(ctx, deps, desk.User, details)
		drm := selectedDRMDevice(deps)
		if drm == "" {
			drm = "/dev/dri/card1"
		}
		if err := verifySunshineDeviceAccess(ctx, deps, desk.User, drm, details); err != nil {
			return failPhase(deps, StreamingServicesName, details, "Sunshine device access blocked", err, true)
		}
		credsSet, credsUser, credsErr := setSunshineCredentials(ctx, deps, desk.User, cfg.InstallBin)
		details["sunshine_credentials_set"] = credsSet
		if credsUser != "" {
			details["sunshine_username"] = credsUser
		}
		if credsErr != nil {
			details["sunshine_credentials_warning"] = credsErr.Error()
			if deps.Unattended || (deps.Profile != nil && deps.Profile.Deploy.Unattended) {
				return failPhase(deps, StreamingServicesName, details, "Sunshine credentials missing", credsErr, true)
			}
		}
		if err := run(ctx, deps, "", []string{"systemctl", "restart", "sunshine-headless.service"}, time.Minute, true); err != nil {
			return failPhase(deps, StreamingServicesName, details, "start Sunshine", err, true)
		}
	}
	deps.State.MarkDone(StreamingServicesName, details)
	_ = deps.PersistState()
	return nil
}

func ensureUinput(ctx context.Context, deps *Deps, user string, details map[string]any) {
	_ = run(ctx, deps, "", []string{"modprobe", "uinput"}, time.Minute, true)
	rule := `KERNEL=="uinput", MODE="0660", GROUP="input"` + "\n"
	if !deps.DryRun {
		_ = os.WriteFile("/etc/udev/rules.d/70-clouddeploy-uinput.rules", []byte(rule), 0o644)
	}
	_ = run(ctx, deps, "", []string{"udevadm", "control", "--reload-rules"}, time.Minute, true)
	_ = run(ctx, deps, "", []string{"udevadm", "trigger", "--subsystem-match=misc", "--attr-match=name=uinput"}, time.Minute, true)
	details["uinput_exists"] = pathExists("/dev/uinput")
	details["uinput_user_writable"] = run(ctx, deps, "", []string{"runuser", "-u", user, "--", "test", "-w", "/dev/uinput"}, 15*time.Second, true) == nil
	details["uinput_rule"] = "/etc/udev/rules.d/70-clouddeploy-uinput.rules"
}

type deviceAccessProbe struct {
	Path   string
	OK     bool
	Err    string
	Stderr string
}

func verifySunshineDeviceAccess(ctx context.Context, deps *Deps, user, drm string, details map[string]any) error {
	if strings.TrimSpace(drm) == "" {
		return fmt.Errorf("selected DRM device is empty")
	}
	render := RenderNodeForCard(drm)
	probes := []struct {
		label    string
		path     string
		required bool
	}{
		{label: "drm_card", path: drm, required: true},
		{label: "drm_render", path: render, required: render != ""},
		{label: "uinput", path: "/dev/uinput", required: false},
	}
	for _, item := range probes {
		if item.path == "" {
			continue
		}
		probe := probeDeviceOpenAsUser(ctx, deps, user, item.path)
		details["device_probe_"+item.label+"_path"] = probe.Path
		details["device_probe_"+item.label+"_ok"] = probe.OK
		if probe.Err != "" {
			details["device_probe_"+item.label+"_err"] = probe.Err
		}
		if probe.Stderr != "" {
			details["device_probe_"+item.label+"_stderr"] = probe.Stderr
		}
		if item.required && !probe.OK {
			return fmt.Errorf("service user %s cannot open %s read/write: %s", user, item.path, firstNonEmpty(probe.Err, probe.Stderr, "unknown error"))
		}
	}
	details["drm_device_access_ok"] = true
	details["render_device_access_ok"] = render == "" || details["device_probe_drm_render_ok"] == true
	details["uinput_access_ok"] = details["device_probe_uinput_ok"] == true
	return nil
}

func probeDeviceOpenAsUser(ctx context.Context, deps *Deps, user, path string) deviceAccessProbe {
	probe := deviceAccessProbe{Path: path}
	if deps.DryRun {
		probe.OK = true
		return probe
	}
	script := `import os, sys
path = sys.argv[1]
fd = os.open(path, os.O_RDWR)
os.close(fd)
`
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"runuser", "-u", user, "--", "python3", "-c", script, path},
		Sudo:    true,
		Timeout: 15 * time.Second,
		LogFile: "-",
	})
	probe.OK = res.Err == nil
	if res.Err != nil {
		probe.Err = res.Err.Error()
	}
	if strings.TrimSpace(res.Stderr) != "" {
		probe.Stderr = lastLines(res.Stderr, 4)
	}
	return probe
}

type StreamValidate struct {
	ServiceActiveFn func(context.Context, *Deps) error
	ServerInfoFn    func(context.Context, *Deps, string) (string, error)
	WebUIStatusFn   func(context.Context, *Deps, string) (int, error)
	JournalFn       func(context.Context, *Deps) (string, error)
	ListenersFn     func(context.Context, *Deps) (string, error)
	RetryWindow     time.Duration
	RetryInterval   time.Duration
}

func (StreamValidate) Name() string { return StreamValidateName }

func (p StreamValidate) Run(ctx context.Context, deps *Deps) error {
	if shouldSkip(deps.State, StreamValidateName) {
		logger(deps).Info("phase stream-validate: already done; skipping")
		return nil
	}
	deps.State.MarkRunning(StreamValidateName)
	_ = deps.PersistState()
	backend := selectedCaptureBackend(deps)
	details := map[string]any{"selected_backend": backend}
	if backend != "" && backend != BackendWaylandKMSNVENCHDR {
		return failPhase(deps, StreamValidateName, details, "unsupported selected capture backend", fmt.Errorf("selected backend %s does not have a v3 stream validator yet", backend), true)
	}
	if deps.DryRun {
		details["dry_run"] = true
		deps.State.MarkDone(StreamValidateName, details)
		_ = deps.PersistState()
		return nil
	}
	serviceActive := p.ServiceActiveFn
	if serviceActive == nil {
		serviceActive = func(ctx context.Context, deps *Deps) error {
			return run(ctx, deps, "", []string{"systemctl", "is-active", "--quiet", "sunshine-headless.service"}, 15*time.Second, false)
		}
	}
	serverInfoFn := p.ServerInfoFn
	if serverInfoFn == nil {
		serverInfoFn = func(ctx context.Context, deps *Deps, url string) (string, error) {
			return output(ctx, deps, "", []string{"curl", "-fsS", "--max-time", "5", url}, 10*time.Second, false)
		}
	}
	webUIStatusFn := p.WebUIStatusFn
	if webUIStatusFn == nil {
		webUIStatusFn = webUIHTTPStatus
	}
	journalFn := p.JournalFn
	if journalFn == nil {
		journalFn = sunshineJournalSinceStart
	}
	listenersFn := p.ListenersFn
	if listenersFn == nil {
		listenersFn = func(ctx context.Context, deps *Deps) (string, error) {
			return output(ctx, deps, "", []string{"bash", "-lc", "ss -H -lntu '( sport = :47984 or sport = :47989 or sport = :47990 or sport = :47998 or sport = :47999 or sport = :48000 )' 2>/dev/null || true"}, 10*time.Second, false)
		}
	}
	if err := serviceActive(ctx, deps); err != nil {
		return failPhase(deps, StreamValidateName, details, "Sunshine service inactive", err, true)
	}
	listeners, _ := listenersFn(ctx, deps)
	details["listeners"] = strings.TrimSpace(listeners)
	retryWindow := p.RetryWindow
	if retryWindow <= 0 {
		retryWindow = 60 * time.Second
	}
	retryInterval := p.RetryInterval
	if retryInterval <= 0 {
		retryInterval = 2 * time.Second
	}
	serverInfo, err := waitForServerInfo(ctx, deps, serverInfoFn, serviceActive, listenersFn, journalFn, "http://127.0.0.1:47989/serverinfo", details, retryWindow, retryInterval)
	if err != nil {
		return failPhase(deps, StreamValidateName, details, "Sunshine serverinfo unreachable", err, true)
	}
	details["serverinfo_bytes"] = len(serverInfo)
	codecRaw, codecOK := sunshinecodec.ServerInfoInt(serverInfo, "ServerCodecModeSupport")
	maxLumaHEVC, maxLumaOK := sunshinecodec.ServerInfoInt(serverInfo, "MaxLumaPixelsHEVC")
	codecSupport := sunshinecodec.DecodeCodecModeSupport(codecRaw)
	details["serverinfo_codec_mode_support"] = codecRaw
	details["serverinfo_codec_mode_support_present"] = codecOK
	details["serverinfo_codec_flags"] = codecSupport.Names()
	hevcMain10Ready := codecOK && codecSupport.HEVCMain10 && maxLumaOK && maxLumaHEVC > 0
	av1Main10Ready := codecOK && codecSupport.AV1Main10
	details["serverinfo_hevc_main10"] = codecOK && codecSupport.HEVCMain10
	details["serverinfo_av1_main10"] = av1Main10Ready
	details["serverinfo_hevc_main10_ready"] = hevcMain10Ready
	details["serverinfo_av1_main10_ready"] = av1Main10Ready
	details["serverinfo_hdr_main10_ready"] = av1Main10Ready || hevcMain10Ready
	details["serverinfo_max_luma_pixels_hevc"] = maxLumaHEVC
	details["serverinfo_max_luma_pixels_hevc_present"] = maxLumaOK
	if deps.Profile != nil && deps.Profile.Display.HDR {
		if !codecOK {
			return failPhase(deps, StreamValidateName, details, "Sunshine serverinfo missing codec support", fmt.Errorf("ServerCodecModeSupport missing from /serverinfo"), true)
		}
		// AV1 Main10 is preferred when the GPU exposes NVENC AV1, but
		// Ampere/A5000/A6000-class GPUs only provide the HEVC Main10 HDR
		// path. Treat either advertised AV1 Main10 OR advertised HEVC
		// Main10 with nonzero MaxLumaPixelsHEVC as sufficient for HDR.
		// The separate H.264+HDR/p010 log gate below remains fatal.
		if !av1Main10Ready && !hevcMain10Ready {
			return failPhase(deps, StreamValidateName, details, "Sunshine serverinfo lacks HDR Main10 codec path", fmt.Errorf("ServerCodecModeSupport=%d (%s), MaxLumaPixelsHEVC=%d present=%v. Need AV1 Main10 or HEVC Main10 with nonzero HEVC luma capacity; H.264 HDR/p010 is invalid.", codecRaw, codecSupport.String(), maxLumaHEVC, maxLumaOK), true)
		}
		if !av1Main10Ready && hevcMain10Ready {
			details["serverinfo_hdr_codec_fallback"] = "hevc-main10"
		} else if av1Main10Ready {
			details["serverinfo_hdr_codec_primary"] = "av1-main10"
		}
	}
	webStatus, webErr := webUIStatusFn(ctx, deps, "https://127.0.0.1:47990")
	details["web_ui_http_status"] = webStatus
	details["web_ui_reachable"] = webStatusSuccess(webStatus)
	if webErr != nil {
		details["web_ui_error"] = webErr.Error()
	}
	if webStatusSuccess(webStatus) {
		buildPhase := deps.State.Get(SunshineBuildName)
		if buildPhase != nil && buildPhase.Details != nil {
			if b, ok := buildPhase.Details["assets_web_index"].(bool); ok && !b {
				details["web_asset_mode"] = "embedded"
			}
		} else {
			details["web_asset_mode"] = "embedded"
		}
	}
	if ip := tailscaleIPFromState(deps); ip != "" {
		ts, terr := serverInfoFn(ctx, deps, "http://"+ip+":47989/serverinfo")
		details["tailscale_serverinfo_ok"] = terr == nil
		details["tailscale_serverinfo_bytes"] = len(ts)
	}
	logs, _ := journalFn(ctx, deps)
	ev := parseSunshineStreamEvidence(logs)
	details["kms_marker"] = ev.KMS
	details["nvenc_marker"] = ev.NVENC
	details["av1_marker"] = ev.AV1
	details["hevc_marker"] = ev.HEVC
	details["resolution_marker"] = ev.Resolution4K
	details["selected_capture_line"] = ev.SelectedCaptureLine
	details["selected_encoder_line"] = ev.SelectedEncoderLine
	details["encode_selection_line"] = ev.EncodeSelectionLine
	details["selected_codec"] = ev.SelectedCodec
	details["video_format"] = ev.VideoFormat
	details["client_dynamic_range"] = ev.ClientDynamicRange
	details["hdr_line"] = ev.HDRLine
	details["cuda_interop_warning"] = ev.CUDAInteropWarning
	details["av1_available"] = ev.AV1
	details["hevc_hdr_available"] = ev.HEVC && ev.HDR && ev.ColorDepth10
	if ev.HEVC && ev.HDR && ev.ColorDepth10 {
		details["selected_hdr_codec"] = "hevc"
	} else if ev.AV1 && ev.HDR && ev.ColorDepth10 {
		details["selected_hdr_codec"] = "av1"
	}
	if ev.Fatal {
		return failPhase(deps, StreamValidateName, details, "fatal Sunshine encoder/display log marker", fmt.Errorf("Sunshine fatal marker in journal"), true)
	}
	if ev.InvalidH264HDR || ev.H264DynamicRangeUnsupported {
		return failPhase(deps, StreamValidateName, details, "invalid Sunshine H.264 HDR encode selection", fmt.Errorf("H.264 session selected HDR/10-bit/p010 or h264_nvenc rejected dynamic range: %s", ev.EncodeSelectionLine), true)
	}
	if ev.SelectedCaptureLine == "" {
		deps.State.MarkPendingMoonlightConnect(StreamValidateName, "Sunshine serverinfo is reachable; waiting for Moonlight stream attempt to produce KMS/NVENC markers")
		deps.State.Get(StreamValidateName).Details = details
		_ = deps.PersistState()
		return nil
	}
	drm := selectedDRMDevice(deps)
	if drm == "" {
		drm = "/dev/dri/card1"
	}
	connector := "DP-1"
	width, height := "3840", "2160"
	if deps.Profile != nil {
		connector = deps.Profile.Display.ForcedConnector
		w, h := ParseResolution(deps.Profile.Display.Resolution)
		if w > 0 && h > 0 {
			width, height = strconv.Itoa(w), strconv.Itoa(h)
		}
	}
	if err := validateSunshineSelectedCapture(ev.SelectedCaptureLine, drm, connector, width, height); err != nil {
		return failPhase(deps, StreamValidateName, details, "wrong Sunshine KMS capture target", err, true)
	}
	if !ev.KMS || !ev.Resolution4K || !ev.NVENC || !ev.HEVC {
		return failPhase(deps, StreamValidateName, details, "missing required Sunshine KMS/NVENC markers", fmt.Errorf("kms=%v resolution4k=%v nvenc=%v hevc=%v", ev.KMS, ev.Resolution4K, ev.NVENC, ev.HEVC), true)
	}
	if deps.Profile != nil && deps.Profile.Display.HDR {
		if !ev.HDR || !ev.ColorDepth10 || !ev.P010 {
			return failPhase(deps, StreamValidateName, details, "missing required Sunshine HDR Main10 markers", fmt.Errorf("hdr=%v color_depth_10=%v p010=%v", ev.HDR, ev.ColorDepth10, ev.P010), true)
		}
	}
	deps.State.MarkDone(StreamValidateName, details)
	_ = deps.PersistState()
	return nil
}

type OptionalApps struct{}

func (OptionalApps) Name() string { return OptionalAppsName }

func (p OptionalApps) Run(ctx context.Context, deps *Deps) error {
	if shouldSkip(deps.State, OptionalAppsName) {
		return nil
	}
	deps.State.MarkRunning(OptionalAppsName)
	_ = deps.PersistState()
	details := map[string]any{"requested": deps.Profile.Deploy.OptionalApps}
	if !deps.Profile.Deploy.OptionalApps {
		deps.State.MarkSkipped(OptionalAppsName, "deploy.optional_apps=false")
		deps.State.Get(OptionalAppsName).Details = details
		_ = deps.PersistState()
		return nil
	}
	pkgs := []string{"flatpak", "steam-installer", "wine64", "winetricks"}
	err := deps.APT.Run(ctx, func(tc *apt.TxContext) error { return tc.Install(ctx, pkgs) })
	details["apt_packages"] = pkgs
	if err != nil {
		deps.State.MarkFailed(OptionalAppsName, "optional app apt install failed", err, false)
		deps.State.Get(OptionalAppsName).Details = details
		_ = deps.PersistState()
		return nil
	}
	_ = run(ctx, deps, "", []string{"flatpak", "remote-add", "--if-not-exists", "flathub", "https://flathub.org/repo/flathub.flatpakrepo"}, 2*time.Minute, false)
	for _, app := range []string{"com.heroicgameslauncher.hgl", "net.lutris.Lutris", "com.usebottles.bottles", "org.prismlauncher.PrismLauncher", "net.davidotek.pupgui2"} {
		_ = run(ctx, deps, "", []string{"flatpak", "install", "-y", "flathub", app}, 20*time.Minute, false)
	}
	deps.State.MarkDone(OptionalAppsName, details)
	_ = deps.PersistState()
	return nil
}

func renderSunshineService(user, uid, bin, conf string, cfg config.SunshineConfig) string {
	if bin == "" {
		bin = "/usr/local/bin/sunshine-clouddeploy"
	}
	// Gate the HDR-force env vars on the profile.
	//
	// Live-VM bug: v3 unconditionally injected
	//   SUNSHINE_FORCE_AV1_HDR10=1
	//   SUNSHINE_SYNTHESIZE_HDR10_METADATA=1
	// even when the session ultimately negotiated H.264 - h264_nvenc
	// then refused the p010 / 10-bit / Rec.2020 colorspace the
	// force-HDR env vars demanded ("dynamic range not supported").
	// Now we render those lines only when the profile asks for them;
	// the SunshineConfig validator already requires both fields to
	// be true for HDR profiles AND requires hevc_mode>=3, so
	// Ampere-class GPUs still offer a valid HEVC Main10 HDR fallback.
	// AV1 Main10 remains preferred when the GPU/encoder advertises it.
	hdrEnvBlock := ""
	if cfg.ForceAV1HDR10 {
		hdrEnvBlock += "Environment=SUNSHINE_FORCE_AV1_HDR10=1\n"
	}
	if cfg.SynthesizeHDR10Metadata {
		hdrEnvBlock += "Environment=SUNSHINE_SYNTHESIZE_HDR10_METADATA=1\n"
	}
	return fmt.Sprintf(`[Unit]
Description=CloudDeploy Sunshine Wayland/KMS/NVENC
Wants=network-online.target kwin-realvt.service
After=network-online.target kwin-realvt.service

[Service]
User=%s
Group=%s
SupplementaryGroups=video render input audio
AmbientCapabilities=CAP_SYS_ADMIN CAP_SYS_NICE
CapabilityBoundingSet=CAP_SYS_ADMIN CAP_SYS_NICE CAP_NET_BIND_SERVICE
NoNewPrivileges=false
WorkingDirectory=/home/%s
Environment=HOME=/home/%s
Environment=USER=%s
Environment=LOGNAME=%s
Environment=XDG_RUNTIME_DIR=/run/user/%s
Environment=WAYLAND_DISPLAY=wayland-0
Environment=DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/%s/bus
Environment=SUNSHINE_STREAM_DIAG_REUSE_AUDIO_PEER=1
Environment=SUNSHINE_STREAM_DIAG_VIDEO_PEER_MODE=rtsp-client-port
Environment=SUNSHINE_STREAM_DIAG_IGNORE_CONTROL_TIMEOUT=1
Environment=SUNSHINE_STREAM_DIAG_FORCE_ANNOUNCE_SUCCESS=1
Environment=SUNSHINE_STREAM_DIAG_FORCE_ANNOUNCE_SUCCESS_IMMEDIATE=1
%sExecStart=%s %s
Restart=on-failure
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`, user, user, user, user, user, user, uid, uid, hdrEnvBlock, bin, conf)
}

func writeResetStreamingHelper(deps *Deps, bin, conf string) error {
	_ = bin
	_ = conf
	body := `#!/usr/bin/env bash
set -euo pipefail
systemctl stop sunshine-headless.service kwin-realvt.service 2>/dev/null || true
rm -f /run/user/*/wayland-0 /run/user/*/wayland-0.lock 2>/dev/null || true
systemctl start --no-block kwin-realvt.service
sleep 8
/usr/local/bin/clouddeploy-force-kwin-mode.sh || true
systemctl restart sunshine-headless.service
systemctl --no-pager --full status kwin-realvt.service sunshine-headless.service
`
	return os.WriteFile("/usr/local/bin/clouddeploy-reset-streaming", []byte(body), 0o755)
}

func writeSunshineWatchdog() error {
	svc := `[Unit]
Description=CloudDeploy Sunshine watchdog

[Service]
Type=oneshot
ExecStart=/bin/systemctl restart sunshine-headless.service
`
	timer := `[Unit]
Description=CloudDeploy Sunshine watchdog timer

[Timer]
OnBootSec=2min
OnUnitInactiveSec=5min
Unit=clouddeploy-watch-streaming.service

[Install]
WantedBy=timers.target
`
	if err := os.WriteFile("/etc/systemd/system/clouddeploy-watch-streaming.service", []byte(svc), 0o644); err != nil {
		return err
	}
	return os.WriteFile("/etc/systemd/system/clouddeploy-watch-streaming.timer", []byte(timer), 0o644)
}

func waitForServerInfo(
	ctx context.Context,
	deps *Deps,
	serverInfoFn func(context.Context, *Deps, string) (string, error),
	serviceActiveFn func(context.Context, *Deps) error,
	listenersFn func(context.Context, *Deps) (string, error),
	journalFn func(context.Context, *Deps) (string, error),
	url string,
	details map[string]any,
	window time.Duration,
	interval time.Duration,
) (string, error) {
	if window <= 0 {
		window = 60 * time.Second
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	deadline := time.Now().Add(window)
	attempts := 0
	var lastErr error
	for {
		attempts++
		body, err := serverInfoFn(ctx, deps, url)
		if err == nil {
			details["serverinfo_attempts"] = attempts
			details["serverinfo_retry_window_ms"] = window.Milliseconds()
			return body, nil
		}
		lastErr = err
		details["serverinfo_last_error"] = err.Error()
		if serviceActiveFn != nil {
			details["sunshine_active_during_retry"] = serviceActiveFn(ctx, deps) == nil
		}
		if listenersFn != nil {
			if listeners, lerr := listenersFn(ctx, deps); lerr == nil {
				details["listeners_during_retry"] = strings.TrimSpace(listeners)
			}
		}
		if journalFn != nil {
			if logs, jerr := journalFn(ctx, deps); jerr == nil {
				details["journal_tail_during_retry"] = lastLines(logs, 50)
			}
		}
		if time.Now().Add(interval).After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			details["serverinfo_attempts"] = attempts
			return "", ctx.Err()
		case <-time.After(interval):
		}
	}
	details["serverinfo_attempts"] = attempts
	return "", fmt.Errorf("%s not reachable after %s: %w", url, window, lastErr)
}

func webUIHTTPStatus(ctx context.Context, deps *Deps, url string) (int, error) {
	out, err := output(ctx, deps, "", []string{"curl", "-k", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "5", url}, 10*time.Second, false)
	if err != nil {
		return 0, err
	}
	code, _ := strconv.Atoi(strings.TrimSpace(out))
	return code, nil
}

func webStatusSuccess(code int) bool {
	switch code {
	case 200, 301, 302, 307, 401, 403:
		return true
	default:
		return false
	}
}

func sunshineJournalSinceStart(ctx context.Context, deps *Deps) (string, error) {
	ts, _ := output(ctx, deps, "", []string{"systemctl", "show", "sunshine-headless.service", "-p", "ActiveEnterTimestamp", "--value"}, 10*time.Second, false)
	ts = strings.TrimSpace(ts)
	if ts != "" && !strings.EqualFold(ts, "n/a") {
		if logs, err := output(ctx, deps, "", []string{"journalctl", "-u", "sunshine-headless.service", "--since", ts, "-n", "800", "--no-pager"}, 15*time.Second, false); err == nil {
			return logs, nil
		}
	}
	return output(ctx, deps, "", []string{"journalctl", "-u", "sunshine-headless.service", "-n", "500", "--no-pager"}, 15*time.Second, false)
}

type sunshineStreamEvidence struct {
	KMS                         bool
	NVENC                       bool
	HEVC                        bool
	AV1                         bool
	Resolution4K                bool
	HDR                         bool
	ColorDepth10                bool
	P010                        bool
	Fatal                       bool
	CUDAInteropWarning          bool
	InvalidH264HDR              bool
	H264DynamicRangeUnsupported bool
	SelectedCaptureLine         string
	SelectedEncoderLine         string
	EncodeSelectionLine         string
	SelectedCodec               string
	VideoFormat                 string
	ClientDynamicRange          string
	HDRLine                     string
}

func parseSunshineStreamEvidence(logs string) sunshineStreamEvidence {
	var ev sunshineStreamEvidence
	for _, raw := range strings.Split(logs, "\n") {
		line := strings.TrimSpace(raw)
		lower := strings.ToLower(line)
		if line == "" {
			continue
		}
		if strings.Contains(line, "STREAM_DIAG kms capture selected") || strings.Contains(line, "kms capture selected") {
			ev.SelectedCaptureLine = line
		}
		if strings.Contains(line, "Encode selection:") {
			ev.EncodeSelectionLine = line
			ev.SelectedCodec = parseLogKV(line, "codec")
			ev.VideoFormat = parseLogKV(line, "videoFormat")
			ev.ClientDynamicRange = parseLogKV(line, "client_dynamicRange")
			if strings.Contains(lower, "codec=h.264") &&
				(strings.Contains(lower, "selected_colorspace=hdr") ||
					strings.Contains(lower, "rec. 2020") ||
					strings.Contains(lower, "smpte 2084") ||
					strings.Contains(lower, "selected_bit_depth=10-bit") ||
					strings.Contains(lower, "selected_pix_fmt=p010")) {
				ev.InvalidH264HDR = true
			}
		}
		if strings.Contains(line, "Found monitor for DRM screencasting") || strings.Contains(line, "Screencasting with KMS") {
			ev.KMS = true
		}
		if strings.Contains(line, "Resolution: 3840x2160") || strings.Contains(line, "Desktop resolution: 3840x2160") || strings.Contains(line, "3840x2160") {
			ev.Resolution4K = true
		}
		if strings.Contains(lower, "hevc_nvenc") || strings.Contains(line, "HEVC NVENC initialized") {
			ev.HEVC = true
			ev.NVENC = true
			ev.SelectedEncoderLine = line
		}
		if strings.Contains(lower, "av1_nvenc") {
			ev.AV1 = true
			ev.NVENC = true
			ev.SelectedEncoderLine = line
		}
		if strings.Contains(lower, "h264_nvenc") || strings.Contains(line, "Nvenc initialized") || strings.Contains(line, "NVENC initialized") {
			ev.NVENC = true
			if ev.SelectedEncoderLine == "" {
				ev.SelectedEncoderLine = line
			}
		}
		if strings.Contains(line, "Color coding: HDR (Rec. 2020 + SMPTE 2084 PQ)") ||
			strings.Contains(line, "selected_colorspace=HDR (Rec. 2020 + SMPTE 2084 PQ)") ||
			strings.Contains(line, "NV_INPUT_COLORSPACE=BT.2100 PQ") ||
			strings.Contains(line, "is_hdr: NVIDIA private HDR") {
			ev.HDR = true
			ev.HDRLine = line
		}
		if strings.Contains(line, "Color depth: 10-bit") || strings.Contains(line, "color_depth=10") {
			ev.ColorDepth10 = true
		}
		if strings.Contains(lower, "selected_pix_fmt=p010") || strings.Contains(lower, "pix_fmt=p010") {
			ev.P010 = true
		}
		if strings.Contains(line, "Attempting to use NVENC without CUDA support") {
			ev.CUDAInteropWarning = true
		}
		if strings.Contains(lower, "h264_nvenc: dynamic range not supported") {
			ev.H264DynamicRangeUnsupported = true
		}
		if strings.Contains(line, "Fatal: Unable to find display or encoder") ||
			strings.Contains(line, "Couldn't find any working encoder") ||
			strings.Contains(line, "Encoder [nvenc] failed") {
			ev.Fatal = true
		}
	}
	return ev
}

func parseLogKV(line, key string) string {
	idx := strings.Index(line, key+"=")
	if idx < 0 {
		return ""
	}
	rest := line[idx+len(key)+1:]
	if strings.HasPrefix(rest, "\"") {
		rest = strings.TrimPrefix(rest, "\"")
		if end := strings.Index(rest, "\""); end >= 0 {
			return rest[:end]
		}
		return rest
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return strings.TrimSpace(rest)
	}
	val := strings.Trim(fields[0], ",)")
	if key == "codec" && len(fields) > 1 && strings.HasPrefix(fields[1], "(") {
		return val
	}
	return val
}

func validateSunshineSelectedCapture(line, drm, connector, width, height string) error {
	if strings.Contains(line, "/dev/dri/card0") && drm != "/dev/dri/card0" {
		return fmt.Errorf("Sunshine selected wrong DRM card: %s", line)
	}
	for _, want := range []string{drm, "connector=" + connector, "width=" + width, "height=" + height} {
		if strings.TrimSpace(want) == "" {
			continue
		}
		if !strings.Contains(line, want) {
			return fmt.Errorf("Sunshine selected capture line missing %q: %s", want, line)
		}
	}
	if strings.Contains(line, "Virtual-1") || strings.Contains(line, "1024x768") || strings.Contains(line, "width=1024") || strings.Contains(line, "height=768") {
		return fmt.Errorf("Sunshine selected virtual/low-res capture target: %s", line)
	}
	return nil
}

func setSunshineCredentials(ctx context.Context, deps *Deps, user, bin string) (bool, string, error) {
	sunUser := strings.TrimSpace(os.Getenv("SUNSHINE_USER"))
	pass := os.Getenv("SUNSHINE_PASS")
	if sunUser == "" {
		sunUser = user
	}
	if strings.TrimSpace(pass) == "" {
		return false, sunUser, fmt.Errorf("SUNSHINE_PASS not set; Web UI will prompt for initial credentials")
	}
	if strings.TrimSpace(bin) == "" {
		bin = "/usr/local/bin/sunshine-clouddeploy"
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:       sunshineCredsArgs(user, bin, sunUser, pass),
		Sudo:       true,
		Timeout:    time.Minute,
		DryRun:     deps.DryRun,
		LogFile:    "-",
		RedactArgs: []int{-1},
	})
	if res.Err != nil {
		return false, sunUser, fmt.Errorf("sunshine --creds failed: %w stderr=%q", res.Err, lastLines(res.Stderr, 4))
	}
	if !deps.DryRun && !sunshineCredentialsEvidence("/home/"+user) {
		return false, sunUser, fmt.Errorf("sunshine --creds exited successfully but no sunshine_state.json or credentials files were found")
	}
	return true, sunUser, nil
}

func sunshineCredsArgs(user, bin, sunUser, pass string) []string {
	return []string{
		"runuser", "-u", user, "--", "env",
		"HOME=/home/" + user,
		bin, "--creds", sunUser, pass,
	}
}

func sunshineCredentialsEvidence(home string) bool {
	base := filepath.Join(home, ".config", "sunshine")
	if pathExists(filepath.Join(base, "sunshine_state.json")) {
		return true
	}
	creds := filepath.Join(base, "credentials")
	entries, err := os.ReadDir(creds)
	return err == nil && len(entries) > 0
}

func ensureKWinReady(ctx context.Context, deps *Deps, user, uid string) error {
	if err := run(ctx, deps, "", []string{"systemctl", "is-active", "--quiet", "kwin-realvt.service"}, 15*time.Second, false); err != nil {
		return err
	}
	qdbus, qerr := resolveQDBus()
	if qerr != nil {
		return qerr
	}
	support, err := outputAsDesktop(ctx, deps, user, uid, []string{qdbus.Path, "--session", "org.kde.KWin", "/KWin", "org.kde.KWin.supportInformation"}, 10*time.Second)
	if err != nil {
		return err
	}
	backend := parseKWinOutputBackend(support)
	if isNestedKWinBackend(backend) {
		return fmt.Errorf("KWin is reachable through %s but backend is %s, not DRM", qdbus.Path, backend)
	}
	return nil
}

func failPhase(deps *Deps, name string, details map[string]any, reason string, err error, fatal bool) error {
	if details == nil {
		details = map[string]any{}
	}
	if err != nil {
		details["err"] = err.Error()
	}
	deps.State.MarkFailed(name, reason, err, fatal)
	deps.State.Get(name).Details = details
	_ = deps.PersistState()
	if fatal {
		if err == nil {
			return fmt.Errorf("phase %s: %s", name, reason)
		}
		return fmt.Errorf("phase %s: %s: %w", name, reason, err)
	}
	return nil
}

func logger(deps *Deps) *slog.Logger {
	if deps != nil && deps.Logger != nil {
		return deps.Logger
	}
	return slog.Default()
}

func selectedDRMDevice(deps *Deps) string {
	if p := deps.State.Get(KWinSessionName); p != nil && p.Details != nil {
		if v, ok := p.Details["selected_drm_device"].(string); ok {
			return strings.TrimSpace(v)
		}
		if v, ok := p.Details["kwin_drm_device_resolved"].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func selectedCaptureBackend(deps *Deps) string {
	if p := deps.State.Get(GPUCaptureCapabilityProbeName); p != nil && p.Details != nil {
		if v, ok := p.Details["selected_backend"].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func tailscaleIPFromState(deps *Deps) string {
	if p := deps.State.Get(TailscaleName); p != nil && p.Details != nil {
		if v, ok := p.Details["tailscale_ip"].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func sunshineConfigPath(deps *Deps) string {
	desk := deps.Profile.EffectiveDesktop()
	cfg := deps.Profile.EffectiveSunshine()
	if strings.TrimSpace(cfg.ConfigPath) != "" {
		return strings.TrimSpace(cfg.ConfigPath)
	}
	return "/home/" + desk.User + "/" + defaultSunshineConfRel
}

func pathExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func updateSunshineCSRF(ctx context.Context, deps *Deps, ip string) {
	path := sunshineConfigPath(deps)
	if path == "" || ip == "" || deps.DryRun {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	want := "csrf_allowed_origins = " + strings.Join(sunshineCSRFOrigins(deps, ip), ",")
	found := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "csrf_allowed_origins") {
			lines[i] = want
			found = true
		}
	}
	if !found {
		lines = append(lines, want)
	}
	_ = os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

func sunshineCSRFOrigins(deps *Deps, ips ...string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 4)
	add := func(origin string) {
		origin = strings.TrimSpace(origin)
		if origin == "" || seen[origin] {
			return
		}
		seen[origin] = true
		out = append(out, origin)
	}
	add("https://localhost:47990")
	add("https://127.0.0.1:47990")
	for _, ip := range ips {
		ip = strings.TrimSpace(ip)
		if ip != "" {
			add("https://" + ip + ":47990")
		}
	}
	if publicIP := strings.TrimSpace(os.Getenv("CLOUDDEPLOY_PUBLIC_IP")); publicIP != "" {
		add("https://" + publicIP + ":47990")
	}
	if deps != nil && deps.Profile != nil {
		for _, origin := range deps.Profile.EffectiveSunshine().ExtraCSRFAllowedOrigins {
			add(origin)
		}
	}
	return out
}

func runAsDesktop(ctx context.Context, deps *Deps, user, uid string, argv []string, timeout time.Duration) error {
	args := append([]string{"runuser", "-u", user, "--", "env",
		"XDG_RUNTIME_DIR=/run/user/" + uid,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + uid + "/bus",
		"WAYLAND_DISPLAY=wayland-0",
		"QT_QPA_PLATFORM=wayland",
		"XDG_CURRENT_DESKTOP=KDE",
		"XDG_SESSION_TYPE=wayland"}, argv...)
	return run(ctx, deps, "", args, timeout, true)
}

func outputAsDesktop(ctx context.Context, deps *Deps, user, uid string, argv []string, timeout time.Duration) (string, error) {
	args := append([]string{"runuser", "-u", user, "--", "env",
		"XDG_RUNTIME_DIR=/run/user/" + uid,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + uid + "/bus",
		"WAYLAND_DISPLAY=wayland-0",
		"QT_QPA_PLATFORM=wayland",
		"XDG_CURRENT_DESKTOP=KDE",
		"XDG_SESSION_TYPE=wayland"}, argv...)
	return output(ctx, deps, "", args, timeout, true)
}

func output(ctx context.Context, deps *Deps, cwd string, argv []string, timeout time.Duration, sudo bool) (string, error) {
	res := deps.Runner.Exec(ctx, runner.CommandSpec{Argv: argv, Cwd: cwd, Sudo: sudo, Timeout: timeout, DryRun: deps.DryRun, LogFile: "-"})
	if res.Err != nil {
		return res.Stdout, fmt.Errorf("%s failed: %w stderr=%q", strings.Join(argv, " "), res.Err, lastLines(res.Stderr, 5))
	}
	return res.Stdout, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func containsAny(s string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
