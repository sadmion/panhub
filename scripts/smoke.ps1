# panhub 端到端冒烟测试：启动服务 → 登录 → 挂载 → 浏览 → 上传 → STRM → WebDAV → Range 播放
# 用法：pwsh -NoProfile -ExecutionPolicy Bypass -File .\scripts\smoke.ps1
$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent
$exe = Join-Path $root 'panhub.exe'
$port = 5299
$base = "http://127.0.0.1:$port"
$fail = @()
$pass = @()

function Check($name, $cond, $detail = '') {
    if ($cond) { $script:pass += $name; Write-Host "  PASS  $name" -ForegroundColor Green }
    else { $script:fail += "$name $detail"; Write-Host "  FAIL  $name  $detail" -ForegroundColor Red }
}

# 准备工作目录
# PowerShell 5.1 默认按 ISO-8859-1 编码 -Body，中文路径会被替换成 '?'，
# 所以统一用 UTF-8 字节发送 JSON。
function Post-Json($url, $obj, $headers) {
    $json = if ($obj -is [string]) { $obj } else { $obj | ConvertTo-Json -Depth 8 }
    $bytes = [Text.Encoding]::UTF8.GetBytes($json)
    Invoke-RestMethod $url -Method Post -Headers $headers -ContentType 'application/json; charset=utf-8' -Body $bytes
}

# Windows PowerShell 5.1 的 Invoke-WebRequest 不支持 PROPFIND 等自定义方法，
# 用 .NET HttpWebRequest 直接发。
function Invoke-Dav($url, $method, $headers, $body) {
    $req = [System.Net.HttpWebRequest]::Create($url)
    $req.Method = $method
    $req.Timeout = 10000
    foreach ($k in $headers.Keys) { $req.Headers[$k] = [string]$headers[$k] }
    if ($body) {
        $bytes = [Text.Encoding]::UTF8.GetBytes($body)
        $req.ContentType = 'application/xml; charset=utf-8'
        $req.ContentLength = $bytes.Length
        $s = $req.GetRequestStream()
        $s.Write($bytes, 0, $bytes.Length)
        $s.Close()
    }
    try {
        $resp = $req.GetResponse()
        $sr = New-Object IO.StreamReader($resp.GetResponseStream())
        $text = $sr.ReadToEnd()
        $sr.Close()
        $code = [int]$resp.StatusCode
        $resp.Close()
        return @{ code = $code; body = $text }
    } catch [System.Net.WebException] {
        $r = $_.Exception.Response
        if ($r) { $code = [int]$r.StatusCode; $r.Close(); return @{ code = $code; body = '' } }
        throw
    }
}

$work = Join-Path $root '.smoke'
Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path (Join-Path $work 'local\movies') | Out-Null
New-Item -ItemType Directory -Force -Path (Join-Path $work 'strm') | Out-Null
Set-Content -Path (Join-Path $work 'local\movies\demo.mkv') -Value 'not-a-real-video' -NoNewline
Set-Content -Path (Join-Path $work 'local\movies\readme.txt') -Value 'hello'

$cfg = @"
server:
  addr: "127.0.0.1:$port"
  admin_password: "test123"
  data_dir: "$($work -replace '\\','/')/data"
  public_base_url: "$base"
mounts:
  - name: local
    type: local
    read_only: false
    config:
      root: "$($work -replace '\\','/')/local"
"@
[IO.File]::WriteAllText((Join-Path $root 'config.smoke.yaml'), $cfg, (New-Object Text.UTF8Encoding($false)))

$out = Join-Path $work 'stdout.log'
$err = Join-Path $work 'stderr.log'
$proc = Start-Process -FilePath $exe -ArgumentList '-config', 'config.smoke.yaml' `
    -WorkingDirectory $root -PassThru -RedirectStandardOutput $out -RedirectStandardError $err -WindowStyle Hidden

try {
    # 等服务端口就绪
    $ready = $false
    for ($i = 0; $i -lt 40; $i++) {
        Start-Sleep -Milliseconds 250
        try { Invoke-WebRequest "$base/" -UseBasicParsing -TimeoutSec 2 | Out-Null; $ready = $true; break } catch { }
    }
    Check '服务启动' $ready "(见 $err)"

    if ($ready) {
        # 1. 未登录必须 401
        try { Invoke-RestMethod "$base/api/status" -TimeoutSec 5 | Out-Null; Check '未登录拦截' $false }
        catch { Check '未登录拦截' ($_.Exception.Response.StatusCode.value__ -eq 401) }

        # 2. 错误密码
        try {
            Invoke-RestMethod "$base/api/auth/login" -Method Post -Body '{"password":"wrong"}' -ContentType 'application/json' | Out-Null
            Check '错误密码被拒' $false
        } catch { Check '错误密码被拒' ($_.Exception.Response.StatusCode.value__ -eq 401) }

        # 3. 登录
        $login = Invoke-RestMethod "$base/api/auth/login" -Method Post -Body '{"password":"test123"}' -ContentType 'application/json'
        Check '登录成功' ($login.token.Length -gt 10)
        $H = @{ Authorization = "Bearer $($login.token)" }

        # 4. 状态
        $st = Invoke-RestMethod "$base/api/status" -Headers $H
        Check '状态接口' ($st.version -eq '0.1.0' -and $st.mounts -eq 1) "version=$($st.version) mounts=$($st.mounts)"

        # 5. 驱动列表
        $dr = Invoke-RestMethod "$base/api/drivers" -Headers $H
        $types = ($dr.drivers | ForEach-Object { $_.type }) -join ','
        Check '驱动列表' ($types -match 'local' -and $types -match 'webdav' -and $types -match 'cloud189')

        # 5b. 139 登录接口
        $c139 = $dr.drivers | Where-Object { $_.type -eq 'cloud139' }
        Check '139 声明支持登录' ($c139.login -eq $true -and ($c139.login_modes -contains 'password') -and ($c139.login_modes -contains 'authorization'))

        try {
            Post-Json "$base/api/drivers/cloud139/login" @{ mode = 'authorization'; fields = @{ authorization = 'not-base64' } } $H | Out-Null
            Check '139 非法授权被拒' $false
        } catch { Check '139 非法授权被拒' ($_.Exception.Response.StatusCode.value__ -eq 400) "code=$($_.Exception.Response.StatusCode.value__)" }

        try {
            Post-Json "$base/api/drivers/nosuchdriver/login" @{ mode = 'password'; fields = @{} } $H | Out-Null
            Check '未知驱动登录 404' $false
        } catch { Check '未知驱动登录 404' ($_.Exception.Response.StatusCode.value__ -eq 404) "code=$($_.Exception.Response.StatusCode.value__)" }

        # 未登录不能调用驱动登录
        try {
            $body139 = @{ mode = 'authorization'; fields = @{ authorization = 'x' } } | ConvertTo-Json
            Invoke-RestMethod "$base/api/drivers/cloud139/login" -Method Post -ContentType 'application/json' -Body $body139 | Out-Null
            Check '驱动登录需鉴权' $false
        } catch { Check '驱动登录需鉴权' ($_.Exception.Response.StatusCode.value__ -eq 401) "code=$($_.Exception.Response.StatusCode.value__)" }

        # 6. 根目录 = 挂载点列表
        $rootList = Invoke-RestMethod "$base/api/fs/list?path=/" -Headers $H
        Check '根目录列出挂载' (@($rootList.entries).Count -eq 1 -and $rootList.entries[0].name -eq 'local')

        # 7. 浏览已存在的目录
        $mv = Invoke-RestMethod "$base/api/fs/list?path=/local/movies" -Headers $H
        $names = ($mv.entries | ForEach-Object { $_.name }) -join ','
        Check '列出目录内容' ($names -match 'demo.mkv' -and $names -match 'readme.txt') "names=$names"

        # 8. 新建目录
        Invoke-RestMethod "$base/api/fs/mkdir" -Method Post -Headers $H -ContentType 'application/json' -Body '{"path":"/local/newdir"}' | Out-Null
        $e = Invoke-RestMethod "$base/api/fs/stat?path=/local/newdir" -Headers $H
        Check '新建目录' ($e.is_dir -eq $true)

        # 9. 上传（用 curl 发 multipart，兼容 Windows PowerShell 5.1）
        $tmpFile = Join-Path $work 'upload.bin'
        [IO.File]::WriteAllBytes($tmpFile, (1..2048 | ForEach-Object { [byte]($_ % 251) }))
        $curlOut = & curl.exe -s -X POST "$base/api/fs/upload?path=/local" -H "Authorization: Bearer $($login.token)" -F "file=@$tmpFile"
        $up = Invoke-RestMethod "$base/api/fs/stat?path=/local/upload.bin" -Headers $H
        Check '上传文件' ($up.size -eq 2048) "size=$($up.size) curl=$curlOut"

        # 10. 下载内容一致
        $dl = Join-Path $work 'download.bin'
        Invoke-WebRequest "$base/api/fs/download?path=/local/upload.bin&token=$($login.token)" -OutFile $dl -UseBasicParsing
        $same = (Get-FileHash $tmpFile).Hash -eq (Get-FileHash $dl).Hash
        Check '下载内容一致' $same

        # 11. Range 请求（Emby 拖进度）
        $req = [System.Net.HttpWebRequest]::Create("$base/api/fs/download?path=/local/upload.bin&token=$($login.token)")
        $req.AddRange(1000, 1099)
        $resp = $req.GetResponse()
        $code = [int]$resp.StatusCode
        $len = $resp.ContentLength
        $resp.Close()
        Check 'Range 206' ($code -eq 206 -and $len -eq 100) "code=$code len=$len"

        # 12. 创建 STRM 任务并立即执行
        $body = @{
            name = 'smoke'; type = 'strm'; mount = 'local'; dir = '/movies'
            target = (Join-Path $work 'strm'); mode = 'proxy'; interval = 'manual'; enabled = $false
        } | ConvertTo-Json
        $t = Post-Json "$base/api/tasks" $body $H
        Start-Sleep -Milliseconds 300
        Invoke-RestMethod "$base/api/tasks/$($t.id)/run" -Method Post -Headers $H | Out-Null
        $strmFile = Join-Path $work 'strm\movies\demo.strm'
        Check 'STRM 文件生成' (Test-Path $strmFile)
        if (Test-Path $strmFile) {
            $content = Get-Content $strmFile -Raw
            Check 'STRM 内容含挂载前缀' ($content -match '/api/stream\?path=%2Flocal%2Fmovies%2Fdemo\.mkv') "content=$content"
            Check 'STRM 跳过非媒体' (-not (Test-Path (Join-Path $work 'strm\movies\readme.strm')))
            # 直接请求 STRM 里的地址，确认播放链路真的通（不需要登录，靠 stream_key）
            $m = [regex]::Match($content, 'https?://[^\s]+')
            if ($m.Success) {
                $sr = Invoke-WebRequest $m.Value -UseBasicParsing
                $bodyText = if ($sr.Content -is [byte[]]) { [Text.Encoding]::UTF8.GetString($sr.Content) } else { [string]$sr.Content }
                Check 'STRM 链接可播放' ($sr.StatusCode -eq 200 -and $bodyText -eq 'not-a-real-video') "code=$($sr.StatusCode) body=$bodyText"
            } else {
                Check 'STRM 链接可播放' $false 'STRM 里没有绝对地址'
            }
        }
        $tl = Invoke-RestMethod "$base/api/tasks" -Headers $H
        Check '任务记录成功' ($tl.tasks[0].last_result -eq 'success') "result=$($tl.tasks[0].last_result) err=$($tl.tasks[0].last_error)"

        # 13. WebDAV PROPFIND（Basic 认证）
        $davCred = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes('admin:test123'))
        $davBody = '<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:displayname/><d:resourcetype/></d:prop></d:propfind>'
        $dav = Invoke-Dav "$base/dav/local/movies" 'PROPFIND' @{ Authorization = "Basic $davCred"; Depth = '1' } $davBody
        Check 'WebDAV PROPFIND' ($dav.code -eq 207 -and $dav.body -match 'demo.mkv') "code=$($dav.code)"

        # 14. WebDAV 认证失败
        $anon = Invoke-Dav "$base/dav/local" 'PROPFIND' @{ Depth = '0' } $davBody
        Check 'WebDAV 拒绝匿名' ($anon.code -eq 401) "code=$($anon.code)"

        # 15. 只读挂载禁止写入
        $ro = @{ name = 'rolocal'; type = 'local'; read_only = $true; config = @{ root = (Join-Path $work 'local') } } | ConvertTo-Json
        Post-Json "$base/api/mounts" $ro $H | Out-Null
        try {
            Invoke-RestMethod "$base/api/fs/mkdir" -Method Post -Headers $H -ContentType 'application/json' -Body '{"path":"/rolocal/x"}' | Out-Null
            Check '只读挂载拒绝写入' $false
        } catch { Check '只读挂载拒绝写入' ($_.Exception.Response.StatusCode.value__ -eq 403) }

        # 16. 删除挂载点
        Invoke-RestMethod "$base/api/mounts/rolocal" -Method Delete -Headers $H | Out-Null
        $after = Invoke-RestMethod "$base/api/mounts" -Headers $H
        Check '删除挂载点' (@($after.mounts).Count -eq 1)
    }
}
finally {
    if ($proc -and -not $proc.HasExited) { Stop-Process -Id $proc.Id -Force }
    Start-Sleep -Milliseconds 300
}

Write-Host ""
Write-Host "通过 $($pass.Count) 项，失败 $($fail.Count) 项" -ForegroundColor Cyan
if ($fail.Count) {
    Write-Host "失败详情:" -ForegroundColor Red
    $fail | ForEach-Object { Write-Host "  - $_" -ForegroundColor Red }
    Write-Host "`n--- server stderr ---"
    if (Test-Path (Join-Path $work 'stderr.log')) { Get-Content (Join-Path $work 'stderr.log') }
    exit 1
}
exit 0
