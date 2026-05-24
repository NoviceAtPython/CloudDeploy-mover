package main

import (
	"strings"
	"testing"
)

func TestPinAccepted(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"bool true", `{"status":true}`, true},
		{"string true", `{"status":"true"}`, true},
		{"string True mixed case", `{"status":"True"}`, true},
		{"bool false", `{"status":false}`, false},
		{"unauthorized shape", `{"error":"Unauthorized","status":false,"status_code":401}`, false},
		{"missing status", `{"named_certs":[]}`, false},
		{"garbage", `not json`, false},
		{"empty", ``, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pinAccepted([]byte(c.body)); got != c.want {
				t.Fatalf("pinAccepted(%q): got %v want %v", c.body, got, c.want)
			}
		})
	}
}

func TestLooksLikePin(t *testing.T) {
	cases := map[string]bool{
		"1234":      true,
		"0000":      true,
		"12345678":  true,
		"123":       false, // too short
		"123456789": false, // too long
		"abcd":      false,
		"12a4":      false,
		"":          false,
		"12 4":      false,
	}
	for in, want := range cases {
		if got := looksLikePin(in); got != want {
			t.Errorf("looksLikePin(%q): got %v want %v", in, got, want)
		}
	}
}

func TestBuildPinRequestBodyIsDeterministicJSON(t *testing.T) {
	got := string(buildPinRequestBody("1234", "Moonlight"))
	want := `{"pin":"1234","name":"Moonlight"}`
	if got != want {
		t.Fatalf("buildPinRequestBody: got %s want %s", got, want)
	}
}

func TestRenderPairBannerIncludesIPAndInstructions(t *testing.T) {
	out := renderPairBanner("100.98.5.79")
	for _, want := range []string{"100.98.5.79", "Moonlight", "PIN", "Add this PC"} {
		if !strings.Contains(out, want) {
			t.Errorf("pair banner missing %q:\n%s", want, out)
		}
	}
	// Empty IP must still render a usable hint, not a blank line.
	if !strings.Contains(renderPairBanner(""), "tailscale ip -4") {
		t.Errorf("empty-IP pair banner should hint how to find the IP")
	}
}

func TestRenderDeployDoneBannerPointsAtPair(t *testing.T) {
	out := renderDeployDoneBanner("100.98.5.79")
	for _, want := range []string{"clouddeployctl pair", "100.98.5.79", "Moonlight"} {
		if !strings.Contains(out, want) {
			t.Errorf("deploy-done banner missing %q:\n%s", want, out)
		}
	}
}

func TestRenderReadyMOTDIsExecutableScriptThatPointsAtPair(t *testing.T) {
	out := renderReadyMOTD()
	for _, want := range []string{"#!/usr/bin/env bash", "tailscale ip -4", "clouddeployctl pair", "CloudDeploy is ready"} {
		if !strings.Contains(out, want) {
			t.Errorf("ready MOTD missing %q:\n%s", want, out)
		}
	}
}
