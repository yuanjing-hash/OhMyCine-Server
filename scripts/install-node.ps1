[CmdletBinding()]
param(
    [string]$Version = '__OHMYCINE_NODE_RELEASE_VERSION__',
    [string]$NodeId,
    [string]$EnrollmentToken,
    [string]$ListenAddress = '0.0.0.0:4433',
    [ValidateSet('http','https')][string]$Transport = 'https',
    [string]$TlsCertificateFile,
    [string]$TlsPrivateKeyFile,
    [string]$ClientCaFile,
    [string]$DataDirectory,
    [string]$ManagedRoot,
    [switch]$VerifyOnly,
    [string]$ManifestPath,
    [string]$SignaturePath,
    [string]$ArchivePath,
    [string]$AssetName
)

Set-StrictMode -Version 3.0
$ErrorActionPreference = 'Stop'
$script:ReleasePublicKeyBase64 = '__OHMYCINE_NODE_RELEASE_PUBLIC_KEY_BASE64__'
$script:OfficialReleaseRoot = 'https://github.com/yuanjing-hash/OhMyCine-Server/releases/download'
$script:ServiceName = 'OhMyCineNode'

function Fail([string]$Message) { throw "OhMyCine Node installation failed: $Message" }

function Assert-SafeStateDirectory([string]$Path, [string]$Label) {
    if ([string]::IsNullOrWhiteSpace($Path) -or -not [IO.Path]::IsPathFullyQualified($Path) -or $Path.IndexOfAny([char[]]"`r`n`0") -ge 0) {
        Fail "$Label must be an absolute local path"
    }
    $full = [IO.Path]::GetFullPath($Path).TrimEnd('\')
    $volumeRoot = [IO.Path]::GetPathRoot($full).TrimEnd('\')
    $unsafe = @($volumeRoot, $env:SystemRoot, $env:ProgramFiles, $env:ProgramData, $env:USERPROFILE) |
        Where-Object { -not [string]::IsNullOrWhiteSpace($_) } |
        ForEach-Object { [IO.Path]::GetFullPath($_).TrimEnd('\') }
    if ($unsafe -contains $full) { Fail "$Label must not be a broad system directory" }
}

function New-NodeTlsIdentity([string]$Directory, [string]$Identity, [string]$ListenHost) {
    $certificatePath = Join-Path $Directory 'node.crt'
    $privateKeyPath = Join-Path $Directory 'node.key'
    [IO.Directory]::CreateDirectory($Directory) | Out-Null
    & icacls.exe $Directory /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' | Out-Null
    if ($LASTEXITCODE -ne 0) { Fail 'could not secure the Node TLS directory' }
    if ((Test-Path -LiteralPath $certificatePath) -or (Test-Path -LiteralPath $privateKeyPath)) {
        if (-not (Test-Path -LiteralPath $certificatePath -PathType Leaf) -or (Get-Item -LiteralPath $certificatePath).LinkType -or
            -not (Test-Path -LiteralPath $privateKeyPath -PathType Leaf) -or (Get-Item -LiteralPath $privateKeyPath).LinkType) {
            Fail 'existing generated TLS identity is incomplete or unsafe'
        }
        foreach ($identityPath in @($certificatePath, $privateKeyPath)) {
            & icacls.exe $identityPath /inheritance:r /grant:r '*S-1-5-18:F' '*S-1-5-32-544:F' | Out-Null
            if ($LASTEXITCODE -ne 0) { Fail 'could not secure the existing Node TLS identity' }
        }
        return @($certificatePath, $privateKeyPath)
    }

    $rsa = [Security.Cryptography.RSA]::Create(3072)
    try {
        $request = [Security.Cryptography.X509Certificates.CertificateRequest]::new(
            "CN=OhMyCine Node $Identity",
            $rsa,
            [Security.Cryptography.HashAlgorithmName]::SHA256,
            [Security.Cryptography.RSASignaturePadding]::Pkcs1
        )
        $request.CertificateExtensions.Add([Security.Cryptography.X509Certificates.X509BasicConstraintsExtension]::new($false, $false, 0, $true))
        $request.CertificateExtensions.Add([Security.Cryptography.X509Certificates.X509KeyUsageExtension]::new(
            [Security.Cryptography.X509Certificates.X509KeyUsageFlags]::DigitalSignature -bor [Security.Cryptography.X509Certificates.X509KeyUsageFlags]::KeyEncipherment,
            $true
        ))
        $enhancedUsage = [Security.Cryptography.OidCollection]::new()
        [void]$enhancedUsage.Add([Security.Cryptography.Oid]::new('1.3.6.1.5.5.7.3.1'))
        $request.CertificateExtensions.Add([Security.Cryptography.X509Certificates.X509EnhancedKeyUsageExtension]::new($enhancedUsage, $false))
        $san = [Security.Cryptography.X509Certificates.SubjectAlternativeNameBuilder]::new()
        $san.AddDnsName('localhost')
        $san.AddIpAddress([Net.IPAddress]::Loopback)
        $san.AddIpAddress([Net.IPAddress]::IPv6Loopback)
        $listenIp = $null
        $normalizedHost = $ListenHost.Trim('[', ']')
        if ([Net.IPAddress]::TryParse($normalizedHost, [ref]$listenIp)) {
            $san.AddIpAddress($listenIp)
        } else {
            $san.AddDnsName($normalizedHost)
        }
        $request.CertificateExtensions.Add($san.Build())
        $certificate = $request.CreateSelfSigned([DateTimeOffset]::UtcNow.AddMinutes(-5), [DateTimeOffset]::UtcNow.AddDays(825))
        try {
            $certificatePem = [Security.Cryptography.PemEncoding]::WriteString('CERTIFICATE', $certificate.Export([Security.Cryptography.X509Certificates.X509ContentType]::Cert))
            $privateKeyPem = [Security.Cryptography.PemEncoding]::WriteString('PRIVATE KEY', $rsa.ExportPkcs8PrivateKey())
            [IO.File]::WriteAllText($certificatePath, $certificatePem, [Text.UTF8Encoding]::new($false))
            [IO.File]::WriteAllText($privateKeyPath, $privateKeyPem, [Text.UTF8Encoding]::new($false))
        } finally {
            $certificate.Dispose()
        }
    } finally {
        $rsa.Dispose()
    }
    foreach ($identityPath in @($certificatePath, $privateKeyPath)) {
        & icacls.exe $identityPath /inheritance:r /grant:r '*S-1-5-18:F' '*S-1-5-32-544:F' | Out-Null
        if ($LASTEXITCODE -ne 0) { Fail 'could not secure the generated Node TLS identity' }
    }
    return @($certificatePath, $privateKeyPath)
}

function Test-NodeTlsIdentity([string]$CertificatePath, [string]$PrivateKeyPath) {
    try {
        $identity = [Security.Cryptography.X509Certificates.X509Certificate2]::CreateFromPemFile($CertificatePath, $PrivateKeyPath)
        try {
            if (-not $identity.HasPrivateKey) { Fail 'TLS certificate and private key do not form an identity' }
        } finally {
            $identity.Dispose()
        }
    } catch {
        Fail 'TLS certificate and private key do not form a valid identity'
    }
}

function Initialize-NodeStateDirectories([string]$DataPath, [string]$ManagedPath) {
    [IO.Directory]::CreateDirectory($DataPath) | Out-Null
    [IO.Directory]::CreateDirectory($ManagedPath) | Out-Null
    & icacls.exe $DataPath /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' | Out-Null
    if ($LASTEXITCODE -ne 0) { Fail 'could not secure the Node data directory' }
}

function Assert-ReleaseTemplate {
    if ($script:ReleasePublicKeyBase64.StartsWith('__OHMYCINE_') -or $Version.StartsWith('__OHMYCINE_')) {
        Fail 'this source template is not a signed release installer'
    }
    if ($Version -notmatch '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$') {
        Fail 'version must be X.Y.Z'
    }
    if ($PSVersionTable.PSVersion.Major -lt 7) {
        Fail 'PowerShell 7 or newer is required for signed manifest verification'
    }
}

function New-ReleasePublicKey {
    try {
        $pemBytes = [Convert]::FromBase64String($script:ReleasePublicKeyBase64)
        $pem = [Text.Encoding]::UTF8.GetString($pemBytes)
        $rsa = [Security.Cryptography.RSA]::Create()
        $rsa.ImportFromPem($pem)
        if ($rsa.KeySize -lt 3072) { Fail 'embedded release public key is too small' }
        return $rsa
    } catch {
        Fail 'embedded release public key is invalid'
    }
}

function Test-ReleasePayload {
    param(
        [Parameter(Mandatory)][string]$Manifest,
        [Parameter(Mandatory)][string]$Signature,
        [Parameter(Mandatory)][string]$Archive,
        [Parameter(Mandatory)][string]$Asset
    )
    foreach ($path in @($Manifest, $Signature, $Archive)) {
        if (-not [IO.Path]::IsPathFullyQualified($path) -or -not (Test-Path -LiteralPath $path -PathType Leaf)) {
            Fail 'verification input is missing or is not an absolute file path'
        }
    }
    $escapedVersion = [Regex]::Escape($Version)
    if ($Asset -notmatch "^OhMyCine-Node-v${escapedVersion}-(windows-amd64\.zip|linux-(amd64|arm64)\.tar\.gz)$") {
        Fail 'asset name does not match the selected Node version/platform'
    }
    if ((Get-Item -LiteralPath $Manifest).Length -gt 1MB -or (Get-Item -LiteralPath $Signature).Length -gt 64KB) {
        Fail 'release manifest or signature exceeds its size limit'
    }

    # Signature verification is deliberately performed before the manifest is
    # parsed or its checksum is trusted.
    $rsa = New-ReleasePublicKey
    try {
        $manifestBytes = [IO.File]::ReadAllBytes($Manifest)
        $signatureBytes = [IO.File]::ReadAllBytes($Signature)
        $valid = $rsa.VerifyData(
            $manifestBytes,
            $signatureBytes,
            [Security.Cryptography.HashAlgorithmName]::SHA256,
            [Security.Cryptography.RSASignaturePadding]::Pkcs1
        )
    } finally {
        $rsa.Dispose()
    }
    if (-not $valid) { Fail 'release manifest signature verification failed' }

    $checksumMatches = @()
    foreach ($line in [IO.File]::ReadAllLines($Manifest, [Text.Encoding]::UTF8)) {
        $match = [Regex]::Match($line, '^([0-9a-f]{64})  ([A-Za-z0-9._-]+)$')
        if ($match.Success -and $match.Groups[2].Value -eq $Asset) {
            $checksumMatches += $match.Groups[1].Value
        }
    }
    if ($checksumMatches.Count -ne 1) { Fail 'release manifest must contain exactly one checksum for this asset' }
    $actual = (Get-FileHash -LiteralPath $Archive -Algorithm SHA256).Hash.ToLowerInvariant()
    if (-not [Security.Cryptography.CryptographicOperations]::FixedTimeEquals(
        [Text.Encoding]::ASCII.GetBytes($checksumMatches[0]),
        [Text.Encoding]::ASCII.GetBytes($actual)
    )) {
        Fail 'Node archive checksum verification failed'
    }
}

function Test-OfficialUri([Uri]$Uri) {
    if ($Uri.Scheme -ne 'https') { return $false }
    return $Uri.Host -in @('github.com', 'release-assets.githubusercontent.com', 'objects.githubusercontent.com')
}

function Receive-OfficialAsset {
    param(
        [Parameter(Mandatory)][Uri]$Uri,
        [Parameter(Mandatory)][string]$Destination,
        [Parameter(Mandatory)][long]$MaximumBytes
    )
    $handler = [Net.Http.HttpClientHandler]::new()
    $handler.AllowAutoRedirect = $false
    $client = [Net.Http.HttpClient]::new($handler)
    $client.DefaultRequestHeaders.UserAgent.ParseAdd('OhMyCine-Node-Installer/1')
    try {
        $current = $Uri
        for ($redirect = 0; $redirect -le 5; $redirect++) {
            if (-not (Test-OfficialUri $current)) { Fail 'release download left the official GitHub hosts' }
            $response = $client.GetAsync($current, [Net.Http.HttpCompletionOption]::ResponseHeadersRead).GetAwaiter().GetResult()
            try {
                $status = [int]$response.StatusCode
                if ($status -ge 300 -and $status -lt 400) {
                    if ($redirect -eq 5 -or $null -eq $response.Headers.Location) { Fail 'release download exceeded its redirect limit' }
                    $current = if ($response.Headers.Location.IsAbsoluteUri) {
                        $response.Headers.Location
                    } else {
                        [Uri]::new($current, $response.Headers.Location)
                    }
                    continue
                }
                if (-not $response.IsSuccessStatusCode) { Fail "official release download returned HTTP $status" }
                if ($response.Content.Headers.ContentLength.HasValue -and $response.Content.Headers.ContentLength.Value -gt $MaximumBytes) {
                    Fail 'release asset exceeds its size limit'
                }
                $inputStream = $response.Content.ReadAsStream()
                $output = [IO.File]::Open($Destination, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
                try {
                    $buffer = [byte[]]::new(1MB)
                    [long]$total = 0
                    while (($read = $inputStream.Read($buffer, 0, $buffer.Length)) -gt 0) {
                        $total += $read
                        if ($total -gt $MaximumBytes) { Fail 'release asset exceeds its size limit' }
                        $output.Write($buffer, 0, $read)
                    }
                } finally {
                    $output.Dispose()
                    $inputStream.Dispose()
                }
                return
            } finally {
                $response.Dispose()
            }
        }
    } finally {
        $client.Dispose()
        $handler.Dispose()
    }
    Fail 'release download failed'
}

function Expand-CheckedNodeArchive {
    param([string]$Archive, [string]$Destination, [string]$ExpectedBinary)
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $zip = [IO.Compression.ZipFile]::OpenRead($Archive)
    try {
        $seen = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
        [long]$expandedBytes = 0
        foreach ($entry in $zip.Entries) {
            $normalized = $entry.FullName.Replace('\', '/')
            if ([string]::IsNullOrWhiteSpace($normalized) -or $normalized.StartsWith('/') -or $normalized -match '[:\x00-\x1f]' -or $normalized -match '(^|/)\.\.(/|$)' -or [IO.Path]::IsPathRooted($normalized)) {
                Fail 'Node archive contains an unsafe path'
            }
            $unixMode = ($entry.ExternalAttributes -shr 16) -band 0xF000
            if ($unixMode -eq 0xA000) { Fail 'Node archive contains a symbolic link' }
            $target = [IO.Path]::GetFullPath((Join-Path $Destination $normalized))
            $root = [IO.Path]::GetFullPath($Destination).TrimEnd('\') + '\'
            if (-not $target.StartsWith($root, [StringComparison]::OrdinalIgnoreCase)) { Fail 'Node archive escapes its extraction directory' }
            if (-not $seen.Add($target)) { Fail 'Node archive contains duplicate targets' }
            $expandedBytes += $entry.Length
            if ($expandedBytes -gt 2GB) { Fail 'Node archive expands beyond its size limit' }
        }
    } finally {
        $zip.Dispose()
    }
    [IO.Compression.ZipFile]::ExtractToDirectory($Archive, $Destination)
    if (-not (Test-Path -LiteralPath $ExpectedBinary -PathType Leaf) -or (Get-Item -LiteralPath $ExpectedBinary).LinkType) {
        Fail 'verified archive does not contain the expected Node binary'
    }
}

Assert-ReleaseTemplate
if ($VerifyOnly) {
    if ([string]::IsNullOrWhiteSpace($ManifestPath) -or [string]::IsNullOrWhiteSpace($SignaturePath) -or [string]::IsNullOrWhiteSpace($ArchivePath) -or [string]::IsNullOrWhiteSpace($AssetName)) {
        Fail 'VerifyOnly requires ManifestPath, SignaturePath, ArchivePath and AssetName'
    }
    Test-ReleasePayload -Manifest $ManifestPath -Signature $SignaturePath -Archive $ArchivePath -Asset $AssetName
    Write-Host 'OhMyCine Node release payload verified.'
    exit 0
}

if ([string]::IsNullOrWhiteSpace($DataDirectory)) { $DataDirectory = Join-Path $env:ProgramData 'OhMyCine Node' }
if ([string]::IsNullOrWhiteSpace($ManagedRoot)) { $ManagedRoot = Join-Path $DataDirectory 'managed' }
if ([Runtime.InteropServices.RuntimeInformation]::OSArchitecture -ne [Runtime.InteropServices.Architecture]::X64) { Fail 'this release supports Windows amd64 only' }
if ($NodeId -notmatch '^[A-Za-z0-9._:-]{1,128}$') { Fail 'node ID is invalid' }
if ($ListenAddress -notmatch '^(\[[0-9A-Fa-f:]+\]|[A-Za-z0-9.-]+):([0-9]{1,5})$' -or [int]$Matches[2] -lt 1 -or [int]$Matches[2] -gt 65535) { Fail 'listen address must be HOST:PORT with a valid port' }
$listenHost = $Matches[1]
$listenPort = [int]$Matches[2]
$parsedListenIp = $null
$normalizedListenHost = $listenHost.Trim('[', ']')
if ($normalizedListenHost -match '^[0-9.]+$' -and -not [Net.IPAddress]::TryParse($normalizedListenHost, [ref]$parsedListenIp)) { Fail 'listen IPv4 address is invalid' }

foreach ($candidate in @($DataDirectory, $ManagedRoot)) {
    if ([string]::IsNullOrWhiteSpace($candidate) -or -not [IO.Path]::IsPathFullyQualified($candidate)) { Fail 'data and managed paths must be absolute' }
}
Assert-SafeStateDirectory -Path $DataDirectory -Label 'data directory'
Assert-SafeStateDirectory -Path $ManagedRoot -Label 'managed root'
$hasCertificate = -not [string]::IsNullOrWhiteSpace($TlsCertificateFile)
$hasPrivateKey = -not [string]::IsNullOrWhiteSpace($TlsPrivateKeyFile)
if ($hasCertificate -ne $hasPrivateKey) { Fail 'TLS certificate and key must be provided together' }
if ($hasCertificate) {
    foreach ($candidate in @($TlsCertificateFile, $TlsPrivateKeyFile)) {
        if (-not [IO.Path]::IsPathFullyQualified($candidate) -or -not (Test-Path -LiteralPath $candidate -PathType Leaf) -or (Get-Item -LiteralPath $candidate).LinkType) { Fail 'TLS certificate and key must be existing absolute regular files' }
    }
    Test-NodeTlsIdentity -CertificatePath $TlsCertificateFile -PrivateKeyPath $TlsPrivateKeyFile
}
if (-not [string]::IsNullOrWhiteSpace($ClientCaFile)) {
    if (-not [IO.Path]::IsPathFullyQualified($ClientCaFile) -or -not (Test-Path -LiteralPath $ClientCaFile -PathType Leaf) -or (Get-Item -LiteralPath $ClientCaFile).LinkType) { Fail 'client CA must be an existing absolute regular file' }
}
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = [Security.Principal.WindowsPrincipal]::new($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { Fail 'run this installer as Administrator' }
if ($EnrollmentToken -notmatch '^[A-Za-z0-9._~-]{20,512}$') { Fail 'enrollment token is invalid or truncated' }

$asset = "OhMyCine-Node-v$Version-windows-amd64.zip"
$manifestName = "OhMyCine-Node-v$Version-SHA256SUMS.txt"
$signatureName = "$manifestName.sig"
$releaseRoot = "$($script:OfficialReleaseRoot)/server-v$Version"
$temporaryRoot = Join-Path ([IO.Path]::GetTempPath()) ('ohmycine-node-install-' + [Guid]::NewGuid().ToString('N'))
[IO.Directory]::CreateDirectory($temporaryRoot) | Out-Null
try {
    $downloadedArchive = Join-Path $temporaryRoot $asset
    $downloadedManifest = Join-Path $temporaryRoot $manifestName
    $downloadedSignature = Join-Path $temporaryRoot $signatureName
    Receive-OfficialAsset -Uri "$releaseRoot/$manifestName" -Destination $downloadedManifest -MaximumBytes 1MB
    Receive-OfficialAsset -Uri "$releaseRoot/$signatureName" -Destination $downloadedSignature -MaximumBytes 64KB
    Receive-OfficialAsset -Uri "$releaseRoot/$asset" -Destination $downloadedArchive -MaximumBytes 2GB
    Test-ReleasePayload -Manifest $downloadedManifest -Signature $downloadedSignature -Archive $downloadedArchive -Asset $asset

    $extractRoot = Join-Path $temporaryRoot 'extracted'
    [IO.Directory]::CreateDirectory($extractRoot) | Out-Null
    $sourceBinary = Join-Path $extractRoot "OhMyCine-Node-v$Version-windows-amd64\ohmycine-node.exe"
    Expand-CheckedNodeArchive -Archive $downloadedArchive -Destination $extractRoot -ExpectedBinary $sourceBinary

    Initialize-NodeStateDirectories -DataPath $DataDirectory -ManagedPath $ManagedRoot
    if (-not $hasCertificate) {
        $generatedIdentity = New-NodeTlsIdentity -Directory (Join-Path $DataDirectory 'tls') -Identity $NodeId -ListenHost $listenHost
        $TlsCertificateFile = $generatedIdentity[0]
        $TlsPrivateKeyFile = $generatedIdentity[1]
        Test-NodeTlsIdentity -CertificatePath $TlsCertificateFile -PrivateKeyPath $TlsPrivateKeyFile
    }

    $existingService = Get-Service -Name $script:ServiceName -ErrorAction SilentlyContinue
    $wasRunning = $null -ne $existingService -and $existingService.Status -eq [ServiceProcess.ServiceControllerStatus]::Running
    if ($wasRunning) {
        Stop-Service -Name $script:ServiceName -Force
        (Get-Service -Name $script:ServiceName).WaitForStatus([ServiceProcess.ServiceControllerStatus]::Stopped, [TimeSpan]::FromSeconds(30))
    }
    if (Get-NetTCPConnection -State Listen -LocalPort $listenPort -ErrorAction SilentlyContinue) {
        if ($wasRunning) { Start-Service -Name $script:ServiceName }
        Fail "listen port $listenPort is already in use"
    }

    $installRoot = Join-Path $env:ProgramFiles 'OhMyCine Node'
    $stableBinary = Join-Path $installRoot 'ohmycine-node.exe'
    [IO.Directory]::CreateDirectory($installRoot) | Out-Null

    $backupBinary = Join-Path $temporaryRoot 'previous-ohmycine-node.exe'
    $hadPreviousBinary = Test-Path -LiteralPath $stableBinary -PathType Leaf
    if ($hadPreviousBinary) { Copy-Item -LiteralPath $stableBinary -Destination $backupBinary }
    Copy-Item -LiteralPath $sourceBinary -Destination $stableBinary -Force

    $serviceKey = "HKLM:\SYSTEM\CurrentControlSet\Services\$($script:ServiceName)"
    $oldEnvironment = $null
    if (Test-Path -LiteralPath $serviceKey) {
        $oldProperties = Get-ItemProperty -LiteralPath $serviceKey -Name Environment -ErrorAction SilentlyContinue
        if ($null -ne $oldProperties) { $oldEnvironment = $oldProperties.Environment }
    }
    $environment = @(
        "OMC_NODE_ID=$NodeId",
        "OMC_NODE_LISTEN=$ListenAddress",
        "OMC_NODE_TRANSPORT=$Transport",
        "OMC_NODE_DATA_DIR=$DataDirectory",
        "OMC_NODE_MANAGED_ROOT=$ManagedRoot",
        "OMC_NODE_TLS_CERT=$TlsCertificateFile",
        "OMC_NODE_TLS_KEY=$TlsPrivateKeyFile",
        "OMC_NODE_SEAL_KEY=$(Join-Path $DataDirectory 'node.seal.key')",
        "OMC_NODE_ENROLLMENT_TOKEN=$EnrollmentToken"
    )
    if (-not [string]::IsNullOrWhiteSpace($ClientCaFile)) { $environment += "OMC_NODE_CLIENT_CA=$ClientCaFile" }

    $createdService = $false
    if ($null -eq $existingService) {
        & sc.exe create $script:ServiceName binPath= ('"' + $stableBinary + '"') start= auto DisplayName= 'OhMyCine Transfer Node' | Out-Null
        if ($LASTEXITCODE -ne 0) { Fail 'could not create the Windows service' }
        $createdService = $true
    } else {
        & sc.exe config $script:ServiceName binPath= ('"' + $stableBinary + '"') start= auto DisplayName= 'OhMyCine Transfer Node' | Out-Null
        if ($LASTEXITCODE -ne 0) { Fail 'could not update the Windows service' }
    }
    New-ItemProperty -LiteralPath $serviceKey -Name Environment -PropertyType MultiString -Value $environment -Force | Out-Null
    $serviceAcl = Get-Acl -LiteralPath $serviceKey
    $serviceAcl.SetAccessRuleProtection($true, $false)
    $systemSid = [Security.Principal.SecurityIdentifier]::new('S-1-5-18')
    $administratorsSid = [Security.Principal.SecurityIdentifier]::new('S-1-5-32-544')
    $serviceAcl.AddAccessRule([Security.AccessControl.RegistryAccessRule]::new($systemSid, 'FullControl', 'ContainerInherit', 'None', 'Allow'))
    $serviceAcl.AddAccessRule([Security.AccessControl.RegistryAccessRule]::new($administratorsSid, 'FullControl', 'ContainerInherit', 'None', 'Allow'))
    Set-Acl -LiteralPath $serviceKey -AclObject $serviceAcl
    & sc.exe failure $script:ServiceName reset= 86400 actions= restart/5000/restart/15000/restart/30000 | Out-Null
    if ($LASTEXITCODE -ne 0) { Fail 'could not configure Windows service recovery' }
    try {
        Start-Service -Name $script:ServiceName
        (Get-Service -Name $script:ServiceName).WaitForStatus([ServiceProcess.ServiceControllerStatus]::Running, [TimeSpan]::FromSeconds(30))
    } catch {
        Stop-Service -Name $script:ServiceName -Force -ErrorAction SilentlyContinue
        if ($hadPreviousBinary) { Copy-Item -LiteralPath $backupBinary -Destination $stableBinary -Force } else { Remove-Item -LiteralPath $stableBinary -Force -ErrorAction SilentlyContinue }
        if ($createdService) {
            & sc.exe delete $script:ServiceName | Out-Null
        } elseif ($null -ne $oldEnvironment) {
            New-ItemProperty -LiteralPath $serviceKey -Name Environment -PropertyType MultiString -Value $oldEnvironment -Force | Out-Null
        } else {
            Remove-ItemProperty -LiteralPath $serviceKey -Name Environment -ErrorAction SilentlyContinue
        }
        if ($wasRunning -and -not $createdService) { Start-Service -Name $script:ServiceName -ErrorAction SilentlyContinue }
        Fail 'Windows service did not start; the previous binary and service configuration were restored when available'
    }
    Write-Host "OhMyCine Node v$Version is installed and running. Complete pairing from the main Server."
} finally {
    if (Test-Path -LiteralPath $temporaryRoot) { Remove-Item -LiteralPath $temporaryRoot -Recurse -Force }
}
