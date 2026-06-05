# Security

## Reporting a vulnerability

Email: **jstimatze@gmail.com** with `hindcast-security` in the subject. Please do not file public GitHub issues for suspected vulnerabilities.

Expect a response within a week. Fixes for high-severity issues will be published as patch releases; disclosure follows the fix.

## Threat model

hindcast runs entirely on your local machine and sends nothing over the network by default. Two features are opt-in network exceptions: the `hindcast eval-api` command, and the `HINDCAST_JUDGE=1` outcome judge (see "Outcome judge (opt-in)" below). Its security posture is what an adversary with various levels of access could learn or do.

### What hindcast stores and where

| artifact | path | contents |
|---|---|---|
| per-project JSONL | `~/.claude/hindcast/projects/<hash>.jsonl` | numeric stats, deterministic labels, salt-hashed prompt tokens |
| per-project BM25 | `~/.claude/hindcast/projects/<hash>.bm25.gob` | salt-hashed inverted index, same numeric metadata |
| per-project accuracy log | `~/.claude/hindcast/projects/<hash>/accuracy.jsonl` | predicted vs actual durations + band fields per turn; numeric only |
| global sketch | `~/.claude/hindcast/global-sketch.json` | rolling window of wall/active seconds, no project identity |
| predictor health | `~/.claude/hindcast/health.json` | tuned sim threshold, MALR-by-tier, regressor winner; numeric only |
| regressor models | `~/.claude/hindcast/regressor.{gbdt,linear}.gob` | gob-encoded numeric weights + deterministic feature-name list; never user data |
| salt | `~/.claude/hindcast/salt` | 32 random bytes, 0600 |
| debug log | `~/.claude/hindcast/hook.log` | timestamps, hook names, session IDs, panics |
| per-session fallback marker | `~/.claude/hindcast/sessions/<id>/fallback-marker` | RFC3339 timestamp string only |
| pending turns | `/tmp/hindcast/pending-<session>-<nanos>.json` | salt-hashed tokens, `cwd`, short-lived (deleted at Stop) |
| MCP estimate handoff | `/tmp/hindcast/estimate-<session>.json` | wall/active integers from `hindcast_estimate` tool; consumed at next Stop |
| session momentum | `/tmp/hindcast/session-<session>.json` | recent-turn duration integers, ≤5 entries; deleted at 24h TTL |
| lock files | `/tmp/hindcast/lock-*` | PID:starttime of lock holder |

All directories are 0700, all files 0600.

### What never touches disk

- **Prompt text.** Classification and tokenization happen in memory; the prompt is dropped before any file write.
- **Git remote URLs.** `project_hash = md5(project_name)[:8]` replaces any path or remote in records.
- **Raw file paths.** Only counts of distinct paths (`files_touched`) are stored.
- **Tool arguments.** Only tool-call counts by name.

### Adversary access levels

**Local attacker with your user privileges.**
- Can read everything hindcast stores. Same threat model as SSH keys.
- Can dictionary-attack salt-hashed tokens in the BM25 indexes (FNV-1a is fast).
- Can modify settings.json to redirect hooks, observe all CC traffic.
- Mitigation: standard Unix filesystem permissions; hindcast adds no new attack surface beyond the files it writes.

**Local attacker who leaks only the BM25 indexes (not the salt).**
- 64-bit salted token hashes per doc, no prompt text.
- Without the salt, reverse-lookup via dictionary is infeasible (salt space is 2^256).
- With the salt, English dictionary attacks on tokens recover word lists per document. Tokens are stopword-filtered and >=2 chars, so "what the user was talking about" is recoverable at a coarse level.

**Shared project directory attacker.**
- Can write `.hindcast-project` in your cwd to influence which per-project bucket your records land in. Cannot cross projects or escalate.

**Network attacker.**
- Hook path and MCP server make zero network calls by default. The one exception on the hook path is the opt-in outcome judge (`HINDCAST_JUDGE=1`, see below).
- The `hindcast eval-api` CLI command is a network user: it calls the Anthropic Messages API at `https://api.anthropic.com/v1/messages` using `ANTHROPIC_API_KEY` from the environment or a local `.env` file. Only runs when you invoke it manually. Prompts sent to the API are historical prompt text read fresh from `~/.claude/projects/*/*.jsonl` at eval time — not from hindcast's stats-only records.

**eval-api prompt exfiltration — explicit.**
Running `hindcast eval-api` transmits the historical prompt text of N sampled turns to Anthropic's API. **The stats-only storage invariant does NOT apply to eval-api.** It's specifically a measurement tool that needs the prompt to ask Claude "how long would you estimate this would take?" If you have repos with NDA, client, or otherwise sensitive prompts, either:

- Skip `eval-api` for those projects (live `hindcast calibrate` is stats-only and keeps the privacy guarantee),
- Or redact the transcripts before running — `eval-api` reads from `~/.claude/projects/*/*.jsonl`; prune or scrub those files first.

The eval-api sampler respects `ANTHROPIC_API_KEY` only; it has no project-allowlist mechanism in v0.1.

**Outcome judge (opt-in) — `HINDCAST_JUDGE=1`.**
The outcome judge is the one feature that makes a network call on the *hook path*. It is **off unless you explicitly set `HINDCAST_JUDGE=1`**. When on, at each Stop it grades the previous turn (success/partial/fail) by sending to the Anthropic Messages API:

- the previous turn's **user prompt**, and
- up to the first **6 KB of the assistant's response text** for that turn.

**This breaks the stats-only / no-plaintext invariant for the judged turns** — raw conversation content leaves your machine. Before sending, hindcast runs a **best-effort secret scrub** (`internal/judge.Redact`: provider keys, AWS keys, PEM blocks, JWTs, and `key=value` secrets are replaced with `[REDACTED]`). This is defense-in-depth, **not a guarantee** — novel secret shapes and the response cap's head-truncation mean sensitive bytes can still egress. Other notes:

- The key is resolved via `ANTHROPIC_API_KEY` (env), a `.env` walking up from cwd, or a global `~/.config/hindcast/.env`. Keep that global file `chmod 600` — if it's group/other-readable, hindcast logs a warning to `hook.log` (a passive log, not a terminal prompt) and proceeds to use it.
- Verdicts are cached at `~/.claude/hindcast/judge-cache/` — one small file per distinct turn holding only `{verdict, model, ts}` (**no prompt or response text**). The cache is a flat, global, content-hashed directory, not per-project. It is cleared by `uninstall` and by a 30-day age-based GC; `hindcast forget <project>` removes that project's `outcomes.jsonl` but **not** the global judge cache (it isn't project-keyed, so a single-project forget can't selectively purge it).
- The model defaults to `claude-haiku-4-5`; override with `HINDCAST_JUDGE_MODEL`.

If you work with NDA/client/sensitive repos, **leave `HINDCAST_JUDGE` unset** — the default build never sends turn content anywhere.

### Known limitations (document-only)

- **Salt rotation.** If the salt is disclosed, existing BM25 indexes become dictionary-attackable. `hindcast rotate-salt` generates a new salt and clears all indexes; records (stats-only, not salt-dependent) are retained. Indexes rebuild naturally from next turns onward. Because records don't store plaintext tokens, rotating the salt means losing historical retrieval context — this is the correct privacy-preserving tradeoff.
- **CWD in pending file.** The `/tmp/hindcast/pending-*.json` file contains the raw CWD string during the turn. Short-lived (deleted at Stop) but a filesystem-path-level leak while alive.
- **`session_id` in hook.log.** CC-generated UUIDs are linkable across hindcast operations. Not cross-CC identifying.
- **FNV-1a rather than HMAC-SHA256.** FNV is much faster; HMAC would survive salt leaks better. Upgrade deferred pending evidence the tradeoff matters.
- **No confidentiality protection for `hook.log`.** Writes in plain text. 0600 perms only. Log rotates at 10 MB to `hook.log.1` (1-generation retention).
- **BM25 stopwords are English-only.** Non-English prompts index more common tokens, degrading retrieval specificity. Not a security issue; adjacent limitation.
- **macOS lock liveness is PID-only.** Linux uses `/proc/PID/stat` field 22 (process start-time in clock ticks since boot) to disambiguate a PID-recycled-since-lock-write case from a still-live holder. macOS exposes start-time via `KERN_PROC_PID` sysctl, but the current implementation doesn't read it — `processStartTime` returns 0 on darwin and `isStaleLock` falls back to `kill -0` (signal-0 reachability). PID recycling on a long-running macOS box could in theory let a fresh process steal a live lock; in practice this is bounded by macOS's 32-bit PID space (4B forks before any specific PID recycles) and the impact is one record drop, not a security failure.

### Schema migration

Records have `schema_version: 1`. v0.1 doesn't implement migration logic. If a future release changes record shape incompatibly:

- Additive changes (new fields via `omitempty`) will be handled transparently — old records deserialize with zero values for new fields.
- Breaking changes will require `hindcast backfill --rebuild` (which wipes and re-parses) or `hindcast uninstall && hindcast install` (clean slate). Breaking-change releases will document the exact command in the CHANGELOG.

Users who `hindcast install` over an older install will inherit whatever data the previous install left. Data format compatibility is guaranteed within a major version only.

### Install-time risks

- `hindcast install` modifies `~/.claude/settings.json`. A timestamped backup is written first (`settings.json.hindcast-backup-YYYYMMDD-HHMMSS`). Backups are not auto-deleted; rotate them yourself if you re-install repeatedly.
- Concurrent `hindcast install` invocations are not file-locked and can race. Don't run install in parallel.

### Supply chain

hindcast is a pure-Go binary with no third-party runtime dependencies. Verifiable via `go.mod` having no `require` entries beyond the standard module directive.

The module path is `github.com/justinstimatze/hindcast`. Install via `go install github.com/justinstimatze/hindcast/cmd/hindcast@VERSION`. Pin a tagged version in scripted installs.

### Uninstall guarantees

`hindcast uninstall` removes hook and MCP entries from `settings.json` by matching the command path, strips no markers from CLAUDE.md (hindcast never writes to CLAUDE.md), and deletes `~/.claude/hindcast/` unless `--keep-data` is passed. One command, full removal.
