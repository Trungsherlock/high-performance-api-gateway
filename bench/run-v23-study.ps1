param([int]$Trials=3,[int]$Seconds=6,[int]$Warmup=1,[int]$Rate=800)
$ErrorActionPreference='Stop'
$root=Split-Path $PSScriptRoot -Parent
Set-Location $root
New-Item -ItemType Directory -Force bin | Out-Null
foreach($name in @('gateway','mockbackend','frontproxy','loadgen','burst')){& go build -o "bin/$name.exe" "./cmd/$name";if($LASTEXITCODE -ne 0){throw "Build failed: $name"}}
$index=Join-Path $root ('bench/results/v2/v23-study-'+(Get-Date -Format 'yyyyMMdd-HHmmss')+'.jsonl')
$cases=@(
 @{Scenario='burst';Gateways=2;Direct=$true},
 @{Scenario='burst';Gateways=2;Direct=$false},
 @{Scenario='failure';Gateways=2;Direct=$false},
 @{Scenario='steady';Gateways=1;Direct=$true},
 @{Scenario='steady';Gateways=1;Direct=$false},
 @{Scenario='steady';Gateways=2;Direct=$false}
)
foreach($case in $cases){
 $arguments=@('-NoProfile','-File','bench/run-v23.ps1','-SkipBuild','-Scenario',$case.Scenario,'-Gateways',$case.Gateways,'-Trials',$Trials,'-Seconds',$Seconds,'-Warmup',$Warmup,'-Rate',$Rate)
 if($case.Direct){$arguments+='-Direct'}
 $output=& powershell @arguments
 if($LASTEXITCODE -ne 0){throw "V2.3 case failed: $($case.Scenario)"}
 $folder=($output|Select-Object -Last 1).Trim();$case.folder=Split-Path $folder -Leaf
 $bytes=[Text.Encoding]::UTF8.GetBytes(($case|ConvertTo-Json -Compress)+[Environment]::NewLine)
 for($i=0;$i -lt 10;$i++){$f=$null;try{$f=[IO.File]::Open($index,[IO.FileMode]::Append,[IO.FileAccess]::Write,[IO.FileShare]::ReadWrite);$f.Write($bytes,0,$bytes.Length);break}catch [IO.IOException]{if($i -eq 9){throw};Start-Sleep -Milliseconds 50}finally{if($f){$f.Dispose()}}}
 Write-Host "Completed $($case.Scenario), gateways=$($case.Gateways), direct=$($case.Direct)"
}
Write-Output $index
