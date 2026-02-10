# example.pkrvars.hcl -- Example variable file for runner.pkr.hcl.
#
# Copy this file and fill in the values for your environment:
#   cp example.pkrvars.hcl my-env.pkrvars.hcl
#
# Then build with:
#   packer build -var-file=my-env.pkrvars.hcl .

# Required -----------------------------------------------------------------

project_id = "my-project"

# Optional -- override any defaults below -----------------------------------

# zone         = "us-central1-a"
# machine_type = "e2-medium"
# image_name   = "scaleset-runner"
# image_family = "scaleset-runner"

# Runner agent version (https://github.com/actions/runner/releases)
# Pin to a specific version to avoid deprecation issues:
# runner_version = "2.323.0"

# Network / connectivity ---------------------------------------------------

# network    = "default"
# subnetwork = ""

# If your org restricts public images, point to an allowed image project:
# source_image_family      = "ubuntu-2404-lts-amd64"
# source_image_project_id  = "my-org-images-project"

# Private VPC (no external IP) -- requires VPN, IAP, or in-VPC build host:
# omit_external_ip = true
# use_internal_ip  = true
# tags             = ["allow-iap-ssh"]
