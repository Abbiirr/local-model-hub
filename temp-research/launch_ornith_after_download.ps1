$ErrorActionPreference = 'Stop'
$root = 'K:\projects\llm_models\models\ornith-awq-manual'
$resultPath = Join-Path $PSScriptRoot 'ornith_vllm_watch_result.json'
$weights = @(
    (Join-Path $root 'model-00001-of-00002.safetensors'),
    (Join-Path $root 'model-00002-of-00002.safetensors')
)

while ($true) {
    $ready = $true
    foreach ($path in $weights) {
        if (-not (Test-Path -LiteralPath $path)) { $ready = $false; break }
    }
    if ($ready) { break }
    Start-Sleep -Seconds 10
}

$existing = docker ps -aq -f 'name=^ornith-awq-vllm-test$'
if ($existing) { docker rm -f ornith-awq-vllm-test | Out-Null }
docker run -d --name ornith-awq-vllm-test --gpus all -p 8001:8000 -v "${root}:/models/ornith:ro" vllm/vllm-openai:latest /models/ornith --served-model-name Ornith-1.0-9B-AWQ-INT4 --host 0.0.0.0 --port 8000 --max-model-len 4096 --max-num-seqs 1 --max-num-batched-tokens 1024 --gpu-memory-utilization 0.75 --enforce-eager --trust-remote-code | Out-Null

while ($true) {
    try {
        $health = Invoke-WebRequest -UseBasicParsing -TimeoutSec 5 'http://127.0.0.1:8001/health'
        if ($health.StatusCode -eq 200) { break }
    }
    catch { Start-Sleep -Seconds 10 }
}

try {
    $models = Invoke-RestMethod -Method Get -TimeoutSec 20 'http://127.0.0.1:8001/v1/models'
    $body = @{ model = 'Ornith-1.0-9B-AWQ-INT4'; prompt = 'Reply with exactly: ORNITH_VLLM_OK'; max_tokens = 12; temperature = 0 } | ConvertTo-Json
    $completion = Invoke-RestMethod -Method Post -TimeoutSec 180 -ContentType 'application/json' -Body $body 'http://127.0.0.1:8001/v1/completions'
    [pscustomobject]@{ passed = $true; checked_at = (Get-Date).ToUniversalTime().ToString('o'); model_ids = @($models.data | ForEach-Object { $_.id }); completion_text = $completion.choices[0].text; raw_completion = $completion } | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $resultPath -Encoding utf8
}
catch {
    [pscustomobject]@{ passed = $false; checked_at = (Get-Date).ToUniversalTime().ToString('o'); error = $_.Exception.Message } | ConvertTo-Json | Set-Content -LiteralPath $resultPath -Encoding utf8
}
