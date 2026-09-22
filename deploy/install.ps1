# PanDrive Windows installer - downloads a release build from GitHub Releases (checksum-verified),
# optionally installs the Go toolchain (only for building from source) and Caddy, and registers a
# Windows service.
#
#   powershell -ExecutionPolicy Bypass -File install.ps1              # latest
#   powershell -ExecutionPolicy Bypass -File install.ps1 -Version v0.24.4
#   powershell -ExecutionPolicy Bypass -File install.ps1 -WithGo -WithCaddy
param(
  [string]$Version = "",
  [switch]$WithGo,
  [switch]$WithCaddy,
  [string]$Domain = "",
  [string]$InstallDir = "$env:LOCALAPPDATA\PanDrive",
  [string]$Repo = "jhopan/pandrive"
)
$ErrorActionPreference = "Stop"

function Get-Arch { if ([Environment]::Is64BitOperatingSystem) { "amd64" } else { "arm64" } }
$arch = Get-Arch
$asset = "pandrive-windows-$arch.exe"

function Invoke-Fetch($url, $out) {
  for ($i = 1; $i -le 3; $i++) {
    try { Invoke-WebRequest -Uri $url -OutFile $out -UseBasicParsing -ErrorAction Stop; return }
    catch { Start-Sleep -Seconds 3 }
  }
  throw "download failed after 3 attempts: $url"
}

# resolve version
if (-not $Version) {
  $rel = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest"
  $Version = $rel.tag_name
}
if (-not $Version.StartsWith("v")) { $Version = "v$Version" }
$base = "https://github.com/$Repo/releases/download/$Version"

Write-Host "==> Downloading $Version ($asset)"
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
$exe = Join-Path $InstallDir $asset
Invoke-Fetch "$base/$asset" $exe
$sums = Join-Path $InstallDir "SHA256SUMS.tmp"
Invoke-Fetch "$base/SHA256SUMS" $sums
$expected = (Select-String -Path $sums -Pattern " $([regex]::Escape($asset))$").Line.Split(" ")[0]
$actual = (Get-FileHash -Path $exe -Algorithm SHA256).Hash.ToLower()
if ($expected.ToLower() -ne $actual) { throw "checksum verification failed - nothing changed" }
Write-Host "==> Checksum OK"

if (Test-Path $exe) { } # exe replaced in place
Copy-Item $exe (Join-Path $InstallDir "pandrive.exe") -Force
Set-Content -Path (Join-Path $InstallDir ".installed-version") -Value $Version

# seed .env once
$envFile = Join-Path $InstallDir ".env"
if (-not (Test-Path $envFile)) {
  $jwt = -join ((1..64) | ForEach-Object { "{0:x}" -f (Get-Random -Max 16) })
  $key = -join ((1..64) | ForEach-Object { "{0:x}" -f (Get-Random -Max 16) })
  @"
APP_PORT=4000
APP_BIND=127.0.0.1
DATABASE_URL=data/pandrive.db
JWT_ACCESS_SECRET=$jwt
TOKEN_ENCRYPTION_KEY=$($key.Substring(0,32))
"@ | Set-Content -Path $envFile -Encoding ASCII
  Write-Host "==> Wrote fresh $envFile (random secrets)"
}

# service
$svcName = "PanDrive"
$svc = Get-Service -Name $svcName -ErrorAction SilentlyContinue
$bin = Join-Path $InstallDir "pandrive.exe"
if ($svc) { Stop-Service $svcName -Force -ErrorAction SilentlyContinue; & sc.exe delete $svcName | Out-Null }
& sc.exe create $svcName binPath= "`"$bin`"" start= auto DisplayName= "PanDrive gateway" | Out-Null
& sc.exe description $svcName "PanDrive Drive gateway (GitHub Releases)" | Out-Null
Start-Service $svcName
Start-Sleep -Seconds 3
try {
  $health = Invoke-WebRequest -Uri "http://127.0.0.1:4000/health" -UseBasicParsing -TimeoutSec 5
  Write-Host "==> Service $svcName running and healthy ($($health.StatusCode))"
} catch {
  Write-Host "==> WARNING: service may not be healthy yet (Get-EventLog / logs)" -ForegroundColor Yellow
}

if ($WithGo) {
  if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    if (Get-Command winget -ErrorAction SilentlyContinue) { winget install -e --id GoLang.Go }
    else { Write-Host "Install Go from https://go.dev/dl/ (only needed to build from source)" }
  } else { Write-Host "==> Go already installed" }
  Write-Host "    Note: Go is only needed to BUILD PanDrive from source - the app itself is self-contained."
}

if ($WithCaddy) {
  if (-not (Get-Command caddy -ErrorAction SilentlyContinue)) {
    if (Get-Command winget -ErrorAction SilentlyContinue) { winget install -e --id CaddyServer.Caddy }
    else { Write-Host "Install Caddy from https://caddyserver.com" }
  }
  if (-not $Domain) { $Domain = "drive.example.com" }
  $caddyfile = Join-Path $InstallDir "Caddyfile"
  @"
$Domain {
    reverse_proxy 127.0.0.1:4000
}
"@ | Set-Content -Path $caddyfile -Encoding ASCII
  Write-Host "==> Caddyfile: $caddyfile (domain: $Domain). Run: caddy start --config `"$caddyfile`" (ports 80/443, DNS pointed here)"
}

Write-Host "==> PanDrive $Version installed at $bin  - open http://127.0.0.1:4000"
Write-Host "    First-run admin password is printed once in the service log."
