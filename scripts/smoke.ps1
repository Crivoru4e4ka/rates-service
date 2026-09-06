# Smoke-тест живого API (по умолчанию http://localhost:8080).
# Использование: ./scripts/smoke.ps1 [-Base http://localhost:8080] [-Pair EUR/MXN]
param(
    [string]$Base = "http://localhost:8080",
    [string]$Pair = "EUR/MXN"
)

$ErrorActionPreference = "Stop"
$failures = 0

function Check($name, $condition) {
    if ($condition) { Write-Host "PASS  $name" }
    else { Write-Host "FAIL  $name"; $script:failures++ }
}

# --- служебные эндпоинты ---
$h = Invoke-RestMethod "$Base/healthz" -TimeoutSec 5
Check "healthz" ($h.status -eq "ok")

$v = Invoke-RestMethod "$Base/version" -TimeoutSec 5
Check "version" ($null -ne $v.version -and $v.go -ne "")

# --- happy path: асинхронное обновление ---
$r = Invoke-RestMethod -Method Post -Uri "$Base/quotes" -ContentType "application/json" `
    -Body ('{"pair":"' + $Pair + '"}') -TimeoutSec 5
Check "POST /quotes -> id" ($r.id -ne "")
Check "POST /quotes -> status pending" ($r.status -eq "pending")

$n = 0
do {
    Start-Sleep -Seconds 2
    $s = Invoke-RestMethod "$Base/quotes/requests/$($r.id)" -TimeoutSec 5
} while ($s.status -eq "pending" -and $n++ -lt 15)
Check "update completed" ($s.status -eq "completed")
Check "price > 0" ($s.price -gt 0)

$q = Invoke-RestMethod "$Base/quotes?pair=$Pair" -TimeoutSec 5
Check "GET /quotes -> price" ($q.price -gt 0)

# --- идемпотентность по ключу ---
$k = [guid]::NewGuid().ToString()
$a = Invoke-RestMethod -Method Post -Uri "$Base/quotes" -ContentType "application/json" `
    -Headers @{ "Idempotency-Key" = $k } -Body ('{"pair":"' + $Pair + '"}') -TimeoutSec 5
$b = Invoke-RestMethod -Method Post -Uri "$Base/quotes" -ContentType "application/json" `
    -Headers @{ "Idempotency-Key" = $k } -Body ('{"pair":"' + $Pair + '"}') -TimeoutSec 5
Check "same Idempotency-Key -> same id" ($a.id -eq $b.id)

# --- негативные кейсы ---
$code = curl.exe -s -o NUL -w "%{http_code}" "$Base/quotes?pair=NOPE"
Check "invalid pair -> 400" ($code -eq "400")

$code = curl.exe -s -o NUL -w "%{http_code}" "$Base/quotes/requests/not-a-uuid"
Check "invalid id -> 400" ($code -eq "400")

$code = curl.exe -s -o NUL -w "%{http_code}" "$Base/quotes/requests/00000000-0000-0000-0000-0000000000ff"
Check "unknown id -> 404" ($code -eq "404")

try {
    Invoke-RestMethod -Method Post -Uri "$Base/quotes" -ContentType "application/json" `
        -Body '{"pair":"XXX/YYY"}' -TimeoutSec 5 | Out-Null
    Check "unsupported pair -> 422" $false
} catch {
    Check "unsupported pair -> 422" ([int]$_.Exception.Response.StatusCode -eq 422)
}

# --- итог ---
Write-Host ""
if ($failures -gt 0) {
    Write-Host "SMOKE FAILED: $failures"
    exit 1
}
Write-Host "SMOKE OK"
