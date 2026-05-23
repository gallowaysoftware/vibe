package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vamp"
)

// runPipelineDoctor execs the given pipeline binary's `requirements
// --format json` subcommand, parses the declared services, and runs a
// preflight HTTP-reachability check against each. The capability list
// is printed as informational only (capability → profile mapping
// validation is the operator's responsibility for now — a future
// version may cross-reference capabilities.yaml).
//
// Designed for the "you need a vibe daemon running, plus searxng,
// plus kokoro-fastapi, plus ..." class of dev-ex friction: instead of
// learning each pipeline's deps from the README, the operator runs
// `vibe doctor --pipeline ./mybin` and sees what's missing in one
// pass, with the setup commands the pipeline author shipped.
func runPipelineDoctor(ctx context.Context, binaryPath string, out io.Writer) error {
	req, err := readPipelineRequirements(ctx, binaryPath)
	if err != nil {
		return fmt.Errorf("read pipeline requirements: %w", err)
	}

	fmt.Fprintf(out, "Pipeline: %s\n", req.Pipeline)
	if req.Description != "" {
		fmt.Fprintf(out, "  %s\n", req.Description)
	}
	fmt.Fprintln(out)

	if len(req.Capabilities) > 0 {
		fmt.Fprintln(out, "Capabilities (verify these are mapped in your ~/.config/vamp/capabilities.yaml):")
		for _, c := range req.Capabilities {
			hint := ""
			if c.ModelHint != nil && c.ModelHint.SuggestedModel != "" {
				hint = " (suggested: " + c.ModelHint.SuggestedModel + ")"
			}
			fmt.Fprintf(out, "  %s %s%s\n", statusInfo.tag(), c.Name, hint)
		}
		fmt.Fprintln(out)
	}

	anyFail := false
	if len(req.Services) > 0 {
		fmt.Fprintln(out, "External services:")
		for _, s := range req.Services {
			res := checkServiceReachable(ctx, s)
			fmt.Fprintf(out, "  %s %-20s %s\n", res.Status.tag(), s.Name, res.Message)
			if res.Status == statusFail {
				anyFail = true
				if s.SetupHint != "" {
					fmt.Fprintf(out, "       setup: %s\n", s.SetupHint)
				}
			}
		}
		fmt.Fprintln(out)
	}

	if req.GPUMemory != "" || req.DiskSpace != "" {
		fmt.Fprintln(out, "Hardware footprint (informational; no enforcement):")
		if req.GPUMemory != "" {
			fmt.Fprintf(out, "  %s GPU memory:   %s\n", statusInfo.tag(), req.GPUMemory)
		}
		if req.DiskSpace != "" {
			fmt.Fprintf(out, "  %s Disk space:   %s\n", statusInfo.tag(), req.DiskSpace)
		}
		fmt.Fprintln(out)
	}

	if len(req.Notes) > 0 {
		fmt.Fprintln(out, "Notes from the pipeline author:")
		for _, n := range req.Notes {
			fmt.Fprintf(out, "  - %s\n", n)
		}
		fmt.Fprintln(out)
	}

	if anyFail {
		return errDoctorFailed
	}
	return nil
}

// readPipelineRequirements execs the pipeline binary's requirements
// subcommand with --format json and decodes the result.
func readPipelineRequirements(ctx context.Context, binaryPath string) (vamp.Requirements, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binaryPath, "requirements", "--format", "json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if msg == "" {
			msg = err.Error()
		}
		return vamp.Requirements{}, fmt.Errorf("exec %s requirements: %s", binaryPath, msg)
	}
	var req vamp.Requirements
	if err := json.Unmarshal(stdout.Bytes(), &req); err != nil {
		return vamp.Requirements{}, fmt.Errorf("parse JSON from %s requirements: %w", binaryPath, err)
	}
	return req, nil
}

// checkServiceReachable runs an HTTP GET against the service URL with
// a short timeout and reports OK on any response (including 4xx — many
// services 404 the bare path but are still up; we only care that the
// TCP connect + HTTP exchange succeeds). Empty URL is reported as
// WARN; the operator may want to check manually.
func checkServiceReachable(ctx context.Context, s vamp.ServiceRequirement) checkResult {
	name := s.Name
	if s.URL == "" {
		return checkResult{Name: name, Status: statusWarn, Message: "no URL declared; check manually"}
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return checkResult{Name: name, Status: statusFail, Message: fmt.Sprintf("invalid URL %q: %v", s.URL, err)}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return checkResult{Name: name, Status: statusFail, Message: fmt.Sprintf("%s unreachable: %v", s.URL, err)}
	}
	resp.Body.Close()
	return checkResult{Name: name, Status: statusOK, Message: fmt.Sprintf("%s reachable (HTTP %d)", s.URL, resp.StatusCode)}
}
