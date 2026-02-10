# install-runner.ps1 -- Packer provisioner script for building GCP Windows
# runner images.
#
# Installs on Windows Server 2022:
#   - Chocolatey package manager
#   - Git for Windows
#   - Docker (Windows containers via DockerMsftProvider)
#   - GitHub Actions runner agent
#   - Scheduled Task to run startup.ps1 on boot
#
# Variables (passed via Packer environment_vars):
#   RUNNER_VERSION  -- GitHub Actions runner version (e.g. "2.321.0")

$ErrorActionPreference = "Stop"

$RunnerVersion = $env:RUNNER_VERSION
if (-not $RunnerVersion) {
    Write-Error "RUNNER_VERSION environment variable must be set"
    exit 1
}

$RunnerHome = "C:\actions-runner"
$ScalesetDir = "C:\scaleset"

# ---------------------------------------------------------------------------
# Chocolatey
# ---------------------------------------------------------------------------
Write-Host ">>> Installing Chocolatey"
Set-ExecutionPolicy Bypass -Scope Process -Force
[System.Net.ServicePointManager]::SecurityProtocol = [System.Net.SecurityProtocolType]::Tls12
Invoke-Expression ((New-Object System.Net.WebClient).DownloadString('https://community.chocolatey.org/install.ps1'))

# Reload PATH so choco is available.
$env:Path = [System.Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + `
            [System.Environment]::GetEnvironmentVariable("Path", "User")

# ---------------------------------------------------------------------------
# Git
# ---------------------------------------------------------------------------
Write-Host ">>> Installing Git"
choco install git -y --no-progress
# Reload PATH for git.
$env:Path = [System.Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + `
            [System.Environment]::GetEnvironmentVariable("Path", "User")

# ---------------------------------------------------------------------------
# Docker (Windows containers)
# ---------------------------------------------------------------------------
Write-Host ">>> Installing Docker (Windows containers)"

# Install the Containers feature (required for Docker on Windows Server).
Install-WindowsFeature -Name Containers -Restart:$false

# Install Docker via DockerMsftProvider.
Install-PackageProvider -Name NuGet -MinimumVersion 2.8.5.201 -Force
Install-Module DockerMsftProvider -Force
Install-Package Docker -ProviderName DockerMsftProvider -Force

# Docker service will start after reboot with the Containers feature.
# Packer will reboot automatically if needed.

# ---------------------------------------------------------------------------
# GitHub Actions runner agent
# ---------------------------------------------------------------------------
Write-Host ">>> Installing GitHub Actions runner $RunnerVersion"

New-Item -ItemType Directory -Path $RunnerHome -Force | Out-Null

$RunnerZip = "actions-runner-win-x64-$RunnerVersion.zip"
$RunnerUrl = "https://github.com/actions/runner/releases/download/v$RunnerVersion/$RunnerZip"
$RunnerZipPath = "$env:TEMP\$RunnerZip"

Write-Host "Downloading $RunnerUrl"
Invoke-WebRequest -Uri $RunnerUrl -OutFile $RunnerZipPath -UseBasicParsing

Write-Host "Extracting to $RunnerHome"
Expand-Archive -Path $RunnerZipPath -DestinationPath $RunnerHome -Force
Remove-Item -Path $RunnerZipPath -Force

# ---------------------------------------------------------------------------
# Startup script & Scheduled Task
# ---------------------------------------------------------------------------
Write-Host ">>> Installing startup script and scheduled task"

# The startup.ps1 script is uploaded by the Packer file provisioner
# to C:\scaleset\startup.ps1 before this script runs.

# Create a Scheduled Task that runs startup.ps1 at system boot.
# Runs as SYSTEM so it has full access without needing a password.
$Action = New-ScheduledTaskAction `
    -Execute "powershell.exe" `
    -Argument "-ExecutionPolicy Bypass -NoProfile -File `"$ScalesetDir\startup.ps1`""

$Trigger = New-ScheduledTaskTrigger -AtStartup

$Principal = New-ScheduledTaskPrincipal `
    -UserId "SYSTEM" `
    -LogonType ServiceAccount `
    -RunLevel Highest

$Settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries `
    -StartWhenAvailable `
    -ExecutionTimeLimit ([TimeSpan]::Zero)

Register-ScheduledTask `
    -TaskName "ScalesetRunner" `
    -Action $Action `
    -Trigger $Trigger `
    -Principal $Principal `
    -Settings $Settings `
    -Force

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------
Write-Host ">>> Cleaning up"
choco cache remove -y 2>$null

Write-Host ">>> Done"
