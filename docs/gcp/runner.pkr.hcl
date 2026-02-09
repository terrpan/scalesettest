# runner.pkr.hcl -- Packer template for building a GCP runner image.
#
# The image is based on Ubuntu 24.04 LTS and includes:
#   - GitHub Actions runner agent
#   - Docker CE
#   - A systemd service that reads JIT config from instance metadata on boot
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
  default     = "scaleset-runner"
  description = "Name of the resulting GCP image. A timestamp suffix is appended automatically."
}

variable "image_family" {
  type        = string
  default     = "scaleset-runner"
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

# ---------------------------------------------------------------------------
# Source
# ---------------------------------------------------------------------------

source "googlecompute" "runner" {
  project_id   = var.project_id
  zone         = var.zone
  machine_type = var.machine_type

  source_image_family  = "ubuntu-2404-lts"
  source_image_project = "ubuntu-os-cloud"

  image_name        = "${var.image_name}-{{timestamp}}"
  image_family      = var.image_family
  image_description = "GitHub Actions runner image for scaleset (Ubuntu 24.04)"

  disk_size = 30
  disk_type = "pd-ssd"

  ssh_username = "packer"
}

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

build {
  sources = ["source.googlecompute.runner"]

  # Upload the startup script first so the install script can place it.
  provisioner "file" {
    source      = "${path.root}/scripts/startup.sh"
    destination = "/tmp/startup.sh"
  }

  provisioner "shell" {
    script = "${path.root}/scripts/install-runner.sh"
    environment_vars = [
      "RUNNER_VERSION=${var.runner_version}",
    ]
    execute_command = "chmod +x {{ .Path }}; sudo {{ .Vars }} {{ .Path }}"
  }
}
