# GCP Compute Engine Runner Image

This directory contains everything needed to build a GCP VM image for
ephemeral GitHub Actions runners and configure the scaleset GCP engine.

## Overview

The GCP engine creates a new Compute Engine VM for every job, passes the
JIT configuration via instance metadata, and deletes the VM when the job
completes. Each runner executes exactly one job and is permanently destroyed.

## Prerequisites

- A GCP project with the Compute Engine API enabled
- [Packer](https://www.packer.io/) >= 1.9
- `gcloud` CLI (for authentication and WIF setup)
- A service account or identity with `roles/compute.instanceAdmin.v1`

## Authentication

The GCP engine uses [Application Default Credentials (ADC)](https://cloud.google.com/docs/authentication/application-default-credentials).
No credential fields exist in the scaleset config -- auth is handled
entirely by the environment.

ADC resolves credentials in this order:

1. `GOOGLE_APPLICATION_CREDENTIALS` env var (JSON key file or WIF config)
2. `gcloud auth application-default login` (local development)
3. Attached service account (GCE, GKE, Cloud Run metadata server)

### Running on GCP (GKE, GCE, Cloud Run)

If scaleset runs on GCP infrastructure, ADC automatically uses the
attached service account via the metadata server. No configuration needed.

**GKE with Workload Identity (recommended for Kubernetes):**

```bash
# Create a GCP service account
gcloud iam service-accounts create scaleset-runner \
  --display-name="Scaleset Runner"

# Grant Compute Instance Admin
gcloud projects add-iam-policy-binding PROJECT_ID \
  --member="serviceAccount:scaleset-runner@PROJECT_ID.iam.gserviceaccount.com" \
  --role="roles/compute.instanceAdmin.v1"

# Bind the Kubernetes service account to the GCP service account
gcloud iam service-accounts add-iam-policy-binding \
  scaleset-runner@PROJECT_ID.iam.gserviceaccount.com \
  --role="roles/iam.workloadIdentityUser" \
  --member="serviceAccount:PROJECT_ID.svc.id.goog[NAMESPACE/KSA_NAME]"
```

Then annotate your Kubernetes service account:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: scaleset
  annotations:
    iam.gke.io/gcp-service-account: scaleset-runner@PROJECT_ID.iam.gserviceaccount.com
```

**GCE with attached service account:**

Launch the VM running scaleset with `--service-account` and the required
scopes:

```bash
gcloud compute instances create scaleset-controller \
  --service-account=scaleset-runner@PROJECT_ID.iam.gserviceaccount.com \
  --scopes=cloud-platform
```

### Running outside GCP (Workload Identity Federation)

WIF lets workloads running outside GCP (on-prem, AWS, Azure, CI systems)
authenticate without service account keys. It works by exchanging an
external OIDC/SAML token for short-lived GCP credentials.

```bash
# 1. Create a Workload Identity Pool
gcloud iam workload-identity-pools create scaleset-pool \
  --location="global" \
  --display-name="Scaleset Pool"

# 2. Create an OIDC provider (example: GitHub Actions OIDC)
gcloud iam workload-identity-pools providers create-oidc github-provider \
  --location="global" \
  --workload-identity-pool="scaleset-pool" \
  --issuer-uri="https://token.actions.githubusercontent.com" \
  --attribute-mapping="google.subject=assertion.sub,attribute.repository=assertion.repository"

# 3. Grant the external identity access to a GCP service account
gcloud iam service-accounts add-iam-policy-binding \
  scaleset-runner@PROJECT_ID.iam.gserviceaccount.com \
  --role="roles/iam.workloadIdentityUser" \
  --member="principalSet://iam.googleapis.com/projects/PROJECT_NUMBER/locations/global/workloadIdentityPools/scaleset-pool/attribute.repository/ORG/REPO"

# 4. Generate the credential configuration file
gcloud iam workload-identity-pools create-cred-config \
  projects/PROJECT_NUMBER/locations/global/workloadIdentityPools/scaleset-pool/providers/github-provider \
  --service-account="scaleset-runner@PROJECT_ID.iam.gserviceaccount.com" \
  --output-file="wif-config.json"

# 5. Point ADC to the config file
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/wif-config.json
```

The WIF config file contains no secrets -- it describes how to exchange
an external token for GCP credentials.

### Local development

```bash
gcloud auth application-default login
```

### Required IAM permissions

The identity running scaleset needs the following permissions on the
target GCP project:

| Permission | Included in Role |
|------------|-----------------|
| `compute.instances.create` | `roles/compute.instanceAdmin.v1` |
| `compute.instances.delete` | `roles/compute.instanceAdmin.v1` |
| `compute.instances.get` | `roles/compute.instanceAdmin.v1` |
| `compute.zoneOperations.get` | `roles/compute.instanceAdmin.v1` |
| `compute.instances.setMetadata` | `roles/compute.instanceAdmin.v1` |

If using a custom service account on runner VMs, also grant
`roles/iam.serviceAccountUser` on that service account.

## Building the Runner Image

### Linux (Ubuntu 24.04)

#### 1. Authenticate Packer

```bash
gcloud auth application-default login
```

#### 2. Initialize Packer plugins

```bash
cd docs/gcp
packer init .
```

#### 3. Build the image

```bash
packer build -var project_id=my-project .
```

Optional variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `project_id` | (required) | GCP project ID |
| `zone` | `us-central1-a` | Build VM zone |
| `image_name` | `scaleset-runner` | Image name prefix |
| `image_family` | `scaleset-runner` | Image family |
| `runner_version` | `2.321.0` | GitHub Actions runner version |
| `machine_type` | `e2-medium` | Build VM machine type |

#### 4. Reference the image in config

After building, use the image family in your scaleset config so new VMs
always use the latest image:

```yaml
engine:
  type: "gcp"
  gcp:
    project: "my-project"
    zone: "us-central1-a"
    image: "projects/my-project/global/images/family/scaleset-runner"
```

Or pin to a specific image:

```yaml
engine:
  type: "gcp"
  gcp:
    image: "projects/my-project/global/images/scaleset-runner-1234567890"
```

### Windows (Windows Server 2022)

#### 1. Authenticate Packer

```bash
gcloud auth application-default login
```

#### 2. Initialize Packer plugins

```bash
cd docs/gcp/windows
packer init .
```

#### 3. Build the image

```bash
packer build -var project_id=my-project .
```

The Windows build takes longer than Linux (~15-20 minutes) due to Windows
updates, Docker feature installation, and the required reboot.

Optional variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `project_id` | (required) | GCP project ID |
| `zone` | `us-central1-a` | Build VM zone |
| `image_name` | `scaleset-runner-windows` | Image name prefix |
| `image_family` | `scaleset-runner-windows` | Image family |
| `runner_version` | `2.321.0` | GitHub Actions runner version |
| `machine_type` | `e2-medium` | Build VM machine type |
| `disk_size` | `50` | Boot disk size in GB |

#### 4. Reference the image in config

```yaml
engine:
  type: "gcp"
  gcp:
    project: "my-project"
    zone: "us-central1-a"
    image: "projects/my-project/global/images/family/scaleset-runner-windows"
```

## What the Images Contain

### Linux

- **Ubuntu 24.04 LTS** base
- **Docker CE** from the official Docker APT repository
- **GitHub Actions runner agent** installed to `/home/runner`
- **`runner` user** (member of the `docker` group)
- **`scaleset-runner.service`** systemd unit that:
  1. Reads `ACTIONS_RUNNER_INPUT_JITCONFIG` from GCP instance metadata
  2. Launches the runner agent as the `runner` user

### Windows

- **Windows Server 2022** base
- **Docker** (Windows containers via DockerMsftProvider) -- runs Windows
  containers only, not Linux containers
- **Git for Windows** (via Chocolatey)
- **GitHub Actions runner agent** installed to `C:\actions-runner`
- **`ScalesetRunner` Scheduled Task** that:
  1. Reads `ACTIONS_RUNNER_INPUT_JITCONFIG` from GCP instance metadata
     via `Invoke-RestMethod`
  2. Launches the runner agent as `SYSTEM`

> **Note:** Docker on Windows Server provides Windows containers only.
> If your workflows need Linux containers, use the Linux image instead.

## Runner Lifecycle

The lifecycle is identical for Linux and Windows -- only the boot
mechanism differs (systemd vs Scheduled Task):

```
scaleset creates VM with JIT config in metadata
  -> VM boots
    -> Linux: systemd starts scaleset-runner.service
       Windows: Scheduled Task runs startup.ps1
      -> startup script reads JIT config from metadata server
        -> runner agent registers and picks up the job
          -> job completes
            -> scaleset calls DestroyRunner, VM is deleted
```
