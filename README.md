# scaleset

> **⚠️  Proof of Concept**
> 
> This is a **POC** implementation, **not production-tested**.  
> The underlying `github.com/actions/scaleset` SDK is currently in **beta** (v0.1.0).  
> Do not use in production environments without thorough testing.

A compute-engine-agnostic autoscaler for GitHub Actions Runner Scale Sets,
built on the [`github.com/actions/scaleset`](https://github.com/actions/scaleset) SDK.

All runners are **strictly ephemeral**: each runner executes exactly one job
and is then permanently destroyed.

## Supported engines

| Engine | Status |
|--------|--------|
| Docker | Available |
| GCP Compute Engine | Available |
| EC2    | Planned |
| Azure VMs | Planned |

## Prerequisites

- Go 1.25+
- Docker daemon (for the Docker engine)
- GCP project with Compute Engine API enabled (for the GCP engine)
- A GitHub App or Personal Access Token with runner registration permissions

## Build

### Local binary

```bash
go build ./cmd/scaleset
./scaleset --version  # Shows "dev" (default when not built with ldflags)
```

### Container images

Container images are built and published to GitHub Container Registry (GHCR) via the CI/CD pipeline using [GoReleaser](https://goreleaser.com/) and [Ko](https://ko.build/).

**Pull the latest release:**

```bash
docker pull ghcr.io/terrpan/scaleset/scaleset:latest
```

**Pull a specific version:**

```bash
docker pull ghcr.io/terrpan/scaleset/scaleset:v0.3.0
```

Images are multi-arch (`linux/amd64` + `linux/arm64`) and include embedded SBOMs and build attestations. See [docs/ci-cd.md](docs/ci-cd.md) for details on the release process, versioning, and security features.

## Configuration

Copy `config.example.yaml` to `config.yaml` and fill in your values:

```bash
cp config.example.yaml config.yaml
```

See the example file for all available options. Every config field can be
overridden by a CLI flag.

### Authentication

**GitHub App (recommended):**

```yaml
github:
  url: "https://github.com/org/repo"
  app:
    client_id: "Iv1.abc123"
    installation_id: 12345
    private_key_path: "/path/to/private-key.pem"
```

**Personal Access Token:**

```yaml
github:
  url: "https://github.com/org/repo"
  token: "ghp_..."
```

## Usage

```bash
# Using a config file
./scaleset --config config.yaml

# Using CLI flags only (PAT auth)
./scaleset \
  --url https://github.com/org/repo \
  --token ghp_... \
  --name my-runners \
  --max-runners 5

# Using CLI flags (GitHub App auth)
./scaleset \
  --url https://github.com/org/repo \
  --app-client-id Iv1.abc123 \
  --app-installation-id 12345 \
  --app-private-key-path /path/to/key.pem \
  --name my-runners
```

### CLI flags

```
--config string               Path to YAML configuration file (default "config.yaml")
--url string                  GitHub URL for scale set registration
--token string                Personal access token
--app-client-id string        GitHub App client ID
--app-installation-id int     GitHub App installation ID
--app-private-key string      GitHub App private key (PEM, inline)
--app-private-key-path string Path to GitHub App private key PEM file
--name string                 Scale set name
--runner-group string         Runner group name
--min-runners int             Minimum number of runners
--max-runners int             Maximum number of runners
--log-level string            Log level (debug, info, warn, error)
--log-format string           Log format (text, json)
```

## Architecture

```
cmd/scaleset/main.go          CLI entrypoint (Cobra)
internal/
  config/config.go            YAML config, validation, factories
  engine/
    engine.go                 Engine interface (compute abstraction)
    docker/docker.go          Docker engine implementation
    gcp/gcp.go                GCP Compute Engine implementation
  scaler/scaler.go            Engine-agnostic listener.Scaler implementation
docs/
  gcp/                        GCP image build guide & Packer template
```

### Runner lifecycle

```
StartRunner -> idle -> (job assigned) -> busy -> (job done) -> DestroyRunner
```

The `engine.Engine` interface defines three methods:

- `StartRunner(ctx, name, jitConfig)` -- provision and start an ephemeral runner
- `DestroyRunner(ctx, id)` -- permanently destroy a runner after its job completes
- `Shutdown(ctx)` -- destroy all managed runners during process termination

The `scaler.Scaler` implements the SDK's `listener.Scaler` interface and
bridges the scaleset message lifecycle to any compute backend via `Engine`.

### Adding a new engine

1. Create `internal/engine/<name>/<name>.go`
2. Implement `engine.Engine` -- remember that `DestroyRunner` must permanently
   destroy the resource (terminate VM, delete pod), never merely stop it
3. Add a case to `config.NewEngine()` for the new engine type
4. Add the new type to `config.Validate()`

### Docker-in-Docker (DinD)

If your workflows need to run Docker commands (`docker build`, `docker compose`,
container actions, etc.), enable DinD in the config:

```yaml
engine:
  docker:
    enable: true
    image: "ghcr.io/actions/actions-runner:latest"
    dind: true
```

This bind-mounts the host's `/var/run/docker.sock` into each runner container.
Containers created by workflows become siblings on the host daemon.

**Security:** the Docker socket gives runner containers full access to the host
Docker daemon. Only enable this if you trust the workflows running on your
runners.

### GCP Compute Engine

The GCP engine creates a Compute Engine VM for every job and deletes it
when the job completes. Runners are strictly ephemeral -- VMs are terminated,
never stopped.

**Prerequisites:**

- A GCP project with the Compute Engine API enabled
- A pre-built runner VM image (see [docs/gcp/README.md](docs/gcp/README.md))
- Application Default Credentials with `roles/compute.instanceAdmin.v1`

**Authentication** uses [ADC](https://cloud.google.com/docs/authentication/application-default-credentials)
-- no credential fields in config. Works with:

- Attached service accounts (GCE, GKE Workload Identity, Cloud Run)
- Workload Identity Federation (for running outside GCP)
- `gcloud auth application-default login` (local dev)

See [docs/gcp/README.md](docs/gcp/README.md) for detailed auth setup
including WIF step-by-step instructions.

**Configuration:**

```yaml
engine:
  gcp:
    enable: true
    project: "my-project"
    zone: "us-central1-a"
    image: "projects/my-project/global/images/family/scaleset-runner"
    machine_type: "e2-medium"     # optional, default: e2-medium
    disk_size_gb: 50              # optional, default: 50
    public_ip: true               # optional, default: true
    # network: "my-vpc"           # optional, default: "default"
    # subnet: "projects/.../subnetworks/my-subnet"  # optional
    # service_account: "runner@my-project.iam.gserviceaccount.com"  # optional
```

## OpenTelemetry

The daemon is instrumented with OpenTelemetry (traces + metrics). A
`docker-compose.yaml` is included that starts the
[Aspire Dashboard](https://aspire.dev/dashboard/standalone/) as a local
OTLP receiver and UI.

```bash
docker compose up -d          # start dashboard at http://localhost:18888
./scaleset --config config.yaml
```

Enable in `config.yaml`:

```yaml
otel:
  enabled: true
  endpoint: "localhost:4318"
  insecure: true
```

**Metrics:** `scaleset.runners.idle`, `scaleset.runners.busy`,
`scaleset.runners.started`, `scaleset.runners.destroyed`,
`scaleset.jobs.completed` (by result), `scaleset.scale.events` (by action),
`scaleset.runner.startup.duration` (histogram).

**Traces:** `scaler.HandleDesiredRunnerCount`, `scaler.startRunner`,
`scaler.HandleJobStarted`, `scaler.HandleJobCompleted`,
`engine.{docker,gcp}.StartRunner`, `engine.{docker,gcp}.DestroyRunner`,
`engine.{docker,gcp}.Shutdown`.

## Prometheus

The daemon can expose a `/metrics` endpoint for Prometheus scraping,
independently of the OTLP tracing pipeline. You can use Prometheus alone,
OTLP alone, or both together.

| `otel.enabled` | `prometheus.enable` | What happens |
|:-:|:-:|:--|
| `false` | `false` | No telemetry |
| `false` | `true`  | Prometheus `/metrics` endpoint only (no traces) |
| `true`  | `false` | OTLP push (traces + metrics), no scrape endpoint |
| `true`  | `true`  | Both: OTLP push + Prometheus scrape endpoint |

Enable in `config.yaml`:

```yaml
prometheus:
  enable: true
  port: 9090        # default
```

A `prometheus.yml` scrape config and a Prometheus service in
`docker-compose.yaml` are included:

```bash
docker compose up -d prometheus   # Prometheus UI at http://localhost:9091
./scaleset --config config.yaml
```

The scrape target uses `host.docker.internal:9090` so Prometheus running
in Docker can reach the scaleset daemon on the host.

All OTEL metrics are automatically available in Prometheus format:
`scaleset_runners_idle`, `scaleset_runners_busy`,
`scaleset_runners_started_total`, `scaleset_runners_destroyed_total`,
`scaleset_jobs_completed_total`, `scaleset_scale_events_total`,
`scaleset_runner_startup_duration_seconds`.

## Targeting the scale set in workflows

```yaml
jobs:
  build:
    runs-on: my-runners  # matches the scale set name
    steps:
      - run: echo "Running on an ephemeral runner"
```
