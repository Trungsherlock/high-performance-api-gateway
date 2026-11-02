param([string]$ResultsDirectory = (Join-Path $PSScriptRoot 'results'))
$ErrorActionPreference = 'Stop'
function Median($values) {
    $ordered = @($values | Sort-Object)
    if ($ordered.Count -eq 0) { return 0 }
    if ($ordered.Count % 2) { return $ordered[[int][math]::Floor($ordered.Count / 2)] }
    return ($ordered[$ordered.Count / 2 - 1] + $ordered[$ordered.Count / 2]) / 2
}
$rows = @()
foreach ($scenario in @('hot','mixed','cold','expiry')) {
    foreach ($mode in @('direct','proxy','cache','full')) {
        $path = Join-Path $ResultsDirectory "$scenario-$mode.jsonl"
        if (Test-Path -LiteralPath $path) {
            $rows += @(Get-Content -LiteralPath $path | Where-Object { $_.Trim() } | ForEach-Object { $_ | ConvertFrom-Json })
        }
    }
}
'| Scenario / mode | Successful RPS median [range] | p50 ms | p95 ms | p99 ms | Error % | Backend calls median | CPU seconds | RSS MiB |'
'|---|---|---|---|---|---|---|---|---|'
foreach ($group in ($rows | Group-Object scenario,mode)) {
    $r = @($group.Group)
    $requests = ($r | Measure-Object requests -Sum).Sum
    $errors = ($r | Measure-Object errors -Sum).Sum
    $rps = @($r.successful_rps | Sort-Object)
    $line = '| {0} / {1} | {2:F0} [{3:F0}–{4:F0}] | {5:F2} | {6:F2} | {7:F2} | {8:F2} | {9:F0} | {10:F2} | {11:F1} |' -f `
        $r[0].scenario,$r[0].mode,(Median $rps),$rps[0],$rps[-1],(Median $r.p50_ms),(Median $r.p95_ms),(Median $r.p99_ms), `
        (100 * $errors / $requests),(Median $r.backend_calls),(Median $r.process_cpu_seconds),((Median $r.process_rss_bytes) / 1MB)
    $line
}
