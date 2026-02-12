# install-runner.ps1 -- Packer provisioner script for building GCP Windows
# runner images (boot-optimized).
#
# Installs on Windows Server 2025 Core:
#   - Git for Windows (direct download, no Chocolatey)
#   - Docker CE (static binaries from Docker)
#   - GitHub Actions runner agent
#   - Scheduled Task to run startup.ps1 on boot
#   - Disables unnecessary services for faster boot
#   - Disables Windows Defender real-time monitoring
#   - Sets High Performance power plan
#   - Aggressive cleanup for smallest image
#
# Variables (passed via Packer environment_vars):
#   RUNNER_VERSION  -- GitHub Actions runner version (e.g. "2.331.0")
#   DOCKER_VERSION  -- Docker CE version (e.g. "27.5.1")

$ErrorActionPreference = "Stop"

$RunnerVersion = $env:RUNNER_VERSION
if (-not $RunnerVersion) {
    Write-Error "RUNNER_VERSION environment variable must be set"
    exit 1
}

$DockerVersion = $env:DOCKER_VERSION
if (-not $DockerVersion) {
    Write-Error "DOCKER_VERSION environment variable must be set"
    exit 1
}

$RunnerHome = "C:\actions-runner"
$ScalesetDir = "C:\scaleset"

# ---------------------------------------------------------------------------
# Git for Windows (direct download, no Chocolatey)
# ---------------------------------------------------------------------------
Write-Host ">>> Installing Git for Windows"

$GitVersion = "2.47.1"
$GitInstaller = "Git-$GitVersion-64-bit.exe"
$GitUrl = "https://github.com/git-for-windows/git/releases/download/v$GitVersion.windows.1/$GitInstaller"
$GitInstallerPath = "$env:TEMP\$GitInstaller"

Write-Host "Downloading $GitUrl"
$webClient = New-Object System.Net.WebClient
try {
    $webClient.DownloadFile($GitUrl, $GitInstallerPath)
} finally {
    $webClient.Dispose()
}

Write-Host "Installing Git (silent)"
Start-Process -FilePath $GitInstallerPath -ArgumentList "/VERYSILENT","/NORESTART","/NOCANCEL","/SP-","/CLOSEAPPLICATIONS","/RESTARTAPPLICATIONS","/COMPONENTS=icons,ext\reg\shellhere,assoc,assoc_sh" -Wait -NoNewWindow

Remove-Item -Path $GitInstallerPath -Force

# Reload PATH to pick up git
$env:Path = [System.Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + `
            [System.Environment]::GetEnvironmentVariable("Path", "User")

# ---------------------------------------------------------------------------
# GitHub Actions Runner Agent
# ---------------------------------------------------------------------------
Write-Host ">>> Installing GitHub Actions runner agent $RunnerVersion"

$RunnerZip = "actions-runner-win-x64-$RunnerVersion.zip"
$RunnerUrl = "https://github.com/actions/runner/releases/download/v$RunnerVersion/$RunnerZip"
$RunnerZipPath = "$env:TEMP\$RunnerZip"

Write-Host "Downloading $RunnerUrl"
$webClient = New-Object System.Net.WebClient
try {
    $webClient.DownloadFile($RunnerUrl, $RunnerZipPath)
} finally {
    $webClient.Dispose()
}

Write-Host "Extracting to $RunnerHome"
New-Item -ItemType Directory -Path $RunnerHome -Force | Out-Null
Expand-Archive -Path $RunnerZipPath -DestinationPath $RunnerHome -Force
Remove-Item -Path $RunnerZipPath -Force

Write-Host "Runner agent installed"

# ---------------------------------------------------------------------------
# Docker CE (static binaries from Docker)
# ---------------------------------------------------------------------------
Write-Host ">>> Installing Docker CE $DockerVersion"

# Install the Containers feature (required for Docker on Windows Server).
Write-Host "Enabling Containers feature"
Install-WindowsFeature -Name Containers -Restart:$false

# Download Docker CE static binaries
$DockerZip = "docker-$DockerVersion.zip"
$DockerUrl = "https://download.docker.com/win/static/stable/x86_64/$DockerZip"
$DockerZipPath = "$env:TEMP\$DockerZip"

Write-Host "Downloading $DockerUrl"
$webClient = New-Object System.Net.WebClient
try {
    $webClient.DownloadFile($DockerUrl, $DockerZipPath)
} finally {
    $webClient.Dispose()
}

Write-Host "Extracting to $env:ProgramFiles"
Expand-Archive -Path $DockerZipPath -DestinationPath $env:ProgramFiles -Force
Remove-Item -Path $DockerZipPath -Force

# Add Docker to system PATH
$dockerPath = "$env:ProgramFiles\docker"
$currentPath = [Environment]::GetEnvironmentVariable("Path", [EnvironmentVariableTarget]::Machine)
if ($currentPath -notlike "*$dockerPath*") {
    [Environment]::SetEnvironmentVariable("Path", "$currentPath;$dockerPath", [EnvironmentVariableTarget]::Machine)
}
$env:Path += ";$dockerPath"

Write-Host "Docker CE binaries installed (service registration after reboot)"

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

Write-Host "ScalesetRunner scheduled task registered"

# ---------------------------------------------------------------------------
# Disable unnecessary services for faster boot
# ---------------------------------------------------------------------------
Write-Host ">>> Disabling unnecessary services"

$disableServices = @(
    "wuauserv",       # Windows Update
    "UsoSvc",         # Update Orchestrator
    "DiagTrack",      # Diagnostics Tracking (telemetry)
    "SysMain",        # Superfetch/Prefetch
    "WerSvc",         # Windows Error Reporting
    "Spooler",        # Print Spooler
    "WSearch",        # Windows Search
    "MapsBroker",     # Downloaded Maps Manager
    "lfsvc"           # Geolocation Service
)

foreach ($svc in $disableServices) {
    try {
        $service = Get-Service -Name $svc -ErrorAction SilentlyContinue
        if ($service) {
            Write-Host "Disabling service: $svc"
            Set-Service -Name $svc -StartupType Disabled -ErrorAction Stop
        }
    } catch {
        Write-Host "Could not disable $svc (may not exist on Server Core): $_"
    }
}

# ---------------------------------------------------------------------------
# Disable Windows Defender real-time monitoring
# ---------------------------------------------------------------------------
Write-Host ">>> Disabling Windows Defender real-time monitoring"

try {
    Set-MpPreference -DisableRealtimeMonitoring $true -ErrorAction Stop
    Write-Host "Real-time monitoring disabled"

    # Add path exclusions for runner and Docker
    Add-MpPreference -ExclusionPath $RunnerHome -ErrorAction Stop
    Add-MpPreference -ExclusionPath "$env:ProgramFiles\docker" -ErrorAction Stop
    Add-MpPreference -ExclusionPath "C:\ProgramData\docker" -ErrorAction Stop
    Write-Host "Defender exclusions added"
} catch {
    Write-Host "Could not configure Windows Defender: $_"
}

# ---------------------------------------------------------------------------
# Set High Performance power plan
# ---------------------------------------------------------------------------
Write-Host ">>> Setting High Performance power plan"
powercfg /setactive 8c5e7fda-e8bf-4a96-9a85-a6e23a8c635c

Write-Host ">>> Phase 1 complete (will reboot to activate Containers feature)"

