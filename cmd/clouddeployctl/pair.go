package main

// pair.go is the one-command Moonlight pairing helper. After a deploy
// finishes, the operator runs `clouddeployctl pair` (the login MOTD and
// the apply banner both point at it). It:
//
//   - prints the Tailscale IP to type into Moonlight,
//   - prompts for the 4-digit PIN Moonlight displays,
//   - submits it to Sunshine's local /api/pin with the Web UI creds
//     loaded from /etc/clouddeploy/secrets.env,
//   - retries on a wrong/early PIN.
//
// No raw curl / bash from the operator. The HTTP POST goes through Go's
// net/http with Basic auth so the password never lands in argv, the
// process list, or the command log.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/phase"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/secrets"
)

// sunshineLocalPinURL is the localhost Web UI pairing endpoint. Sunshine
// binds it on 47990 with a self-signed cert, so the client skips TLS
// verification for 127.0.0.1 only.
const sunshineLocalPinURL = "https://127.0.0.1:47990/api/pin"

// secretsEnvPath is where bootstrap writes the Sunshine/Tailscale creds.
const secretsEnvPath = "/etc/clouddeploy/secrets.env"

// readyMOTDPath is the SSH-login banner written when a deploy completes.
const readyMOTDPath = "/etc/update-motd.d/99-clouddeploy-ready"

func newPairCmd() *cobra.Command {
	var pinFlag string
	var nameFlag string
	cmd := &cobra.Command{
		Use:   "pair",
		Short: "Pair Moonlight with this host (interactive PIN entry)",
		Long: `Pair a Moonlight client with this host without any raw bash.

Run this after the deploy finishes. It prints the Tailscale IP to add in
Moonlight, then waits for the 4-digit PIN Moonlight shows and submits it
to Sunshine for you. The Sunshine Web UI credentials are read from
` + secretsEnvPath + ` (or the SUNSHINE_USER / SUNSHINE_PASS environment
variables). Pass --pin to submit a PIN non-interactively.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			// profileRequired=false: pairing is read-mostly and must
			// work with zero args (the MOTD suggests `sudo clouddeployctl
			// pair`). No apply lock is held while we wait on the operator.
			deps, lock, err := loadDeps(cmd, false)
			if err != nil {
				return err
			}
			if lock != nil {
				defer lock.Release()
			}
			return runPair(ctx, deps, os.Stdin, os.Stdout, pinFlag, nameFlag)
		},
	}
	cmd.Flags().StringVar(&pinFlag, "pin", "", "submit this PIN non-interactively instead of prompting")
	cmd.Flags().StringVar(&nameFlag, "name", "Moonlight", "client name to register with Sunshine")
	return cmd
}

// runPair drives the interactive pairing loop. in/out are injectable so
// the prompt logic is testable without a real terminal.
func runPair(ctx context.Context, deps *phase.Deps, in io.Reader, out io.Writer, pinFlag, nameFlag string) error {
	user, pass := sunshinePairCreds(deps)
	if strings.TrimSpace(pass) == "" {
		return fmt.Errorf("Sunshine password not found: set SUNSHINE_PASS in %s (or the environment) and retry", secretsEnvPath)
	}
	if strings.TrimSpace(nameFlag) == "" {
		nameFlag = "Moonlight"
	}

	ip := pairTailscaleIP(ctx, deps)
	fmt.Fprint(out, renderPairBanner(ip))

	// Non-interactive single shot (scripting / re-pair).
	if strings.TrimSpace(pinFlag) != "" {
		return submitAndReport(ctx, out, user, pass, pinFlag, nameFlag)
	}

	reader := bufio.NewReader(in)
	for {
		fmt.Fprint(out, "Enter the Moonlight PIN ('q' to quit): ")
		line, err := reader.ReadString('\n')
		pin := strings.TrimSpace(line)
		if pin == "" {
			if err != nil {
				// EOF with no input: nothing more to read.
				return fmt.Errorf("no PIN entered")
			}
			continue
		}
		if strings.EqualFold(pin, "q") || strings.EqualFold(pin, "quit") {
			fmt.Fprintln(out, "Pairing cancelled. Re-run 'clouddeployctl pair' whenever you're ready.")
			return nil
		}
		if !looksLikePin(pin) {
			fmt.Fprintln(out, "  That doesn't look like a Moonlight PIN (expected 4 digits). Try again.")
			continue
		}
		if submitErr := submitAndReport(ctx, out, user, pass, pin, nameFlag); submitErr == nil {
			return nil
		} else {
			fmt.Fprintf(out, "  %v\n", submitErr)
			fmt.Fprintln(out, "  Make sure you just added/selected this host in Moonlight so it is showing a PIN, then try again.")
		}
		if err != nil {
			// stdin closed after this attempt.
			return fmt.Errorf("pairing input closed")
		}
	}
}

func submitAndReport(ctx context.Context, out io.Writer, user, pass, pin, name string) error {
	accepted, detail, err := submitPin(ctx, user, pass, pin, name)
	if err != nil {
		return fmt.Errorf("could not reach Sunshine at %s: %v", sunshineLocalPinURL, err)
	}
	if !accepted {
		return fmt.Errorf("Sunshine rejected the PIN (%s)", detail)
	}
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "  Paired successfully. Start the stream from Moonlight now.")
	return nil
}

// submitPin POSTs the PIN to Sunshine's local pairing endpoint with Web
// UI Basic auth. Returns (accepted, human-readable detail, transport
// error). A non-nil error means we could not talk to Sunshine at all.
func submitPin(ctx context.Context, user, pass, pin, name string) (bool, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sunshineLocalPinURL, bytes.NewReader(buildPinRequestBody(pin, name)))
	if err != nil {
		return false, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(user, pass)

	resp, err := pairHTTPClient().Do(req)
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	if resp.StatusCode == http.StatusUnauthorized {
		return false, "HTTP 401 Unauthorized - SUNSHINE_USER/SUNSHINE_PASS do not match the Sunshine Web UI credentials", nil
	}
	if pinAccepted(body) {
		return true, "", nil
	}
	detail := strings.TrimSpace(string(body))
	if detail == "" {
		detail = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return false, detail, nil
}

// pairHTTPClient talks to the localhost Sunshine Web UI, which presents a
// self-signed cert. Verification is skipped only because the target is
// pinned to 127.0.0.1.
func pairHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

type pinRequest struct {
	PIN  string `json:"pin"`
	Name string `json:"name"`
}

func buildPinRequestBody(pin, name string) []byte {
	b, _ := json.Marshal(pinRequest{PIN: pin, Name: name})
	return b
}

// pinAccepted reports whether Sunshine's /api/pin response indicates the
// PIN was accepted. Sunshine has returned both {"status":true} and
// {"status":"true"} across versions, so both shapes are honored.
func pinAccepted(body []byte) bool {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	switch v := m["status"].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	}
	return false
}

// looksLikePin accepts a Moonlight PIN (4 digits today; allow up to 8 to
// be forgiving of future clients) so a typo'd word doesn't get POSTed.
func looksLikePin(s string) bool {
	if len(s) < 4 || len(s) > 8 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// sunshinePairCreds resolves the Sunshine Web UI credentials. Order:
// secrets.env, then SUNSHINE_USER/SUNSHINE_PASS env overrides, then the
// headless desktop user as the default username.
func sunshinePairCreds(deps *phase.Deps) (user, pass string) {
	if kv, err := secrets.Load(secretsEnvPath); err == nil && kv != nil {
		user = kv["SUNSHINE_USER"]
		pass = kv["SUNSHINE_PASS"]
	}
	if v := strings.TrimSpace(os.Getenv("SUNSHINE_USER")); v != "" {
		user = v
	}
	if v := strings.TrimSpace(os.Getenv("SUNSHINE_PASS")); v != "" {
		pass = v
	}
	if strings.TrimSpace(user) == "" {
		if deps != nil && deps.Profile != nil {
			user = deps.Profile.EffectiveDesktop().User
		}
		if strings.TrimSpace(user) == "" {
			user = "cloudgamer"
		}
	}
	return user, pass
}

// pairTailscaleIP prefers the live `tailscale ip -4` value (current
// truth) and falls back to the IP recorded by the tailscale phase.
func pairTailscaleIP(ctx context.Context, deps *phase.Deps) string {
	if deps != nil && deps.Runner != nil {
		res := deps.Runner.Exec(ctx, runner.CommandSpec{
			Argv:    []string{"tailscale", "ip", "-4"},
			LogFile: "-",
			Timeout: 10 * time.Second,
			DryRun:  deps.DryRun,
		})
		for _, line := range strings.Split(res.Stdout, "\n") {
			line = strings.TrimSpace(line)
			if net.ParseIP(line) != nil {
				return line
			}
		}
	}
	if deps != nil && deps.State != nil {
		if p := deps.State.Get(phase.TailscaleName); p != nil && p.Details != nil {
			if v, ok := p.Details["tailscale_ip"].(string); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

func renderPairBanner(ip string) string {
	ipLine := strings.TrimSpace(ip)
	if ipLine == "" {
		ipLine = "(Tailscale IP not detected - run: tailscale ip -4)"
	}
	return fmt.Sprintf(`
================================================================================
  CloudDeploy is ready to stream.

  1. Open Moonlight on your gaming device.
  2. Add this PC manually by IP address:

         %s

  3. Moonlight shows a 4-digit PIN. Type it below and press Enter.
================================================================================
`, ipLine)
}

// finalizeDeployUX is called once at the end of a successful deploy. It
// writes the login MOTD and prints the "run clouddeployctl pair" hint.
// Best-effort: a failure here never fails the deploy.
func finalizeDeployUX(ctx context.Context, deps *phase.Deps) {
	if deps == nil || deps.DryRun {
		return
	}
	if err := writeReadyMOTD(); err != nil {
		fmt.Printf("note: could not write CloudDeploy login banner (non-fatal): %v\n", err)
	}
	fmt.Print(renderDeployDoneBanner(pairTailscaleIP(ctx, deps)))
}

func writeReadyMOTD() error {
	if err := os.MkdirAll("/etc/update-motd.d", 0o755); err != nil {
		return err
	}
	return os.WriteFile(readyMOTDPath, []byte(renderReadyMOTD()), 0o755)
}

// renderReadyMOTD is the executable update-motd.d script shown on every
// SSH login after a deploy completes. It resolves the live Tailscale IP
// at login time so the operator never has to look it up.
func renderReadyMOTD() string {
	return `#!/usr/bin/env bash
# Managed by CloudDeploy v3. Rewritten on each successful deploy.
ip="$(tailscale ip -4 2>/dev/null | head -n1)"
echo
echo "============================================================"
echo "  CloudDeploy is ready - Sunshine is running."
echo
if [ -n "$ip" ]; then
  echo "  In Moonlight, add this PC by IP:  $ip"
else
  echo "  In Moonlight, add this PC by your Tailscale IP."
fi
echo "  Then pair it with one command:    sudo clouddeployctl pair"
echo "============================================================"
echo
`
}

func renderDeployDoneBanner(ip string) string {
	ipLine := strings.TrimSpace(ip)
	if ipLine == "" {
		ipLine = "run: tailscale ip -4"
	}
	return fmt.Sprintf(`
================================================================================
  NEXT STEP - connect Moonlight (no other commands needed):

      sudo clouddeployctl pair

  That shows your Tailscale IP (%s), waits for the Moonlight PIN,
  and finishes pairing for you.
================================================================================
`, ipLine)
}
