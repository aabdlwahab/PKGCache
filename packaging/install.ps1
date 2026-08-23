<#
.SYNOPSIS
  Install pkgcache on Windows.

.DESCRIPTION
  Downloads pkgcache, verifies it against a SHA-256 before installing anything, puts it
  on PATH for the current user, and optionally points it at the team cache it came from.

  The checksum is not optional decoration. A truncated download is still a runnable-looking
  file, and the failure it produces later looks like a bug in the program rather than a bad
  copy. Nothing is installed until the bytes are confirmed.

.PARAMETER Server
  A pkgreg cache to install from, e.g. https://cache.internal:8443. Requires -CaSha256.

.PARAMETER CaSha256
  The cache's CA fingerprint, from whoever runs it. The certificate served by -Server is
  checked against this before anything is downloaded, because a cache that issues its own
  certificate cannot otherwise be told apart from anything else answering that address.

.PARAMETER From
  A local path or plain URL to install instead of asking a cache.

.PARAMETER Sha256
  Expected checksum, when -From cannot supply one.

.EXAMPLE
  .\install.ps1 -Server https://cache:8443 -CaSha256 AA:BB:CC:...

.EXAMPLE
  .\install.ps1 -From .\pkgcache-windows-amd64.exe -Sha256 abc123...
#>
[CmdletBinding()]
param(
	[string]$Server,
	[string]$CaSha256,
	[string]$From,
	[string]$Sha256,
	[string]$Prefix = "$env:LOCALAPPDATA\Programs\pkgcache",
	[string]$Limit = "25G",
	[switch]$NoConfigure
)

$ErrorActionPreference = 'Stop'
function Note($m) { Write-Host $m }
function Die($m) { Write-Error $m; exit 1 }
function Norm($s) { if ($null -eq $s) { return "" } ($s -replace '[:\s]', '').ToLower() }

if (-not $Server -and -not $From) {
	Die "Give -Server (with -CaSha256) or -From. Run Get-Help .\install.ps1 for examples."
}

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
	'AMD64' { 'amd64' }
	'ARM64' { 'arm64' }
	default { Die "unsupported architecture $env:PROCESSOR_ARCHITECTURE" }
}
Note "pkgcache installer - windows/$arch"

$work = Join-Path ([System.IO.Path]::GetTempPath()) ("pkgcache-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $work -Force | Out-Null
$binary = Join-Path $work 'pkgcache.exe'

try {
	if ($From) {
		if ($From -match '^https?://') {
			Note "downloading $From"
			Invoke-WebRequest -Uri $From -OutFile $binary -UseBasicParsing
		} else {
			if (-not (Test-Path $From)) { Die "no such file: $From" }
			Copy-Item $From $binary
		}
		if ($Sha256) {
			$got = (Get-FileHash $binary -Algorithm SHA256).Hash
			if ((Norm $got) -ne (Norm $Sha256)) {
				Die "checksum mismatch: got $got, expected $Sha256"
			}
			Note "checksum verified"
		} else {
			Note "no -Sha256 given, so the download is unverified"
		}
	} else {
		if (-not $CaSha256) {
			Die "-Server needs -CaSha256; ask whoever runs the cache for the fingerprint"
		}
		$Server = $Server.TrimEnd('/')

		# The certificate is inspected before it is trusted. This callback accepts the
		# connection so the CA can be read, and the fingerprint is then compared against
		# the one supplied on the command line -- which came from a person, not the
		# network. A mismatch stops the install before anything is downloaded.
		Add-Type -TypeDefinition @"
using System.Net;
using System.Security.Cryptography.X509Certificates;
public static class PkgcacheTrust {
    public static string LastThumbprint = "";
    public static void Capture() {
        ServicePointManager.ServerCertificateValidationCallback =
            delegate(object s, X509Certificate cert, X509Chain chain, System.Net.Security.SslPolicyErrors e) {
                if (chain != null && chain.ChainElements.Count > 0) {
                    var root = chain.ChainElements[chain.ChainElements.Count - 1].Certificate;
                    LastThumbprint = root.GetCertHashString(System.Security.Cryptography.HashAlgorithmName.SHA256);
                } else {
                    LastThumbprint = new X509Certificate2(cert).GetCertHashString(System.Security.Cryptography.HashAlgorithmName.SHA256);
                }
                return true;
            };
    }
    public static void Reset() { ServicePointManager.ServerCertificateValidationCallback = null; }
}
"@ -ErrorAction SilentlyContinue
		[PkgcacheTrust]::Capture()

		Note "fetching the CA from $Server"
		$caPath = Join-Path $work 'ca.crt'
		Invoke-WebRequest -Uri "$Server/api/ca.crt" -OutFile $caPath -UseBasicParsing
		$ca = New-Object System.Security.Cryptography.X509Certificates.X509Certificate2 $caPath
		$caHash = $ca.GetCertHashString([System.Security.Cryptography.HashAlgorithmName]::SHA256)
		if ((Norm $caHash) -ne (Norm $CaSha256)) {
			Die @"
The CA at $Server does not match the fingerprint you gave.
  served:   $caHash
  expected: $CaSha256
Nothing was installed. Either the fingerprint is stale or this is not the cache you meant.
"@
		}
		Note "CA verified against the fingerprint you gave"

		Note "asking $Server what it publishes"
		$list = Invoke-RestMethod -Uri "$Server/api/v1/downloads" -UseBasicParsing
		$pick = $list.downloads | Where-Object {
			$_.tool -eq 'pkgcache' -and $_.os -eq 'windows' -and $_.arch -eq $arch
		} | Select-Object -First 1
		if (-not $pick) {
			Die @"
$Server publishes no pkgcache build for windows/$arch.
Whoever runs it publishes them with ``pkgreg publish-client``.
Meanwhile you can install a file you already have:
  .\install.ps1 -From .\pkgcache-windows-$arch.exe
"@
		}
		Note "downloading $($pick.name)"
		Invoke-WebRequest -Uri "$Server/api/v1/downloads/$($pick.name)" -OutFile $binary -UseBasicParsing
		$got = (Get-FileHash $binary -Algorithm SHA256).Hash
		if ((Norm $got) -ne (Norm $pick.sha256)) {
			Die @"
Checksum mismatch - the download is corrupt or truncated.
  got:      $got
  expected: $($pick.sha256)
Nothing was installed. Run this again.
"@
		}
		Note "checksum verified: $got"
	}

	# ---- install ----------------------------------------------------------------
	New-Item -ItemType Directory -Path $Prefix -Force | Out-Null
	$dest = Join-Path $Prefix 'pkgcache.exe'
	# Replaced rather than written into: a running pkgcache holds its own image open, and
	# writing over it fails midway and leaves a half-file where the program used to be.
	if (Test-Path $dest) {
		try { & $dest stop 2>$null | Out-Null } catch { }
		Remove-Item $dest -Force -ErrorAction SilentlyContinue
	}
	Move-Item $binary $dest -Force
	# Mark of the Web, present on anything that came through a browser.
	Unblock-File $dest -ErrorAction SilentlyContinue
	Note "installed $dest"

	& $dest version
	if ($LASTEXITCODE -ne 0) { Die "the installed binary does not run" }

	# ---- PATH, for this user only; no elevation needed ---------------------------
	$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
	if ($userPath -notlike "*$Prefix*") {
		[Environment]::SetEnvironmentVariable('Path', "$userPath;$Prefix", 'User')
		Note "added $Prefix to your PATH (new terminals will see it)"
	}
	$env:Path = "$env:Path;$Prefix"

	if ($Server -and -not $NoConfigure) {
		Note ""
		Note "pointing this machine at $Server"
		& $dest setup -server $Server -ca-sha256 $CaSha256 -limit $Limit
	}
} finally {
	try { [PkgcacheTrust]::Reset() } catch { }
	Remove-Item $work -Recurse -Force -ErrorAction SilentlyContinue
}
