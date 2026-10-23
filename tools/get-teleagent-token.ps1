# get-teleagent-token.ps1 — 一键提取 TeleAgent 凭证（Windows）
#
# 用法（在 TeleAgent 桌面客户端已登录的电脑上运行，需要 PowerShell 7+）：
#   pwsh -ExecutionPolicy Bypass -File get-teleagent-token.ps1
#
# 输出：可直接粘贴到 .env 的三行配置，以及 token 到期时间。
#
# 原理：新版客户端把 token 存在 Electron safeStorage 里，即 Chromium os_crypt 的
#       v10 信封（AES-256-GCM），不是裸 DPAPI：
#         token.json.encryptedToken = base64( "v10" + 12B nonce + ciphertext + 16B tag )
#       解密用的 32 字节主密钥在 <数据目录>\Local State 的 os_crypt.encrypted_key，
#       那段是 base64("DPAPI" + DPAPI blob)，DPAPI 只对「主密钥」这一层生效，
#       且限定当前 Windows 用户——与客户端自身的解密路径等价，不提权、不外传。
#       （旧版明文 state.json 与「整块 DPAPI」两种历史格式仍按顺序兼容尝试。）

$ErrorActionPreference = "Stop"

if ($PSVersionTable.PSVersion.Major -lt 7) {
    Write-Host "需要 PowerShell 7+（AES-GCM 解密依赖 .NET 5+ 的 AesGcm 类型），请用 pwsh 重新运行。" -ForegroundColor Red
    exit 1
}

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

# 3. token.json（Electron safeStorage = Chromium os_crypt v10 信封）
if (-not $token) {
    $tokenFile = Join-Path $userAuthDir "token.json"
    if (-not (Test-Path $tokenFile)) {
        Write-Host "未找到 token 文件：$tokenFile" -ForegroundColor Red; exit 1
    }
    $tj = Get-Content $tokenFile -Raw | ConvertFrom-Json
    if ($tj.token) {
        # 明文兜底（历史版本 / scheme 未加密）
        $token = $tj.token
    } elseif ($tj.encryptedToken) {
        Add-Type -AssemblyName System.Security
        $blob = [Convert]::FromBase64String($tj.encryptedToken)

        $isV10 = ($blob.Length -gt 15 -and
                  [Text.Encoding]::ASCII.GetString($blob[0..2]) -eq 'v10')
        if ($isV10) {
            # 正常路径：v10 + AES-256-GCM，主密钥来自 Local State（DPAPI 保护）
            $localState = Join-Path $dataRoot "Local State"
            if (-not (Test-Path $localState)) {
                Write-Host "token 是 os_crypt v10 信封，但缺少主密钥文件：$localState" -ForegroundColor Red; exit 1
            }
            $encKey = (Get-Content $localState -Raw | ConvertFrom-Json).os_crypt.encrypted_key
            if (-not $encKey) {
                Write-Host "Local State 里没有 os_crypt.encrypted_key" -ForegroundColor Red; exit 1
            }
            $keyRaw = [Convert]::FromBase64String($encKey)
            if ([Text.Encoding]::ASCII.GetString($keyRaw[0..4]) -ne 'DPAPI') {
                Write-Host "os_crypt.encrypted_key 缺少 'DPAPI' 前缀，格式与预期不符" -ForegroundColor Red; exit 1
            }
            # DPAPI 只作用于去掉 5 字节前缀后的主密钥 blob（不是整个 token blob）
            $aesKey = [System.Security.Cryptography.ProtectedData]::Unprotect(
                $keyRaw[5..($keyRaw.Length - 1)], $null,
                [System.Security.Cryptography.DataProtectionScope]::CurrentUser)

            $nonce = $blob[3..14]                      # 12 字节
            $tag   = $blob[($blob.Length - 16)..($blob.Length - 1)]
            $ct    = $blob[15..($blob.Length - 17)]    # 去掉 3+12 头部与 16 字节 tag
            $pt    = New-Object byte[] $ct.Length
            # ArgumentList 加逗号：否则 PowerShell 会把 byte[] 拆成 32 个 byte 参数
            $aes = New-Object System.Security.Cryptography.AesGcm -ArgumentList (, $aesKey)
            try { $aes.Decrypt($nonce, $ct, $tag, $pt) } finally { $aes.Dispose() }
            $token = [Text.Encoding]::UTF8.GetString($pt)
            $aesKey = $null
        } else {
            # 历史路径：整个 blob 直接就是 DPAPI 保护的数据
            try {
                $plain = [System.Security.Cryptography.ProtectedData]::Unprotect(
                    $blob, $null, [System.Security.Cryptography.DataProtectionScope]::CurrentUser)
                $token = [Text.Encoding]::UTF8.GetString($plain)
            } catch {
                Write-Host "解密失败：token.json 既不是 os_crypt v10 也不是裸 DPAPI 格式" -ForegroundColor Red
                Write-Host "  $($_.Exception.Message)"
                exit 1
            }
        }
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
            # .LocalDateTime 而不是直接传 DateTimeOffset：New-TimeSpan -End 在
            # PowerShell 7 下不接受 DateTimeOffset（5.1 会隐式转换），传错了会被
            # 下面的 catch 静默吞掉，表现为「无法解析过期时间」。
            $exp = [DateTimeOffset]::FromUnixTimeSeconds($claims.exp).LocalDateTime
            $days = [math]::Round((New-TimeSpan -Start (Get-Date) -End $exp).TotalDays, 1)
            $expireInfo = "$($exp.ToString('yyyy-MM-dd HH:mm'))（剩余 $days 天）"
        }
    } catch {
        Write-Host "（到期时间解析失败：$($_.Exception.Message)）" -ForegroundColor DarkGray
    }
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
