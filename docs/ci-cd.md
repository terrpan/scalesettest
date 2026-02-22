# CI/CD Pipeline

This document describes the continuous integration and release pipeline for the scaleset project.

## Overview

The pipeline is built on **GitHub Actions** and uses:

- **[golangci-lint](https://golangci-lint.run/)** for linting
- **[CodeQL](https://codeql.github.com/)** for static security analysis (SAST)
- **[GoReleaser](https://goreleaser.com/)** for builds, releases, and changelog generation
- **[Ko](https://ko.build/)** (via GoReleaser's `kos` integration) for container image builds
- **[svu](https://github.com/caarlos0/svu)** for semantic versioning based on conventional commits
- **[Trivy](https://trivy.dev/)** for container vulnerability scanning
- **[GitHub Attestations](https://docs.github.com/en/actions/security-for-github-actions/using-artifact-attestations)** for build provenance (SLSA)

All workflows follow **least-privilege permissions** and use **concurrency controls** to prevent resource waste.

## Workflows

| Workflow | Trigger | Purpose |
|----------|---------|---------|
| **CI** (`.github/workflows/ci.yml`) | PR, push to `main` | Lint, test, dependency review |
| **Build** (`.github/workflows/build.yml`) | PR, push to `main` | GoReleaser snapshot builds, push dev images on `main` |
| **CodeQL** (`.github/workflows/codeql.yml`) | PR, push to `main`, weekly schedule | Static application security testing (SAST) |
| **Version** (`.github/workflows/version.yml`) | Manual (`workflow_dispatch`) | Determine next semver tag with `svu`, create tag, dispatch release workflow |
| **Release** (`.github/workflows/release.yml`) | Tag push (`v*`), workflow dispatch | Build binaries, push multi-arch images, create GitHub Release, attest, scan |

---

## CI Pipeline

**File:** `.github/workflows/ci.yml`

### Triggers

- Pull requests targeting `main`
- Pushes to `main`

### Jobs

#### 1. **Lint**

Runs `golangci-lint` with the project's `.golangci.yml` config.

- **Checks:** errcheck, ineffassign, staticcheck, gofmt, govet, and more
- **Timeout:** 5 minutes

#### 2. **Test**

Runs the full Go test suite:

```bash
go test -v -race -coverprofile=coverage.out ./...
```

- **Race detector:** Enabled to catch concurrency bugs
- **Coverage:** Uploaded as an artifact for analysis

Integration tests (tagged with `//go:build integration`) are skipped in CI. Run them locally with:

```bash
go test -v -tags integration ./...
```

#### 3. **Dependency Review**

Analyzes dependency changes in PRs for:

- Known vulnerabilities (CVEs)
- License compliance issues
- Supply chain risks

Runs only on pull requests (not on pushes to `main`).

---

## Build Pipeline

**File:** `.github/workflows/build.yml`

### Triggers

- Pull requests targeting `main`
- Pushes to `main`

### Jobs

#### 1. **Build & Push**

Runs `goreleaser release --snapshot --clean` to validate the full release pipeline without creating a GitHub Release.

**On PRs:**
- Dry-run build (no image push)
- Validates GoReleaser config and build reproducibility

**On pushes to `main`:**
- Builds snapshot binaries and container images locally
- Pushes a dev snapshot image to GHCR tagged with the short commit SHA (e.g., `ghcr.io/terrpan/scalesettest/scaleset:abc1234`)
- Triggers the vulnerability scan job

#### 2. **Vulnerability Scan** (main only)

Scans the snapshot image with **Trivy** and uploads results to GitHub Security:

- **Formats:** JSON (artifact) + SARIF (Security tab)
- **Severities:** CRITICAL, HIGH, MEDIUM
- **Retention:** 90 days

---

## CodeQL Analysis (SAST)

**File:** `.github/workflows/codeql.yml`

### Triggers

- Pull requests targeting `main`
- Pushes to `main`
- Weekly schedule (Sundays at 00:00 UTC)

### Analysis

CodeQL performs **static application security testing** to detect:

- SQL injection
- Command injection
- Path traversal
- Insecure randomness
- Use of weak cryptography
- Other CWEs relevant to Go

Results are uploaded to the **Security** tab under "Code scanning alerts."

---

## Versioning Workflow

**File:** `.github/workflows/version.yml`

### Trigger

**Manual only** (`workflow_dispatch`)

You control when releases happen — not every push to `main` creates a release.

### How It Works

1. **Checkout** the repository with full history (`fetch-depth: 0`)
2. **Install svu** (semantic version utility)
3. **Determine version:**
   - `svu current` → current version (e.g., `v0.2.0`)
   - `svu next` → next version based on conventional commits since the last tag
4. **Create an annotated tag** (e.g., `v0.3.0`) and push it to the remote
5. **Dispatch the release workflow** with the new tag

### Options

- **Force patch increment:** Force a patch bump even if no conventional commits warrant it (useful for non-code releases like docs-only changes)
- **Dry run:** Preview what version would be created without actually tagging

### Conventional Commits

`svu` analyzes commit messages to determine the next version. Use these prefixes:

| Commit Prefix | Example | Bump Type | Notes |
|---------------|---------|-----------|-------|
| `feat:` | `feat: add GCP engine` | **Minor** (`v0.2.0 → v0.3.0`) | New feature |
| `fix:` | `fix: handle nil pointer in scaler` | **Patch** (`v0.2.0 → v0.2.1`) | Bug fix |
| `perf:` | `perf: optimize startup time` | **Patch** | Performance improvement |
| `!` or `BREAKING CHANGE:` | `feat!: change config schema` | **Major** (`v0.2.0 → v1.0.0`) | Breaking change |
| `docs:`, `chore:`, `ci:`, `test:`, `style:` | `docs: update README` | **No bump** | Excluded from changelog |

**Examples:**

```bash
# Minor bump (new feature)
git commit -m "feat: add support for AWS EC2 engine"

# Patch bump (bug fix)
git commit -m "fix: correct runner lifecycle in Docker engine"

# Major bump (breaking change, long form)
git commit -m "feat: redesign config schema

BREAKING CHANGE: engine.type field renamed to engine.kind"

# Major bump (breaking change, short form with !)
git commit -m "feat!: redesign config schema"

# No version bump (excluded)
git commit -m "docs: add CI/CD documentation"
```

If multiple commits exist since the last tag, `svu` picks the **highest bump type**. For example:

- 3 × `fix:` + 1 × `feat:` → **minor bump** (feat wins)
- 10 × `fix:` → **patch bump**
- 1 × `feat!:` + 5 × `feat:` → **major bump** (breaking change wins)

### Why Manual?

The version workflow is intentionally manual so you can:

- **Batch changes** — merge 10 PRs, then release once instead of 10 times
- **Control release timing** — release when stakeholders are available, not at 3am
- **Preview versions** — use dry-run mode to see what `svu` will compute before committing

If you prefer fully automatic releases, the workflow can be changed to trigger on `push: branches: [main]`.

---

## Release Pipeline

**File:** `.github/workflows/release.yml`

### Triggers

- **Tag push** (`v*`) — normally triggered by the Version workflow
- **Manual workflow dispatch** — fallback to re-run a failed release for an existing tag

### Jobs

#### 1. **GoReleaser**

Builds, packages, and publishes the release:

**Binaries:**
- `linux/amd64`, `linux/arm64`
- Standalone binaries (no extraction needed) — `scaleset_linux_amd64`, `scaleset_linux_arm64`
- Tarballs (includes README) — `scaleset_0.3.0_linux_amd64.tar.gz`, etc.
- Checksums — `checksums.txt` (SHA256)

**Container Images:**
- Built with **Ko** (no Dockerfile) via GoReleaser's `kos` integration
- Multi-arch manifest (`linux/amd64` + `linux/arm64`)
- Base image: `cgr.dev/chainguard/static` (distroless, minimal attack surface)
- Pushed to GHCR: `ghcr.io/terrpan/scalesettest/scaleset:v0.3.0` + `:latest`
- **SBOM embedded** in the image (SPDX format) — Ko generates and attaches the SBOM automatically

**Build Info:**
- `ldflags` inject version, commit, and build timestamp into `internal/buildinfo` package
- Run `scaleset --version` to see injected values

**GitHub Release:**
- Draft: `false` (published immediately)
- Prerelease: `auto` (tags with `-alpha`, `-beta`, `-rc` are marked as pre-releases)
- Changelog: grouped by conventional commit type (Features, Bug Fixes, Performance, etc.)

**Build Attestation:**
- **SLSA provenance** attestation generated and pushed to GHCR
- Links the image to the exact workflow run, commit, and build environment
- Verify with: `gh attestation verify oci://ghcr.io/terrpan/scalesettest/scaleset:v0.3.0 --owner terrpan`

#### 2. **Vulnerability Scan**

Scans the released image with **Trivy**:

- **Formats:** JSON (artifact) + SARIF (Security tab)
- **Severities:** CRITICAL, HIGH, MEDIUM
- **Retention:** 90 days

Runs after GoReleaser succeeds and uploads results to the **Security** tab under "Code scanning alerts" (category: `trivy-release`).

---

## SBOM (Software Bill of Materials)

**Format:** SPDX (industry standard)

**Generation:** Ko automatically generates an SBOM during the container build and embeds it in the image.

**Location:** The SBOM is pushed as a separate artifact to GHCR alongside the image:

```bash
# List all tags for the scaleset package (includes SBOM tags)
crane ls ghcr.io/terrpan/scalesettest/scaleset

# Example output:
# v0.3.0
# sha256-abc123...sbom   ← SBOM artifact
```

**Inspection:**

```bash
# Pull the SBOM artifact
crane export ghcr.io/terrpan/scalesettest/scaleset:sha256-<digest>.sbom | tar -xOf - sbom.spdx.json | jq
```

Or use **cosign** to inspect the supply chain:

```bash
cosign tree ghcr.io/terrpan/scalesettest/scaleset:v0.3.0
```

---

## Branch Protection

The `main` branch is protected with the following rules:

| Setting | Value |
|---------|-------|
| **Require pull request** | ✅ Yes |
| **Required approving reviews** | 0 (sole maintainer) |
| **Dismiss stale reviews** | ✅ Yes |
| **Require status checks** | ✅ Yes |
| **Required checks** | `Lint`, `Test`, `Build & Push`, `CodeQL Analysis (go)`, `Dependency Review` |
| **Require branches to be up to date** | ✅ Yes |
| **Require conversation resolution** | ✅ Yes |
| **Require signed commits** | ❌ No |
| **Restrict pushes** | ❌ No (sole maintainer) |
| **Allow force pushes** | ❌ No |
| **Allow deletions** | ❌ No |

---

## Release Process (End-to-End)

Here's the full flow from code change to published release:

```
┌─────────────────────────────────────────────────────────────────┐
│  Developer                                                      │
└─────────────────────────────────────────────────────────────────┘
  │
  │  1. Create feature branch
  │  2. Make changes, commit with conventional commit message
  │     (e.g., "feat: add AWS EC2 engine")
  │  3. Push branch, open PR
  │
  v
┌─────────────────────────────────────────────────────────────────┐
│  CI Pipeline (PR)                                               │
│  ────────────────────────────────────────────────────────────   │
│  ✓ Lint (golangci-lint)                                         │
│  ✓ Test (go test -race)                                         │
│  ✓ Build (GoReleaser snapshot, dry-run)                         │
│  ✓ CodeQL (SAST)                                                │
│  ✓ Dependency Review (CVE scan)                                 │
└─────────────────────────────────────────────────────────────────┘
  │
  │  4. Review, approve, merge to main
  │
  v
┌─────────────────────────────────────────────────────────────────┐
│  CI Pipeline (main)                                             │
│  ────────────────────────────────────────────────────────────   │
│  ✓ Lint, Test, CodeQL                                           │
│  ✓ Build & Push snapshot image (ghcr.io/.../scaleset:abc1234)  │
│  ✓ Vulnerability Scan (Trivy → Security tab)                    │
└─────────────────────────────────────────────────────────────────┘
  │
  │  5. Developer decides it's time to release
  │  6. Manually trigger "Version" workflow via GitHub UI
  │
  v
┌─────────────────────────────────────────────────────────────────┐
│  Version Workflow                                               │
│  ────────────────────────────────────────────────────────────   │
│  ✓ svu current → v0.2.0                                         │
│  ✓ svu next → v0.3.0 (found "feat:" commit → minor bump)        │
│  ✓ git tag -a v0.3.0                                            │
│  ✓ git push origin v0.3.0                                       │
│  ✓ gh workflow run release.yml --field tag=v0.3.0               │
└─────────────────────────────────────────────────────────────────┘
  │
  v
┌─────────────────────────────────────────────────────────────────┐
│  Release Workflow                                               │
│  ────────────────────────────────────────────────────────────   │
│  ✓ GoReleaser:                                                  │
│    • Build binaries (linux/amd64, linux/arm64)                  │
│    • Create archives + checksums                                │
│    • Build multi-arch container image with Ko                   │
│    • Generate SBOM (SPDX, embedded in image)                    │
│    • Push to GHCR (ghcr.io/.../scaleset:v0.3.0 + :latest)       │
│    • Generate grouped changelog from conventional commits       │
│    • Create GitHub Release with binaries + instructions         │
│  ✓ Attest build provenance (SLSA, pushed to registry)           │
│  ✓ Vulnerability Scan (Trivy, results → Security tab)           │
└─────────────────────────────────────────────────────────────────┘
  │
  v
┌─────────────────────────────────────────────────────────────────┐
│  Published Release                                              │
│  ────────────────────────────────────────────────────────────   │
│  • GitHub Release: https://github.com/.../releases/tag/v0.3.0   │
│  • Container: ghcr.io/terrpan/scalesettest/scaleset:v0.3.0      │
│  • Binaries: scaleset_linux_amd64, scaleset_linux_arm64         │
│  • Checksums: checksums.txt                                     │
│  • SBOM: embedded in image + pushed as GHCR artifact            │
│  • Attestation: verifiable with gh attestation verify           │
│  • Vulnerability report: uploaded to Security tab               │
└─────────────────────────────────────────────────────────────────┘
```

---

## Troubleshooting

### "Version workflow created a tag but release workflow didn't trigger"

**Cause:** Tags pushed with `GITHUB_TOKEN` don't trigger other workflows (GitHub Actions limitation).

**Solution:** The Version workflow explicitly dispatches the Release workflow via `gh workflow run`. If this fails, manually trigger the Release workflow with `workflow_dispatch` and enter the tag name.

### "GoReleaser image path doesn't match GHCR package"

**Cause:** `KO_DOCKER_REPO` env var was overriding GoReleaser's `kos.repositories` config.

**Solution:** We removed `KO_DOCKER_REPO` from workflows. GoReleaser's `repositories` field in `.goreleaser.yaml` is the single source of truth.

### "Attestation step failed with 404 on image digest"

**Cause:** The workflow was using a hardcoded image path that didn't match where Ko published.

**Solution:** We now parse the actual image name and digest from GoReleaser's artifacts JSON output dynamically.

### "svu next says no bump needed but I added new commits"

**Cause:** None of the commits since the last tag use conventional commit prefixes (`feat:`, `fix:`, etc.).

**Solution:** Use conventional commit messages, or manually trigger the Version workflow with "Force patch increment" checked.

---

## Security

- **Branch protection** prevents direct pushes to `main` — all changes go through PRs
- **Dependency Review** scans for CVEs in new dependencies
- **CodeQL** performs SAST on every PR and weekly
- **Trivy** scans all released container images for vulnerabilities
- **SLSA attestations** link images to their build provenance for supply chain verification
- **Least-privilege permissions** — workflows only request the minimum permissions needed
- **Distroless base image** (`cgr.dev/chainguard/static`) minimizes attack surface

---

## References

- [GoReleaser documentation](https://goreleaser.com/)
- [Ko documentation](https://ko.build/)
- [svu (semantic version utility)](https://github.com/caarlos0/svu)
- [Conventional Commits specification](https://www.conventionalcommits.org/)
- [GitHub Actions security hardening](https://docs.github.com/en/actions/security-for-github-actions)
- [SLSA provenance attestations](https://slsa.dev/)
- [SPDX SBOM specification](https://spdx.dev/)
