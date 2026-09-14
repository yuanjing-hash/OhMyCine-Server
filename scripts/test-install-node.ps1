Set-StrictMode -Version 3.0
$ErrorActionPreference = 'Stop'
$scriptRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
$serverRoot = Split-Path -Parent $scriptRoot
$shellInstaller = Get-Content -Raw -LiteralPath (Join-Path $scriptRoot 'install-node.sh')
$windowsInstallerPath = Join-Path $scriptRoot 'install-node.ps1'
$windowsInstaller = Get-Content -Raw -LiteralPath $windowsInstallerPath

foreach ($contract in @($shellInstaller, $windowsInstaller)) {
    if (-not $contract.Contains('__OHMYCINE_NODE_RELEASE_PUBLIC_KEY_BASE64__') -or -not $contract.Contains('__OHMYCINE_NODE_RELEASE_VERSION__')) {
        throw 'Node installer source must remain a fail-closed release template.'
    }
    foreach ($forbidden in @('winget install', 'apt-get install', 'dnf install', 'yum install', 'New-NetFirewallRule', 'netsh advfirewall', 'nginx', 'qbittorrent')) {
        if ($contract.Contains($forbidden)) { throw "Node installer contains forbidden system mutation: $forbidden" }
    }
}
if ($shellInstaller.IndexOf('openssl dgst -sha256 -verify') -gt $shellInstaller.IndexOf('sha256sum --check')) {
    throw 'Linux installer trusts the checksum before verifying the signed manifest.'
}
if ($windowsInstaller.IndexOf('$rsa.VerifyData') -gt $windowsInstaller.IndexOf('Get-FileHash')) {
    throw 'Windows installer trusts the checksum before verifying the signed manifest.'
}
if (-not $shellInstaller.Contains('ohmycine-node.service') -or -not $windowsInstaller.Contains("'OhMyCineNode'")) {
    throw 'Node installers do not use the canonical service names.'
}
foreach ($requiredTlsContract in @('openssl req -x509 -newkey rsa:3072', 'DNS:localhost', 'TLS certificate and key must be provided together')) {
    if (-not $shellInstaller.Contains($requiredTlsContract)) { throw "Linux installer is missing automatic TLS contract: $requiredTlsContract" }
}
foreach ($requiredTlsContract in @('CertificateRequest', 'RSA]::Create(3072)', "AddDnsName('localhost')", 'TLS certificate and key must be provided together')) {
    if (-not $windowsInstaller.Contains($requiredTlsContract)) { throw "Windows installer is missing automatic TLS contract: $requiredTlsContract" }
}
if ($shellInstaller.LastIndexOf('openssl req -x509 -newkey rsa:3072') -lt $shellInstaller.LastIndexOf('verify_release_payload "$MANIFEST_PATH"')) {
    throw 'Linux installer generates TLS state before release verification.'
}
if ($windowsInstaller.LastIndexOf('New-NodeTlsIdentity -Directory') -lt $windowsInstaller.LastIndexOf('Test-ReleasePayload -Manifest $downloadedManifest')) {
    throw 'Windows installer generates TLS state before release verification.'
}
if (-not $shellInstaller.Contains('${CERT_LISTEN_SAN}') -or -not $windowsInstaller.Contains('-ListenHost $listenHost')) {
    throw 'Generated Node certificates do not include the validated listen host.'
}
if ($shellInstaller.Contains('echo "$ENROLLMENT_TOKEN"') -or $windowsInstaller.Contains('Write-Host $EnrollmentToken')) {
    throw 'Node installer may print the enrollment token.'
}

$temporaryRoot = Join-Path ([IO.Path]::GetTempPath()) ('ohmycine-node-installer-test-' + [Guid]::NewGuid().ToString('N'))
[IO.Directory]::CreateDirectory($temporaryRoot) | Out-Null
try {
    $installerTokens = $null
    $installerParseErrors = $null
    $installerAst = [Management.Automation.Language.Parser]::ParseInput($windowsInstaller, [ref]$installerTokens, [ref]$installerParseErrors)
    if ($installerParseErrors.Count -ne 0) { throw "Windows installer has parse errors: $installerParseErrors" }
    foreach ($functionName in @('Fail', 'New-NodeTlsIdentity', 'Test-NodeTlsIdentity')) {
        $functionAst = $installerAst.Find({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq $functionName }, $true)
        if ($null -eq $functionAst) { throw "Windows installer function is missing: $functionName" }
        Invoke-Expression $functionAst.Extent.Text
    }
    Set-Item -Path Function:icacls.exe -Value { $global:LASTEXITCODE = 0 }
    try {
        $tlsDirectory = Join-Path $temporaryRoot 'generated-tls'
        $generated = New-NodeTlsIdentity -Directory $tlsDirectory -Identity 'node-test' -ListenHost '192.0.2.44'
        Test-NodeTlsIdentity -CertificatePath $generated[0] -PrivateKeyPath $generated[1]
        $certificate = [Security.Cryptography.X509Certificates.X509Certificate2]::CreateFromPemFile($generated[0], $generated[1])
        try {
            $publicKey = [Security.Cryptography.X509Certificates.RSACertificateExtensions]::GetRSAPublicKey($certificate)
            try {
                if ($publicKey.KeySize -lt 3072) { throw "Generated Node TLS key is only $($publicKey.KeySize) bits." }
            } finally { $publicKey.Dispose() }
            $san = ($certificate.Extensions | Where-Object { $_.Oid.Value -eq '2.5.29.17' }).Format($false)
            if (-not $san.Contains('localhost') -or -not $san.Contains('127.0.0.1') -or -not $san.Contains('192.0.2.44')) { throw "Generated Node TLS SAN is incomplete: $san" }
            $originalThumbprint = $certificate.Thumbprint
        } finally { $certificate.Dispose() }
        $reused = New-NodeTlsIdentity -Directory $tlsDirectory -Identity 'changed-node' -ListenHost '198.51.100.8'
        $reusedCertificate = [Security.Cryptography.X509Certificates.X509Certificate2]::CreateFromPemFile($reused[0], $reused[1])
        try {
            if ($reusedCertificate.Thumbprint -ne $originalThumbprint) { throw 'Repeat TLS preparation replaced the pinned Node identity.' }
        } finally { $reusedCertificate.Dispose() }
        [IO.File]::Delete($reused[1])
        $partialRejected = $false
        try { New-NodeTlsIdentity -Directory $tlsDirectory -Identity 'node-test' -ListenHost 'localhost' | Out-Null } catch { $partialRejected = $true }
        if (-not $partialRejected) { throw 'Generated TLS identity accepted a missing private-key half.' }
    } finally {
        Remove-Item -Path Function:icacls.exe -ErrorAction SilentlyContinue
    }

    $version = '9.8.7'
    $asset = "OhMyCine-Node-v$version-windows-amd64.zip"
    $archive = Join-Path $temporaryRoot $asset
    [IO.File]::WriteAllBytes($archive, [Text.Encoding]::UTF8.GetBytes('signed node archive fixture'))
    $digest = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant()
    $manifest = Join-Path $temporaryRoot "OhMyCine-Node-v$version-SHA256SUMS.txt"
    [IO.File]::WriteAllText($manifest, "$digest  $asset`n", [Text.UTF8Encoding]::new($false))

    $rsa = [Security.Cryptography.RSA]::Create(3072)
    try {
        $publicPem = $rsa.ExportSubjectPublicKeyInfoPem()
        $publicBase64 = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($publicPem))
        $signature = Join-Path $temporaryRoot "OhMyCine-Node-v$version-SHA256SUMS.txt.sig"
        $signatureBytes = $rsa.SignData(
            [IO.File]::ReadAllBytes($manifest),
            [Security.Cryptography.HashAlgorithmName]::SHA256,
            [Security.Cryptography.RSASignaturePadding]::Pkcs1
        )
        [IO.File]::WriteAllBytes($signature, $signatureBytes)
    } finally {
        $rsa.Dispose()
    }

    $rendered = $windowsInstaller.Replace('__OHMYCINE_NODE_RELEASE_PUBLIC_KEY_BASE64__', $publicBase64).Replace('__OHMYCINE_NODE_RELEASE_VERSION__', $version)
    if ($rendered.Contains('__OHMYCINE_NODE_RELEASE_')) { throw 'Installer test did not render every release marker.' }
    $renderedPath = Join-Path $temporaryRoot 'install-node-rendered.ps1'
    [IO.File]::WriteAllText($renderedPath, $rendered, [Text.UTF8Encoding]::new($false))

    $validOutput = & pwsh -NoLogo -NoProfile -File $renderedPath -Version $version -VerifyOnly -ManifestPath $manifest -SignaturePath $signature -ArchivePath $archive -AssetName $asset 2>&1
    if ($LASTEXITCODE -ne 0) { throw "Windows installer rejected a correctly signed release payload: $validOutput" }

    & pwsh -NoLogo -NoProfile -File $renderedPath -Version $version -NodeId 'node-test' -ListenAddress '192.0.2.44:4433' -DataDirectory (Join-Path $temporaryRoot 'one-sided-data') -ManagedRoot (Join-Path $temporaryRoot 'one-sided-managed') -TlsCertificateFile $archive *> $null
    if ($LASTEXITCODE -eq 0) { throw 'Windows installer accepted a certificate without its private key.' }

    [IO.File]::AppendAllText($archive, 'tampered')
    & pwsh -NoLogo -NoProfile -File $renderedPath -Version $version -VerifyOnly -ManifestPath $manifest -SignaturePath $signature -ArchivePath $archive -AssetName $asset *> $null
    if ($LASTEXITCODE -eq 0) { throw 'Windows installer accepted an archive with a checksum mismatch.' }

    [IO.File]::WriteAllText($manifest, ('0' * 64) + "  $asset`n", [Text.UTF8Encoding]::new($false))
    & pwsh -NoLogo -NoProfile -File $renderedPath -Version $version -VerifyOnly -ManifestPath $manifest -SignaturePath $signature -ArchivePath $archive -AssetName $asset *> $null
    if ($LASTEXITCODE -eq 0) { throw 'Windows installer accepted a manifest with an invalid signature.' }
} finally {
    if (Test-Path -LiteralPath $temporaryRoot) { Remove-Item -LiteralPath $temporaryRoot -Recurse -Force }
}

Write-Host 'Node installer contract and signature checks passed.'
$global:LASTEXITCODE = 0
