$ErrorActionPreference = 'Stop'

$baseUrl = 'http://127.0.0.1:8001'
$resultPath = Join-Path $PSScriptRoot 'ornith_vllm_watch_result.json'

while ($true) {
    try {
        $health = Invoke-WebRequest -UseBasicParsing -TimeoutSec 5 "$baseUrl/health"
        if ($health.StatusCode -eq 200) { break }
    }
    catch {
        Start-Sleep -Seconds 10
    }
}

try {
    $models = Invoke-RestMethod -Method Get -TimeoutSec 20 "$baseUrl/v1/models"
    $body = @{
        model = 'Ornith-1.0-9B-AWQ-INT4'
        prompt = 'Reply with exactly: ORNITH_VLLM_OK'
        max_tokens = 12
        temperature = 0
    } | ConvertTo-Json
    $completion = Invoke-RestMethod -Method Post -TimeoutSec 180 -ContentType 'application/json' -Body $body "$baseUrl/v1/completions"
    [pscustomobject]@{
        passed = $true
        checked_at = (Get-Date).ToUniversalTime().ToString('o')
        model_ids = @($models.data | ForEach-Object { $_.id })
        completion_text = $completion.choices[0].text
        raw_completion = $completion
    } | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $resultPath -Encoding utf8
}
catch {
    [pscustomobject]@{
        passed = $false
        checked_at = (Get-Date).ToUniversalTime().ToString('o')
        error = $_.Exception.Message
    } | ConvertTo-Json | Set-Content -LiteralPath $resultPath -Encoding utf8
}
