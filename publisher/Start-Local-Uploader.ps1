$ErrorActionPreference = 'Stop'

$publisherRoot = $PSScriptRoot
$binDir = Join-Path $publisherRoot 'bin'
$exePath = Join-Path $binDir 'xrw-local-uploader.exe'
$configPath = Join-Path $binDir 'local-uploader.env'
$examplePath = Join-Path $publisherRoot 'local-uploader.env.example'

try { $Host.UI.RawUI.WindowTitle = 'XRW Local Uploader - Close this window to stop' } catch {}

$runningUploader = Get-Process -Name 'xrw-local-uploader' -ErrorAction SilentlyContinue |
    Where-Object { $_.Path -eq $exePath }
if ($runningUploader) {
    throw '上传器已经在运行。请先关闭原来的上传终端，不要重复启动。'
}

New-Item -ItemType Directory -Path $binDir -Force | Out-Null

# Watch the whole module, not a hand written package list: the uploader also
# links internal/legacy, internal/sitealbum and internal/config, and the web
# assets are embedded, so a narrower list silently skips rebuilds.
$sourceRoots = @(
    (Join-Path $publisherRoot 'cmd'),
    (Join-Path $publisherRoot 'internal'),
    (Join-Path $publisherRoot 'go.mod'),
    (Join-Path $publisherRoot 'go.sum')
)
$latestSource = $sourceRoots |
    ForEach-Object { Get-ChildItem -LiteralPath $_ -File -Recurse -ErrorAction SilentlyContinue } |
    Sort-Object LastWriteTimeUtc -Descending |
    Select-Object -First 1
$needsBuild = -not (Test-Path -LiteralPath $exePath)
if (-not $needsBuild -and $latestSource) {
    $needsBuild = $latestSource.LastWriteTimeUtc -gt (Get-Item -LiteralPath $exePath).LastWriteTimeUtc
}

if ($needsBuild) {
    Push-Location $publisherRoot
    try {
        if (-not $env:GOPROXY) {
            $env:GOPROXY = 'https://goproxy.cn,direct'
        }
        go build -trimpath -o $exePath ./cmd/xrw-local-uploader
        if ($LASTEXITCODE -ne 0) {
            throw '上传器编译失败，未启动旧版本。'
        }
    }
    finally {
        Pop-Location
    }
}

if (-not (Test-Path -LiteralPath $configPath)) {
    Copy-Item -LiteralPath $examplePath -Destination $configPath
    Write-Host '首次运行：请填写 Bot Token、频道和 gimg 签名密钥，保存并关闭记事本。' -ForegroundColor Yellow
    Start-Process -FilePath 'notepad.exe' -ArgumentList $configPath -Wait
}

Write-Host ''
Write-Host '本地上传器正在此窗口运行。关闭这个终端窗口，服务就会停止。' -ForegroundColor Cyan
Write-Host '也可按 Ctrl+C 停止。只关闭浏览器网页不会停止服务。'
Write-Host '已保存的上传记录和快照会保留；上传中关闭可能需要重试当前未完成批次。'
Write-Host ''

Push-Location $binDir
try {
    # Run synchronously in this console; never detach into a hidden process.
    & $exePath
    if ($LASTEXITCODE -ne 0) {
        throw "上传器已退出，退出码：$LASTEXITCODE"
    }
}
finally {
    Pop-Location
}
