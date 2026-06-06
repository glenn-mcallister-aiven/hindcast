// Command hindcast is the entry point for all hindcast subcommands:
// the Claude Code hooks (pending/record/inject), the MCP stdio server,
// and the user-facing CLI (install/uninstall/show/status/forget).
//
// All subcommands share a single Go binary so settings.json only has to
// point at one executable path.
package main

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"

	"github.com/justinstimatze/hindcast/internal/hook"
)

// version is "dev" by default and baked at release time via
//
//	go install -ldflags "-X main.version=$(git describe --tags --always --dirty)" ./cmd/hindcast
//
// The git tag is the single source of truth — there is no hand-maintained
// version constant to drift out of sync. buildVersion() resolves it.
var version = "dev"

// buildVersion reports the binary's version, preferring (in order): a
// release value baked in via -ldflags; the module version when installed
// with `go install …@vX.Y.Z`; the embedded VCS commit (+dirty) for local
// `go build`. Falls back to "dev" when none is available (e.g. a tarball
// build outside a git tree).
func buildVersion() string {
	if version != "dev" {
		return version
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev, dirty string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) >= 12 {
				rev = s.Value[:12]
			} else {
				rev = s.Value
			}
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev != "" {
		return rev + dirty
	}
	return version
}

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(1)
	}
	args := os.Args[1:]
	switch args[0] {
	// Hooks — invoked by Claude Code via settings.json; never run by hand.
	case "pending":
		hook.Guard("pending", cmdPending)
	case "record":
		hook.Guard("record", cmdRecord)
	case "inject":
		hook.Guard("inject", cmdInject)
	case "mcp":
		hook.Guard("mcp", cmdMCP)
	case "_autotune-worker":
		// Internal subcommand spawned detached by the Stop hook to refresh
		// health.json + regressor models. Underscored to mark it as not
		// part of the user-facing CLI surface; intentionally absent from
		// --help. Lives in its own process so a long retune can't be
		// killed by the Stop worker's 30s timeout, which would leave
		// stale .tmp files mid-rename.
		hook.Guard("autotune", cmdAutoTuneWorker)
	case "_judge-probe":
		// Internal: judge a turn read as JSON from stdin, for live-testing
		// the outcome judge wiring. Underscored / absent from --help.
		// Usage: echo '{"prompt":..,"response":..}' | HINDCAST_JUDGE=1 hindcast _judge-probe
		cmdJudgeProbe(args[1:])

	// User-facing CLI.
	case "install":
		cmdInstall(args[1:])
	case "backfill":
		cmdBackfill(args[1:])
	case "bench":
		cmdBench(args[1:])
	case "calibrate":
		cmdCalibrate(args[1:])
	case "export-seed":
		cmdExportSeed(args[1:])
	case "eval-api":
		cmdEvalAPI(args[1:])
	case "uninstall":
		cmdUninstall(args[1:])
	case "show":
		cmdShow(args[1:])
	case "status":
		cmdStatus(args[1:]...)
	case "forget":
		cmdForget(args[1:])
	case "rotate-salt":
		cmdRotateSalt(args[1:])
	case "predict":
		cmdPredict(args[1:])
	case "verify":
		cmdVerify(args[1:])
	case "bench-cross":
		cmdBenchCross(args[1:])
	case "tune":
		cmdTune(args[1:])
	case "train":
		cmdTrain(args[1:])

	case "version", "--version", "-v":
		fmt.Println("hindcast", buildVersion())
	case "help", "--help", "-h":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "hindcast: unknown command %q\n\n", args[0])
		usage(os.Stderr)
		os.Exit(1)
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `hindcast — wall-clock estimation priors for Claude Code

Usage:
  hindcast install               Wire hooks + MCP into ~/.claude/settings.json,
                                 initialize per-install salt, backfill from transcripts.
  hindcast uninstall             Remove hook/MCP entries and all stored data.
  hindcast show [--project P]    Dump what hindcast has recorded.
  hindcast status                Hook health check (tail of hook.log).
  hindcast forget PROJECT        Delete a project's data, rebuild global sketch.
  hindcast rotate-salt           New BM25 salt; clears all indexes (records kept).
  hindcast version               Print version.
  hindcast backfill              Parse ~/.claude/projects/* into records (safe to re-run).
  hindcast bench                 Offline prediction-accuracy benchmark on backfilled data.
  hindcast bench-cross           Cross-corpus benchmark vs METR HCAST / OpenHands traces.
  hindcast verify                Self-eval (prefix-LOO) on your own backfilled data.
  hindcast tune                  Find your empirical sim cliff; write health.json.
  hindcast train                 Train per-user GBDT regressor on backfilled records.
  hindcast show --health         Display tuned predictor state.
  hindcast calibrate             Online A/B analysis (control vs treatment, legacy).
  hindcast predict [PROMPT]      One-shot kNN prediction for a prompt (CLI).
  hindcast eval-api [-n N]       Offline A/B against the Claude API (control vs treatment).
  hindcast export-seed           Dump the global sketch as JSON (maintainer tooling).

Hooks (invoked by Claude Code; not for manual use):
  hindcast pending               UserPromptSubmit — record turn start + BM25 delivery.
  hindcast record                Stop — record completed turn.
  hindcast inject                SessionStart — inject session-scoped priors.
  hindcast mcp                   MCP stdio server — exposes hindcast_prior tool.

See README.md for usage and CHANGELOG.md for release notes.
`)
}
