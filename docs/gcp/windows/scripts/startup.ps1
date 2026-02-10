# startup.ps1 -- GCP instance startup script for ephemeral Windows runners.
#
# This script is executed by a Scheduled Task on every boot.
# It reads the JIT configuration from GCP instance metadata and launches
# the GitHub Actions runner agent.

$ErrorActionPreference = "Stop"

# Read JIT config from GCP instance metadata.
try {
    $jitConfig = Invoke-RestMethod `
        -Uri "http://metadata.google.internal/computeMetadata/v1/instance/attributes/ACTIONS_RUNNER_INPUT_JITCONFIG" `
        -Headers @{"Metadata-Flavor" = "Google"} `
        -UseBasicParsing
} catch {
    Write-Error "Failed to read JIT config from instance metadata: $_"
    exit 1
}

if (-not $jitConfig) {
    Write-Error "No JIT config found in instance metadata"
    exit 1
}

$env:ACTIONS_RUNNER_INPUT_JITCONFIG = $jitConfig

Set-Location "C:\actions-runner"
& .\run.cmd
