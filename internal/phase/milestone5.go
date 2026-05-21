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
	for _, bin := range []string{cfg.InstallBin, "/usr/local/bin/sunshine"} {
		if err := run(ctx, deps, "", []string{"setcap", "cap_sys_admin,cap_net_bind_service,cap_sys_nice+ep", bin}, time.Minute, true); err != nil {
			return failPhase(deps, SunshineBuildName, details, "set Sunshine capabilities", err, true)
		}
	}
	if err := installSunshineAssets(ctx, deps, cfg.BuildDir); err != nil {
		return failPhase(deps, SunshineBuildName, details, "install Sunshine runtime assets", err, true)
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

func installSunshineAssets(ctx context.Context, deps *Deps, buildDir string) error {
	src := filepath.Join(buildDir, "build", "assets")
	if err := run(ctx, deps, "", []string{"install", "-d", "-m", "0755", "/usr/local/assets"}, time.Minute, true); err != nil {
		return err
	}
	if err := run(ctx, deps, "", []string{"bash", "-lc", "cp -a " + shellQuote(src) + "/. /usr/local/assets/"}, 5*time.Minute, true); err != nil {
		return err
	}
	_ = run(ctx, deps, "", []string{"bash", "-lc", "if [ -d /usr/share/sunshine/web ]; then mkdir -p /usr/local/assets/web && cp -a /usr/share/sunshine/web/. /usr/local/assets/web/; fi"}, 2*time.Minute, true)
	if _, err := os.Stat("/usr/local/assets/apps.json"); err != nil {
		return fmt.Errorf("/usr/local/assets/apps.json missing after asset install")
	}
	if _, err := os.Stat("/usr/local/assets/web/index.html"); err != nil {
		return fmt.Errorf("/usr/local/assets/web/index.html missing after asset install")
	}
	return nil
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
	av1Mode := "2"
	hevcMode := "0"
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

type Tailscale struct{}

func (Tailscale) Name() string { return TailscaleName }

func (p Tailscale) Run(ctx context.Context, deps *Deps) error {
	log := logger(deps)
	if shouldSkip(deps.State, TailscaleName) {
		log.Info("phase tailscale: already done; skipping")
		return nil
	}
	deps.State.MarkRunning(TailscaleName)
	_ = deps.PersistState()
	cfg := deps.Profile.EffectiveTailscale()
	details := map[string]any{"enabled": cfg.EnabledValue(), "authkey_env": cfg.AuthKeyEnv}
	if !cfg.EnabledValue() {
		deps.State.MarkSkipped(TailscaleName, "tailscale.enabled=false")
		deps.State.Get(TailscaleName).Details = details
		_ = deps.PersistState()
		return nil
	}
	key := strings.TrimSpace(os.Getenv(cfg.AuthKeyEnv))
	if key == "" {
		details["authkey_present"] = false
		deps.State.MarkSkipped(TailscaleName, "no Tailscale auth key in environment; continuing without Tailscale")
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
	ip, _ := output(ctx, deps, "", []string{"tailscale", "ip", "-4"}, 15*time.Second, false)
	ip = strings.TrimSpace(strings.Split(strings.TrimSpace(ip), "\n")[0])
	if deps.DryRun && ip == "" {
		ip = "100.64.0.1"
	}
	details["tailscale_ip"] = ip
	if ip == "" {
		return failPhase(deps, TailscaleName, details, "tailscale ip missing", fmt.Errorf("tailscale up returned success but `tailscale ip -4` returned no address"), true)
	}
	if ip != "" {
		updateSunshineCSRF(ctx, deps, ip)
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
	details["wpctl_seen"] = strings.TrimSpace(wpctl) != ""
	details["pactl_sinks"] = strings.TrimSpace(pactl)
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
	unit := renderSunshineService(desk.User, uid, cfg.InstallBin, conf)
	details := map[string]any{"compositor_service": "kwin-realvt.service", "sunshine_service": "sunshine-headless.service", "config": conf}
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
		credsSet, credsUser, credsErr := setSunshineCredentials(ctx, deps, desk.User, cfg.InstallBin)
		details["sunshine_credentials_set"] = credsSet
		if credsUser != "" {
			details["sunshine_username"] = credsUser
		}
		if credsErr != nil {
			details["sunshine_credentials_warning"] = credsErr.Error()
		}
		if err := run(ctx, deps, "", []string{"systemctl", "restart", "sunshine-headless.service"}, time.Minute, true); err != nil {
			return failPhase(deps, StreamingServicesName, details, "start Sunshine", err, true)
		}
	}
	deps.State.MarkDone(StreamingServicesName, details)
	_ = deps.PersistState()
	return nil
}

type StreamValidate struct {
	ServiceActiveFn func(context.Context, *Deps) error
	ServerInfoFn    func(context.Context, *Deps, string) (string, error)
	JournalFn       func(context.Context, *Deps) (string, error)
	ListenersFn     func(context.Context, *Deps) (string, error)
}

func (StreamValidate) Name() string { return StreamValidateName }

func (p StreamValidate) Run(ctx context.Context, deps *Deps) error {
	if shouldSkip(deps.State, StreamValidateName) {
		logger(deps).Info("phase stream-validate: already done; skipping")
		return nil
	}
	deps.State.MarkRunning(StreamValidateName)
	_ = deps.PersistState()
	details := map[string]any{}
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
	journalFn := p.JournalFn
	if journalFn == nil {
		journalFn = func(ctx context.Context, deps *Deps) (string, error) {
			return output(ctx, deps, "", []string{"journalctl", "-u", "sunshine-headless.service", "-n", "500", "--no-pager"}, 15*time.Second, false)
		}
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
	serverInfo, err := serverInfoFn(ctx, deps, "http://127.0.0.1:47989/serverinfo")
	if err != nil {
		return failPhase(deps, StreamValidateName, details, "Sunshine serverinfo unreachable", err, true)
	}
	details["serverinfo_bytes"] = len(serverInfo)
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
	details["hdr_line"] = ev.HDRLine
	details["cuda_interop_warning"] = ev.CUDAInteropWarning
	if ev.Fatal {
		return failPhase(deps, StreamValidateName, details, "fatal Sunshine encoder/display log marker", fmt.Errorf("Sunshine fatal marker in journal"), true)
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

func renderSunshineService(user, uid, bin, conf string) string {
	if bin == "" {
		bin = "/usr/local/bin/sunshine-clouddeploy"
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
Environment=SUNSHINE_FORCE_AV1_HDR10=1
Environment=SUNSHINE_SYNTHESIZE_HDR10_METADATA=1
ExecStart=%s %s
Restart=on-failure
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`, user, user, user, user, user, user, uid, uid, bin, conf)
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

type sunshineStreamEvidence struct {
	KMS                 bool
	NVENC               bool
	HEVC                bool
	AV1                 bool
	Resolution4K        bool
	HDR                 bool
	ColorDepth10        bool
	P010                bool
	Fatal               bool
	CUDAInteropWarning  bool
	SelectedCaptureLine string
	SelectedEncoderLine string
	HDRLine             string
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
		if strings.Contains(line, "Fatal: Unable to find display or encoder") ||
			strings.Contains(line, "Couldn't find any working encoder") ||
			strings.Contains(line, "Encoder [nvenc] failed") {
			ev.Fatal = true
		}
	}
	return ev
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
		Argv: []string{
			"runuser", "-u", user, "--", "env",
			"HOME=/home/" + user,
			bin, "--creds", sunUser, pass,
		},
		Sudo:       true,
		Timeout:    time.Minute,
		DryRun:     deps.DryRun,
		LogFile:    "-",
		RedactArgs: []int{-1},
	})
	if res.Err != nil {
		return false, sunUser, fmt.Errorf("sunshine --creds failed: %w stderr=%q", res.Err, lastLines(res.Stderr, 4))
	}
	return true, sunUser, nil
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
