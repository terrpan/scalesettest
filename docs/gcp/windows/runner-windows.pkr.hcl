# runner-windows.pkr.hcl -- Packer template for building a GCP Windows
# runner image.
#
# The image is based on Windows Server 2022 and includes:
#   - GitHub Actions runner agent
#   - Docker (Windows containers via DockerMsftProvider)
#   - Git for Windows
#   - A Scheduled Task that reads JIT config from instance metadata on boot
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
  default     = "2.321.0"
  description = "GitHub Actions runner agent version to install."
}

variable "machine_type" {
  type        = string
  default     = "e2-medium"
  description = "Machine type for the build VM."
}

variable "disk_size" {
  type        = number
  default     = 50
  description = "Boot disk size in GB for the build VM."
}

# ---------------------------------------------------------------------------
# Source
# ---------------------------------------------------------------------------

source "googlecompute" "runner-windows" {
  project_id   = var.project_id
  zone         = var.zone
  machine_type = var.machine_type

  source_image_family  = "windows-2022"
  source_image_project = "windows-cloud"

  image_name        = "${var.image_name}-{{timestamp}}"
  image_family      = var.image_family
  image_description = "GitHub Actions runner image for scaleset (Windows Server 2022)"

  disk_size = var.disk_size
  disk_type = "pd-ssd"

  communicator = "winrm"
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
    ]
  }

  # Reboot after Docker/Containers feature install, then verify.
  provisioner "windows-restart" {
    restart_timeout = "15m"
  }

  provisioner "powershell" {
    inline = [
      "Write-Host 'Verifying Docker installation...'",
      "Start-Service Docker -ErrorAction SilentlyContinue",
      "docker version",
      "Write-Host 'Verifying runner installation...'",
      "Test-Path C:\\actions-runner\\run.cmd",
      "Write-Host 'Image build complete.'",
    ]
  }
}
