# runner.pkr.hcl -- Packer template for building a GCP runner image.
#
# The image is based on Ubuntu 24.04 LTS and includes:
#   - GitHub Actions runner agent
#   - Docker CE
#   - A systemd service that reads JIT config from instance metadata on boot
#
# Build:
#   packer init .
#   packer build -var-file=your-env.pkrvars.hcl .

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
  default     = "2.331.0"
  description = "GitHub Actions runner agent version.
}

variable "machine_type" {
  type        = string
  default     = "e2-medium"
  description = "Machine type for the build VM."
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
  default     = "ubuntu-2404-lts-amd64"
  description = "Source image family to use for the build VM."
}

variable "source_image_project_id" {
  type        = string
  default     = "ubuntu-os-cloud"
  description = "GCP project containing the source image. If empty, Packer searches well-known public projects."
}

variable "omit_external_ip" {
  type        = bool
  default     = false
  description = "Do not assign an external IP to the build VM. Requires use_internal_ip and VPC connectivity (VPN, IAP, or same-network build host)."
}

variable "use_internal_ip" {
  type        = bool
  default     = false
  description = "SSH to the build VM via its internal IP. Requires network connectivity to the VPC."
}

variable "tags" {
  type        = list(string)
  default     = []
  description = "Network tags to apply to the build VM (e.g. for firewall rules)."
}

# ---------------------------------------------------------------------------
# Source
# ---------------------------------------------------------------------------

source "googlecompute" "runner" {
  project_id   = var.project_id
  zone         = var.zone
  machine_type = var.machine_type

  source_image_family     = var.source_image_family
  source_image_project_id = var.source_image_project_id != "" ? [var.source_image_project_id] : null

  network    = var.network
  subnetwork = var.subnetwork != "" ? var.subnetwork : null

  omit_external_ip = var.omit_external_ip
  use_internal_ip  = var.use_internal_ip
  use_os_login     = false
  tags             = var.tags

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
