param(
    [ValidateSet('hot','mixed','cold','expiry')][string]$Scenario = 'hot',
    [string]$Duration = '3s',
    [int]$Trials = 3
)
$ErrorActionPreference = 'Stop'
Push-Location (Join-Path $PSScriptRoot '..')
try {
    New-Item -ItemType Directory -Force bin,bench/results | Out-Null
    go build -o bin/bench.exe ./cmd/bench
    if ($LASTEXITCODE -ne 0) { throw 'Build failed' }
    foreach ($mode in @('direct','proxy','cache','full')) {
        $records = @(& ./bin/bench.exe -mode $mode -scenario $Scenario -duration $Duration -trials $Trials -warmup 300ms -concurrency 100 -delay 50ms)
        if ($LASTEXITCODE -ne 0) { throw "Benchmark failed: $mode" }
        $records | Write-Output
        [System.IO.File]::WriteAllLines((Join-Path $PWD "bench/results/$Scenario-$mode.jsonl"), [string[]]$records, (New-Object System.Text.UTF8Encoding $false))
    }
} finally { Pop-Location }
