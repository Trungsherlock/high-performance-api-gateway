# Exploratory V2.2 sweep followed by repeated comparison cases. No V1 files overwritten.
param([switch]$SkipBuild)
$ErrorActionPreference='Stop'
$root=Split-Path $PSScriptRoot -Parent
Set-Location $root
if(!$SkipBuild){foreach($name in @('gateway','mockbackend','loadgen')){& go build -o "bin/$name.exe" "./cmd/$name";if($LASTEXITCODE -ne 0){throw "Build failed: $name"}}}
$index=Join-Path $root ('bench/results/v2/v22-study-'+(Get-Date -Format 'yyyyMMdd-HHmmss')+'.jsonl')
function Case([string]$Name,[int]$C=32,[int]$Q=32,[int]$W=50,[int]$R=1200,[int]$Delay=50,[int]$Seconds=4,[int]$Trials=1,[int]$SlowAfter=0,[int]$SlowFor=0){
 $arguments=@('-NoProfile','-File','bench/run-v2.ps1','-SkipBuild','-Observe','-Workload','unique','-Concurrency',512,'-Mode','full','-Arrival','scheduled','-Warmup',1,'-Seconds',$Seconds,'-Trials',$Trials,'-Rate',$R,'-DelayMS',$Delay,'-MaxConcurrent',$C,'-MaxWaiters',$Q,'-PermitWaitMS',$W,'-SlowAfterSeconds',$SlowAfter,'-SlowForSeconds',$SlowFor,'-SlowDelayMS',200,'-Label',"v22-$Name")
 $output=& powershell @arguments
 if($LASTEXITCODE -ne 0){throw "Case failed: $Name"}
 $folder=($output | Select-Object -Last 1).Trim()
 @{name=$Name;folder=$folder;c=$C;q=$Q;waitMS=$W;rate=$R;delayMS=$Delay;seconds=$Seconds;trials=$Trials;slowAfter=$SlowAfter;slowFor=$SlowFor} | ConvertTo-Json -Compress | Add-Content -Encoding UTF8 $index
 Write-Host "Completed $Name"
}
foreach($c in @(8,16,32,64)){Case -Name "screen-c$c" -C $c}
foreach($q in @(0,8,64,-1)){Case -Name "screen-q$q" -Q $q}
foreach($w in @(0,10,100)){Case -Name "screen-w$w" -W $w}
foreach($r in @(400,800,1600)){Case -Name "screen-r$r" -R $r}
Case -Name screen-slow -R 800 -Delay 200
foreach($q in @(32,-1)){
 Case -Name "healthy-q$q" -Q $q -R 400 -Seconds 6 -Trials 3
 Case -Name "overload-q$q" -Q $q -R 1200 -Seconds 6 -Trials 3
 Case -Name "slow-q$q" -Q $q -R 800 -Delay 200 -Seconds 6 -Trials 3
 Case -Name "recovery-q$q" -Q $q -R 800 -Seconds 9 -Trials 3 -SlowAfter 4 -SlowFor 3
}
Write-Output $index
