# get-teleagent-token.ps1 — 一键提取 TeleAgent 凭证（Windows）
#
# 用法（在TeleAgent桌面客户端已登录的电脑上运行）：
#   powershell -ExecutionPolicy Bypass -File get-teleagent-token.ps1
#
# 输出：可直接粘贴到 .env 的三行配置，以及 token 到期时间。
# 原理：新版客户端把 JWT 用 Windows DPAPI（Electron safeStorage）加密存储在
#       token.json 中；本脚本用当前 Windows 用户上下文解密（无需客户端配合）。

$ErrorActionPreference = "Stop"

$shareRoot = Join-Path $env:LOCALAPPDATA ".." # 占位，下面用真实路径
$dataRoot = if ($env:TELEAGENT_DATA_DIR) { $env:TELEAGENT_DATA_DIR } else { Join-Path $HOME ".local\share\TeleAgent" }

if (-not (Test-Path $dataRoot)) {
    Write-Host "未找到数据目录：$dataRoot" -ForegroundColor Red
    Write-Host "请确认已安装并登录新版 TeleAgent 桌面客户端（登录后目录才会生成）。"
    exit 1
}

# 1. 定位当前用户目录（current-owner.json 指向活跃账号）
$ownerFile = Join-Path $dataRoot "current-owner.json"
if (Test-Path $ownerFile) {
    $owner = Get-Content $ownerFile -Raw | ConvertFrom-Json
    $userDirName = $owner.ownerDir
} else {
    # 回退：取 users 下最新目录
    $userDirName = (Get-ChildItem (Join-Path $dataRoot "users") -Directory | Sort-Object LastWriteTime -Descending | Select-Object -First 1).Name
}
if (-not $userDirName) { Write-Host "未找到用户目录（users/ 为空）" -ForegroundColor Red; exit 1 }

$userAuthDir = Join-Path $dataRoot "users\$userDirName\app-auth"
$globalAuthDir = Join-Path $dataRoot "app-auth"

Write-Host "== 数据目录: $dataRoot"
Write-Host "== 用户目录: $userDirName"

# 2. deviceId / installId（明文）
$deviceId = $installId = ""
$metaFile = Join-Path $globalAuthDir "device-meta.json"
if (Test-Path $metaFile) {
    $meta = Get-Content $metaFile -Raw | ConvertFrom-Json
    $deviceId = $meta.deviceId
    $installId = $meta.installId
}

# 兼容旧版：state.json（未加密）
$stateFile = Join-Path $globalAuthDir "state.json"
$token = ""
if (Test-Path $stateFile) {
    $st = Get-Content $stateFile -Raw | ConvertFrom-Json
    if ($st.token) { $token = $st.token }
    if (-not $deviceId -and $st.deviceId) { $deviceId = $st.deviceId }
    if (-not $installId -and $st.installId) { $installId = $st.installId }
}

# 3. token.json（DPAPI 加密，新版）
if (-not $token) {
    $tokenFile = Join-Path $userAuthDir "token.json"
    if (-not (Test-Path $tokenFile)) {
        Write-Host "未找到 token 文件：$tokenFile" -ForegroundColor Red; exit 1
    }
    $tj = Get-Content $tokenFile -Raw | ConvertFrom-Json
    if ($tj.encryptedToken) {
        Add-Type -AssemblyName System.Security
        $blob = [Convert]::FromBase64String($tj.encryptedToken)
        $plain = [System.Security.Cryptography.ProtectedData]::Unprotect(
            $blob, $null, [System.Security.Cryptography.DataProtectionScope]::CurrentUser)
        $token = [Text.Encoding]::UTF8.GetString($plain)
    } elseif ($tj.token) {
        $token = $tj.token
    }
}

if (-not $token) { Write-Host "未能提取到 token" -ForegroundColor Red; exit 1 }

# 4. 解码 JWT 到期时间
$expireInfo = "（无法解析过期时间）"
$parts = $token.Split(".")
if ($parts.Count -ge 2) {
    $p = $parts[1].Replace("-", "+").Replace("_", "/")
    switch ($p.Length % 4) { 0 {} 2 { $p += "==" } 3 { $p += "=" } default { $p += "==" } }
    try {
        $claims = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($p)) | ConvertFrom-Json
        if ($claims.exp) {
            $exp = [DateTimeOffset]::FromUnixTimeSeconds($claims.exp).ToLocalTime()
            $days = [math]::Round((New-TimeSpan -Start (Get-Date) -End $exp).TotalDays, 1)
            $expireInfo = "$($exp.ToString('yyyy-MM-dd HH:mm'))（剩余 $days 天）"
        }
    } catch { }
}

# 5. 输出（可直接粘贴到 .env）
Write-Host ""
Write-Host "===== 复制以下内容到 .env =====" -ForegroundColor Green
Write-Host "TELEAGENT_TOKEN=$token"
Write-Host "TELEAGENT_DEVICE_ID=$deviceId"
Write-Host "TELEAGENT_INSTALL_ID=$installId"
Write-Host "===============================" -ForegroundColor Green
Write-Host "Token 到期：$expireInfo"
Write-Host ""
Write-Host "提示：token 约一个月过期。到期后重新运行本脚本即可，无需手动找文件。"
