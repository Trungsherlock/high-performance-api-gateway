param(
 [ValidateSet('proxy','full')][string]$Mode='full',
 [ValidateSet('scheduled','closed')][string]$Arrival='scheduled',
 [ValidateSet('hot','mixed','unique')][string]$Workload='mixed',
 [int]$Seconds=30,[int]$Warmup=5,[int]$Trials=3,[int]$Rate=500,
 [int]$Concurrency=100,[int]$Seed=42,[int]$DelayMS=50,[string]$Label='run',
 [switch]$Observe,[switch]$Baseline,[switch]$SkipBuild,
 [int]$MaxConcurrent=32,[int]$MaxWaiters=32,[int]$PermitWaitMS=50,
 [int]$SlowAfterSeconds=0,[int]$SlowForSeconds=0,[int]$SlowDelayMS=200
)
$ErrorActionPreference='Stop'
if($Seconds -lt 1 -or $Warmup -lt 0 -or $Trials -lt 1 -or $Rate -lt 1 -or $Concurrency -lt 1 -or $MaxConcurrent -lt 0 -or $MaxWaiters -lt -1 -or $PermitWaitMS -lt 0 -or $SlowAfterSeconds -lt 0 -or $SlowForSeconds -lt 0 -or $SlowDelayMS -lt 0 -or $Label -notmatch '^[a-zA-Z0-9_-]+$'){throw 'Invalid run settings'}
$root=Split-Path $PSScriptRoot -Parent
Set-Location $root
$run=Join-Path $root ('bench/results/v2/'+(Get-Date -Format 'yyyyMMdd-HHmmss-fff')+'-'+$Label)
New-Item -ItemType Directory -Force $run | Out-Null
New-Item -ItemType Directory -Force bin | Out-Null
if($Baseline -and $Observe){throw 'Baseline cannot enable new instrumentation'}
$serverRoot=$root
if($Baseline){$serverRoot=Join-Path $root 'bin/v1-source';if(!(Test-Path $serverRoot)){& git worktree add --detach $serverRoot v1-baseline;if($LASTEXITCODE -ne 0){throw 'Baseline export failed'}}}
if($SkipBuild -and $Baseline){throw 'SkipBuild is not allowed for V1 baseline builds'}
if(!$SkipBuild){foreach($name in @('gateway','mockbackend','loadgen')){$sourceRoot=if($name -eq 'loadgen'){$root}else{$serverRoot};Push-Location $sourceRoot;try{& go build -o "$root/bin/$name.exe" "./cmd/$name"}finally{Pop-Location};if($LASTEXITCODE -ne 0){throw "build $name failed"}}}
foreach($name in @('gateway','mockbackend','loadgen')){if(!(Test-Path "$root/bin/$name.exe")){throw "Missing binary $name; build first"}}

function Port{$l=[Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback,0);$l.Start();$p=$l.LocalEndpoint.Port;$l.Stop();return $p}
function Ready($url,$process){for($i=0;$i -lt 100;$i++){if($process.HasExited){throw "Process exited before readiness: $url"};try{$r=Invoke-WebRequest -UseBasicParsing -Uri $url -TimeoutSec 1;if($r.StatusCode -eq 200){return}}catch{};Start-Sleep -Milliseconds 100};throw "Readiness timeout: $url"}
# Explicit sharing and bounded retry tolerate transient Windows file scanners/readers.
function Append-Line {
 param([string]$Path,[Parameter(ValueFromPipeline=$true)][string]$Line)
 process {
  $bytes=[Text.Encoding]::UTF8.GetBytes($Line+[Environment]::NewLine)
  for($attempt=0;$attempt -lt 10;$attempt++){
   $stream=$null
   try{$stream=[IO.File]::Open($Path,[IO.FileMode]::Append,[IO.FileAccess]::Write,[IO.FileShare]::ReadWrite);$stream.Write($bytes,0,$bytes.Length);return}
   catch [IO.IOException]{if($attempt -eq 9){throw};Start-Sleep -Milliseconds 50}
   finally{if($null -ne $stream){$stream.Dispose()}}
  }
 }
}
$sources=@(Get-ChildItem "$serverRoot/cmd/gateway","$serverRoot/cmd/mockbackend","$serverRoot/internal" -Recurse -Filter '*.go')+@(Get-Item "$serverRoot/go.mod")
$hashes=@($sources | ForEach-Object { [pscustomobject]@{path='server/'+$_.FullName.Substring($serverRoot.Length+1);sha256=(Get-FileHash -Algorithm SHA256 -LiteralPath $_.FullName).Hash} })
$hashes+=@(Get-ChildItem "$root/cmd/loadgen" -Filter '*.go' | ForEach-Object { [pscustomobject]@{path='generator/'+$_.Name;sha256=(Get-FileHash -Algorithm SHA256 -LiteralPath $_.FullName).Hash} })
$hashes+=@([pscustomobject]@{path='bench/run-v2.ps1';sha256=(Get-FileHash -Algorithm SHA256 -LiteralPath "$root/bench/run-v2.ps1").Hash})
$hashes | ConvertTo-Json | Set-Content -Encoding UTF8 "$run/source-sha256.json"
Get-Item bin/gateway.exe,bin/mockbackend.exe,bin/loadgen.exe | ForEach-Object {[pscustomobject]@{binary=$_.Name;sha256=(Get-FileHash -Algorithm SHA256 -LiteralPath $_.FullName).Hash}} | ConvertTo-Json | Set-Content -Encoding UTF8 "$run/binary-sha256.json"
$machine=Get-CimInstance Win32_ComputerSystem
@{commit=(& git rev-parse HEAD);dirty=([bool](& git status --porcelain));diff=(& git diff | Out-String);go=(& go version);os=[Environment]::OSVersion.VersionString;cpu=(Get-CimInstance Win32_Processor).Name;ramBytes=$machine.TotalPhysicalMemory;placement='three native processes on one Windows host';mode=$Mode;arrival=$Arrival;workload=$Workload;seconds=$Seconds;warmup=$Warmup;trials=$Trials;rate=$Rate;concurrency=$Concurrency;seed=$Seed;delayMS=$DelayMS;slowAfterSeconds=$SlowAfterSeconds;slowForSeconds=$SlowForSeconds;slowDelayMS=$SlowDelayMS;payload='id 42: 31 bytes; mixed IDs variable';observe=[bool]$Observe;baseline=[bool]$Baseline;serverCommit=$(if($Baseline){& git rev-parse v1-baseline}else{& git rev-parse HEAD});gateway=@{maxConcurrent=$(if($Mode -eq 'proxy'){0}else{$MaxConcurrent});maxWaiters=$(if($Baseline){-1}else{$MaxWaiters});coalesce=($Mode -eq 'full');cacheEnabled=($Mode -eq 'full');permitWait="$($PermitWaitMS)ms";timeout='10s';cacheEntries=1000;cacheBodyBytes=16777216;maxEntryBytes=65536;ttlCap='10s'};command=$MyInvocation.Line}|ConvertTo-Json -Depth 6|Set-Content -Encoding UTF8 "$run/manifest.json"
for($trial=1;$trial -le $Trials;$trial++){
 $children=@();$oldCache=$env:CACHE_ENABLED
 try{
  $bp=Port;$gp=Port;while($gp -eq $bp){$gp=Port}
  $backendArgs=@('-addr',"127.0.0.1:$bp",'-delay',"$($DelayMS)ms")
  if(!$Baseline){$backendArgs+=@('-slow-after',"$($SlowAfterSeconds)s",'-slow-for',"$($SlowForSeconds)s",'-slow-delay',"$($SlowDelayMS)ms")}
  $backend=Start-Process -WindowStyle Hidden -PassThru -FilePath "$root/bin/mockbackend.exe" -ArgumentList $backendArgs -RedirectStandardOutput "$run/$trial-backend.stdout" -RedirectStandardError "$run/$trial-backend.log";$children+=$backend
  Ready "http://127.0.0.1:$bp/healthz" $backend
  $env:CACHE_ENABLED=if($Mode -eq 'full'){'true'}else{'false'}
  $arguments=@('-addr',"127.0.0.1:$gp",'-upstream',"http://127.0.0.1:$bp")
  if($Mode -eq 'proxy'){$arguments+=@('-coalesce=false','-max-concurrent=0')}else{$arguments+=@('-max-concurrent',$MaxConcurrent,'-permit-wait',"$($PermitWaitMS)ms")}
  if(!$Baseline){$arguments+=@('-max-waiters',$MaxWaiters)}
  if($Observe){$ap=Port;while($ap -in @($bp,$gp)){$ap=Port};$arguments+=@('-admin-addr',"127.0.0.1:$ap",'-instance',"trial-$trial",'-request-log-every','1000')}
  $gateway=Start-Process -WindowStyle Hidden -PassThru -FilePath "$root/bin/gateway.exe" -ArgumentList $arguments -RedirectStandardOutput "$run/$trial-gateway.stdout" -RedirectStandardError "$run/$trial-gateway.log";$children+=$gateway
  Ready "http://127.0.0.1:$gp/healthz" $gateway
  if($Observe){Ready "http://127.0.0.1:$ap/readyz" $gateway}
  $loadArgs=@('-target',"http://127.0.0.1:$gp",'-mode',$Arrival,'-workload',$Workload,'-duration',"$($Seconds)s",'-warmup',"$($Warmup)s",'-rate',$Rate,'-concurrency',$Concurrency,'-seed',($Seed+$trial),'-backend',"http://127.0.0.1:$bp")
  if($Observe){$loadArgs+=@('-admin',"http://127.0.0.1:$ap")}
  $generator=Start-Process -WindowStyle Hidden -PassThru -FilePath "$root/bin/loadgen.exe" -ArgumentList $loadArgs -RedirectStandardOutput "$run/$trial-load.jsonl" -RedirectStandardError "$run/$trial-load.log";$children+=$generator
  $null=$generator.Handle
  @{trial=$trial;backendPID=$backend.Id;gatewayPID=$gateway.Id;generatorPID=$generator.Id;backendArgs=$backendArgs;gatewayArgs=$arguments;generatorArgs=$loadArgs;cacheEnabled=$env:CACHE_ENABLED} | ConvertTo-Json -Depth 5 | Set-Content -Encoding UTF8 "$run/$trial-processes.json"
  $watch=[Diagnostics.Stopwatch]::StartNew()
  while(!$generator.HasExited){
   if($watch.Elapsed.TotalSeconds -gt ($Seconds+$Warmup+30)){throw 'Generator exceeded run deadline'}
   foreach($p in $children){$p.Refresh();if(!$p.HasExited){@{time=[DateTime]::UtcNow.ToString('o');pid=$p.Id;role=$p.ProcessName;cpuSeconds=$p.TotalProcessorTime.TotalSeconds;rssBytes=$p.WorkingSet64}|ConvertTo-Json -Compress|Append-Line -Path "$run/$trial-resources.jsonl"}}
   if($backend.HasExited -or $gateway.HasExited){throw 'Server exited during trial'}
   if($Observe){Invoke-RestMethod -TimeoutSec 2 "http://127.0.0.1:$ap/metrics"|ConvertTo-Json -Depth 10 -Compress|Append-Line -Path "$run/$trial-metrics.jsonl"}
   Invoke-RestMethod -TimeoutSec 2 "http://127.0.0.1:$bp/stats"|ConvertTo-Json -Compress|Append-Line -Path "$run/$trial-backend-samples.jsonl"
   Start-Sleep -Seconds 1
  }
  $generator.WaitForExit();if($generator.ExitCode -ne 0){throw 'Load generator failed'}
  Invoke-RestMethod -TimeoutSec 2 "http://127.0.0.1:$bp/stats"|ConvertTo-Json|Set-Content -Encoding UTF8 "$run/$trial-backend.json"
  if($Observe){Invoke-RestMethod -TimeoutSec 2 "http://127.0.0.1:$ap/metrics"|ConvertTo-Json -Depth 10|Set-Content -Encoding UTF8 "$run/$trial-final-metrics.json"}
  'completed' | Set-Content -Encoding UTF8 "$run/$trial-status.txt"
 }catch{$_ | Out-String | Set-Content -Encoding UTF8 "$run/$trial-failure.txt";throw}finally{foreach($p in $children){if(!$p.HasExited){Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue};$p.Dispose()};$env:CACHE_ENABLED=$oldCache}
}
'completed' | Set-Content -Encoding UTF8 "$run/status.txt"
Write-Output $run
