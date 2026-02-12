# runner-windows.pkr.hcl -- Packer template for building a GCP Windows
# runner image (boot-optimized).
#
# The image is based on Windows Server 2025 Core and includes:
#   - GitHub Actions runner agent
#   - Docker CE (static binaries from Docker)
#   - Git for Windows
#   - Boot optimizations:
#       * Disabled Windows Defender real-time monitoring
#       * Disabled unnecessary Windows services
#       * High Performance power plan
#       * 32 GB disk for faster image attach
#
# Build:
#   packer init .
#   packer build -var project_id=my-project .

packer {
  required_plugins {
    googlecompute = {
      source  = "github.com/hashicorp/googlecompute"
      version = ">= 1.1.0"
    }
  }
}

# ---------------------------------------------------------------------------
# Variables
# ---------------------------------------------------------------------------

variable "project_id" {
  type        = string
  description = "GCP project ID where the image will be created."
}

variable "zone" {
  type        = string
  default     = "us-central1-a"
  description = "GCP zone for the build VM."
}

variable "image_name" {
  type        = string
  default     = "scaleset-runner-windows"
  description = "Name of the resulting GCP image. A timestamp suffix is appended automatically."
}

variable "image_family" {
  type        = string
  default     = "scaleset-runner-windows"
  description = "Image family to assign. Use this in scaleset config to always get the latest image."
}

variable "runner_version" {
  type        = string
  default     = "2.331.0"
  description = "GitHub Actions runner agent version to install."
}

variable "docker_version" {
  type        = string
  default     = "29.2.1"
  description = "Docker CE version to install (from Docker static binaries)."
}

variable "machine_type" {
  type        = string
  default     = "e2-medium"
  description = "Machine type for the build VM."
}

variable "disk_size" {
  type        = number
  default     = 50
  description = "Boot disk size in GB for the build VM. Must be at least 50 GB."
}

variable "network" {
  type        = string
  default     = "default"
  description = "VPC network for the build VM."
}

variable "subnetwork" {
  type        = string
  default     = ""
  description = "Subnetwork for the build VM. If empty, the default subnet for the zone is used."
}

variable "source_image_family" {
  type        = string
  default     = "windows-2025-core"
  description = "Source image family to use for the build VM. windows-2025-core is slimmer than Desktop Experience."
}

variable "source_image_project_id" {
  type        = string
  default     = "windows-cloud"
  description = "GCP project containing the source image."
}

variable "omit_external_ip" {
  type        = bool
  default     = false
  description = "Do not assign an external IP to the build VM. Requires use_internal_ip and VPC connectivity."
}

variable "use_internal_ip" {
  type        = bool
  default     = false
  description = "Connect to the build VM via its internal IP. Requires network connectivity to the VPC."
}

variable "tags" {
  type        = list(string)
  default     = ["packer-winrm"]
  description = "Network tags to apply to the build VM. Default includes 'packer-winrm' for the WinRM firewall rule."
}

variable "dism_cleanup" {
  type        = bool
  default     = false
  description = "Run DISM /ResetBase cleanup to shrink image size. Saves ~2 GB but adds 20-40 min to build time. Not needed for testing since the image is boot-optimized (no need to minimize boot disk size). Recommended for production images to reduce storage costs."
}

# ---------------------------------------------------------------------------
# Source
# ---------------------------------------------------------------------------

source "googlecompute" "runner-windows" {
  project_id   = var.project_id
  zone         = var.zone
  machine_type = var.machine_type

  source_image_family     = var.source_image_family
  source_image_project_id = var.source_image_project_id != "" ? [var.source_image_project_id] : null

  network    = var.network
  subnetwork = var.subnetwork != "" ? var.subnetwork : null

  omit_external_ip = var.omit_external_ip
  use_internal_ip  = var.use_internal_ip
  tags             = var.tags

  image_name        = "${var.image_name}-{{timestamp}}"
  image_family      = var.image_family
  image_description = "GitHub Actions runner image for scaleset (Windows Server 2025 Core, boot-optimized)"

  disk_size = var.disk_size
  disk_type = "pd-ssd"

  communicator   = "winrm"
  winrm_username = "packer"
  winrm_insecure = true
  winrm_use_ssl  = true

  # GCP generates a random password and configures WinRM automatically
  # when using the googlecompute builder with the winrm communicator.
  metadata = {
    sysprep-specialize-script-cmd = "winrm quickconfig -quiet & net user /add packer & net localgroup administrators packer /add & winrm set winrm/config/service/auth @{Basic=\"true\"}"
  }
}

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

build {
  sources = ["source.googlecompute.runner-windows"]

  # Upload the startup script first so the install script can reference it.
  provisioner "file" {
    source      = "${path.root}/scripts/startup.ps1"
    destination = "C:\\scaleset\\startup.ps1"
  }

  provisioner "powershell" {
    script = "${path.root}/scripts/install-runner.ps1"
    environment_vars = [
      "RUNNER_VERSION=${var.runner_version}",
      "DOCKER_VERSION=${var.docker_version}",
    ]
  }

  # Reboot after Containers feature install (required for vmcompute.dll).
  provisioner "windows-restart" {
    restart_timeout = "15m"
  }

  # Phase 2: Register dockerd service and cleanup (runs after reboot).
  provisioner "powershell" {
    script = "${path.root}/scripts/install-runner-phase2.ps1"
    environment_vars = [
      "DISM_CLEANUP=${var.dism_cleanup}",
    ]
  }

  # Verify installation.
  provisioner "powershell" {
    inline = [
      "Write-Host 'Verifying Docker installation...'",
      "Start-Service Docker -ErrorAction SilentlyContinue",
      "docker version",
      "Write-Host 'Verifying runner installation...'",
      "Test-Path C:\\actions-runner\\run.cmd",
      "Write-Host 'Verifying scheduled task...'",
      "Get-ScheduledTask -TaskName ScalesetRunner",
      "Write-Host 'Image build complete.'",
    ]
  }
}
