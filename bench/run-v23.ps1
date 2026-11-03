param(
 [ValidateSet('burst','steady','failure')][string]$Scenario='burst',
 [ValidateSet(1,2)][int]$Gateways=2,[switch]$Direct,[switch]$SkipBuild,
 [int]$Trials=3,[int]$Seconds=6,[int]$Warmup=1,[int]$Rate=800,
 [int]$AggregatePermits=32,[int]$AggregateWaiters=32,[int]$Seed=42
)
$ErrorActionPreference='Stop'
$root=Split-Path $PSScriptRoot -Parent
Set-Location $root
if($Trials -lt 1 -or $Seconds -lt 1 -or $Warmup -lt 0 -or $Rate -lt 1 -or $AggregatePermits -lt 1 -or $AggregateWaiters -lt 0 -or $AggregatePermits%$Gateways -ne 0 -or $AggregateWaiters%$Gateways -ne 0){throw 'Invalid settings or indivisible aggregate budget'}
if($Scenario -eq 'failure' -and ($Direct -or $Gateways -ne 2)){throw 'Failure scenario requires two gateways and the front'}
if($Scenario -eq 'steady' -and $Direct -and $Gateways -ne 1){throw 'Direct steady case measures one gateway'}
New-Item -ItemType Directory -Force bin | Out-Null
$names=@('gateway','mockbackend','frontproxy','loadgen','burst')
if(!$SkipBuild){foreach($name in $names){& go build -o "bin/$name.exe" "./cmd/$name";if($LASTEXITCODE -ne 0){throw "Build failed: $name"}}}
foreach($name in $names){if(!(Test-Path "bin/$name.exe")){throw "Missing binary: $name"}}
$run=Join-Path $root ('bench/results/v2/'+(Get-Date -Format 'yyyyMMdd-HHmmss-fff')+"-v23-$Scenario-$Gateways-direct$([bool]$Direct)")
New-Item -ItemType Directory -Force $run | Out-Null
function Port{$l=[Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback,0);$l.Start();$p=$l.LocalEndpoint.Port;$l.Stop();return $p}
function Ready($url,$p){for($i=0;$i -lt 20;$i++){if($p.HasExited){throw "Child exited before readiness: $url"};try{if((Invoke-WebRequest -UseBasicParsing -TimeoutSec 1 $url).StatusCode -eq 200){return}}catch{};Start-Sleep -Milliseconds 100};throw "Readiness timed out: $url"}
function Json($url){return Invoke-RestMethod -TimeoutSec 2 $url}
function Save($value,$path){$value | ConvertTo-Json -Depth 15 | Set-Content -Encoding UTF8 $path}
function Append($value,$path){$bytes=[Text.Encoding]::UTF8.GetBytes(($value|ConvertTo-Json -Depth 15 -Compress)+[Environment]::NewLine);for($i=0;$i -lt 10;$i++){$f=$null;try{$f=[IO.File]::Open($path,[IO.FileMode]::Append,[IO.FileAccess]::Write,[IO.FileShare]::ReadWrite);$f.Write($bytes,0,$bytes.Length);return}catch [IO.IOException]{if($i -eq 9){throw};Start-Sleep -Milliseconds 50}finally{if($f){$f.Dispose()}}}}
function StartOwned($name,$arguments,$role,$suffix=''){
 $p=Start-Process -WindowStyle Hidden -PassThru -FilePath "$root/bin/$name.exe" -ArgumentList $arguments -RedirectStandardOutput "$run/$trial-$role$suffix.stdout" -RedirectStandardError "$run/$trial-$role$suffix.log"
 $null=$p.Handle
 $children.Add($p);$processes.Add([pscustomobject]@{role=$role;pid=$p.Id;arguments=$arguments;executable=$name;time=[DateTime]::UtcNow.ToString('o')})
 return $p
}
function Snapshot{
 $s=@{backend=(Json "$backendURL/stats");time=[DateTime]::UtcNow.ToString('o');gateways=@{}}
 foreach($g in $instances){if(!$g.Process.HasExited){$s.gateways[$g.Name]=Json "$($g.Admin)/metrics"}else{$s.gateways[$g.Name]=@{unavailable=$true}}}
 if(!$Direct){$s.front=Json "$frontAdmin/metrics"}
 return $s
}
function Sample{
 foreach($record in $processes){$p=$children | Where-Object {$_.Id -eq $record.pid} | Select-Object -First 1;$p.Refresh();if(!$p.HasExited){Append @{time=[DateTime]::UtcNow.ToString('o');role=$record.role;pid=$p.Id;cpuSeconds=$p.TotalProcessorTime.TotalSeconds;rssBytes=$p.WorkingSet64} "$run/$trial-resources.jsonl"}}
 Append (Snapshot) "$run/$trial-topology.jsonl"
}
function Generator($kind,$target,$suffix){
 if($kind -eq 'burst'){$arguments=@('-targets',$target,'-requests',100)}else{
  $arguments=@('-target',$target,'-admins',(($instances|ForEach-Object{$_.Admin}) -join ','),'-backend',$backendURL,'-workload','unique','-mode','scheduled','-duration',"$($Seconds)s",'-warmup',"$($Warmup)s",'-rate',$Rate,'-concurrency',256,'-seed',($Seed+$trial))
  if(!$Direct){$arguments+=@('-front-admin',$frontAdmin)}
 }
 $p=StartOwned $kind $arguments 'generator' $suffix
 $watch=[Diagnostics.Stopwatch]::StartNew();$next=0.0
 while(!$p.HasExited){if($watch.Elapsed.TotalSeconds -gt ($Seconds+$Warmup+15)){throw 'Generator watchdog expired'};if($watch.Elapsed.TotalSeconds -ge $next){Sample;$next=$watch.Elapsed.TotalSeconds+1};Start-Sleep -Milliseconds 100}
 $p.WaitForExit();if($p.ExitCode -ne 0){throw "Generator failed: $suffix"}
 return "$run/$trial-generator$suffix.stdout"
}
function FrontState($all){
 for($i=0;$i -lt 50;$i++){$s=Json "$frontAdmin/metrics";if($all){if(@($s.Targets|Where-Object{$_.Healthy}).Count -eq $Gateways){return}}else{if(!$s.Targets[0].Healthy -and $s.Targets[1].Healthy){return}};Start-Sleep -Milliseconds 100};throw 'Front health convergence failed'
}
$delay=if($Scenario -eq 'steady'){50}else{500}
$permit=[int]($AggregatePermits/$Gateways);$queue=[int]($AggregateWaiters/$Gateways)
Save @{scenario=$Scenario;gateways=$Gateways;direct=[bool]$Direct;trials=$Trials;seconds=$Seconds;warmup=$Warmup;rate=$Rate;seed=$Seed;delayMS=$delay;aggregatePermits=$AggregatePermits;aggregateWaiters=$AggregateWaiters;perGatewayPermits=$permit;perGatewayWaiters=$queue;permitWaitMS=50;gatewayTimeout='10s';frontTimeout='2s';frontMaxConnsPerTarget=256;frontMaxIdlePerTarget=32;probeInterval='100ms';probeTimeout='100ms';generatorTimeout='3s';cacheEntriesPerGateway=1000;cacheBodyBudgetPerGateway=16777216;cacheTTL='10s';commit=(& git rev-parse HEAD);dirty=[bool](& git status --porcelain);go=(& go version);os=[Environment]::OSVersion.VersionString;cpu=(Get-CimInstance Win32_Processor).Name;ramBytes=(Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory;placement='native processes, one Windows host'} "$run/manifest.json"
Get-ChildItem cmd,internal -Filter '*.go' -Recurse | ForEach-Object{[pscustomobject]@{path=$_.FullName.Substring($root.Length+1);sha256=(Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash}} | ConvertTo-Json | Set-Content -Encoding UTF8 "$run/source-sha256.json"
$names|ForEach-Object{[pscustomobject]@{name=$_;sha256=(Get-FileHash "bin/$_.exe" -Algorithm SHA256).Hash}} | ConvertTo-Json | Set-Content -Encoding UTF8 "$run/binary-sha256.json"
Get-FileHash bench/run-v23.ps1 -Algorithm SHA256 | ConvertTo-Json | Set-Content -Encoding UTF8 "$run/harness-sha256.json"
for($trial=1;$trial -le $Trials;$trial++){
 $children=[Collections.Generic.List[Diagnostics.Process]]::new();$processes=[Collections.Generic.List[object]]::new();$instances=[Collections.Generic.List[object]]::new();$oldCache=$env:CACHE_ENABLED
 try{
  $bp=Port;$backendURL="http://127.0.0.1:$bp";$backend=StartOwned 'mockbackend' @('-addr',"127.0.0.1:$bp",'-delay',"$($delay)ms") 'backend';Ready "$backendURL/healthz" $backend
  $env:CACHE_ENABLED='true'
  for($i=1;$i -le $Gateways;$i++){
   $gp=Port;$ap=Port;while($ap -eq $gp){$ap=Port}
   $data="http://127.0.0.1:$gp";$adminURL="http://127.0.0.1:$ap";$id="gateway-$i"
   $arguments=@('-addr',"127.0.0.1:$gp",'-admin-addr',"127.0.0.1:$ap",'-upstream',$backendURL,'-instance',$id,'-max-concurrent',$permit,'-max-waiters',$queue,'-permit-wait','50ms','-request-log-every',1000)
   $g=StartOwned 'gateway' $arguments $id;Ready "$data/healthz" $g;Ready "$adminURL/readyz" $g
   $instances.Add([pscustomobject]@{Name=$id;URL=$data;Admin=$adminURL;Process=$g;Arguments=$arguments})
  }
  if(!$Direct){$fp=Port;$fa=Port;while($fa -eq $fp){$fa=Port};$frontURL="http://127.0.0.1:$fp";$frontAdmin="http://127.0.0.1:$fa";$origins=($instances|ForEach-Object{$_.URL})-join ',';$checks=($instances|ForEach-Object{$_.Admin+'/readyz'})-join ','
   $front=StartOwned 'frontproxy' @('-addr',"127.0.0.1:$fp",'-admin-addr',"127.0.0.1:$fa",'-targets',$origins,'-readiness',$checks,'-timeout','2s') 'front';Ready "$frontAdmin/readyz" $front;FrontState $true
  }
  Save (Snapshot) "$run/$trial-before.json"
  if($Scenario -eq 'steady'){$target=if($Direct){$instances[0].URL}else{$frontURL};$null=Generator 'loadgen' $target '';Save (Snapshot) "$run/$trial-after.json"}
  elseif($Scenario -eq 'burst'){$target=if($Direct){($instances|ForEach-Object{$_.URL})-join ','}else{$frontURL};$null=Generator 'burst' $target '';Save (Snapshot) "$run/$trial-after.json"}
  else{
   $null=Generator 'burst' $frontURL '-before-stop';Save (Snapshot) "$run/$trial-before-stop.json"
   Stop-Process -Id $instances[0].Process.Id -Force;$instances[0].Process.WaitForExit();$stopTime=[DateTime]::UtcNow
   $null=Generator 'burst' $frontURL '-transition';FrontState $false;Save @{StoppedUTC=$stopTime.ToString('o');ExcludedUTC=[DateTime]::UtcNow.ToString('o')} "$run/$trial-exclusion.json"
   $null=Generator 'burst' $frontURL '-survivor';Save (Snapshot) "$run/$trial-survivor.json"
   $instances[0].Process=StartOwned 'gateway' $instances[0].Arguments $instances[0].Name '-restart';Ready "$($instances[0].Admin)/readyz" $instances[0].Process;FrontState $true
   Save (Snapshot) "$run/$trial-before-rejoin.json";$null=Generator 'burst' $frontURL '-rejoined';Save (Snapshot) "$run/$trial-after.json"
  }
  'completed' | Set-Content -Encoding UTF8 "$run/$trial-status.txt"
 }catch{$_|Out-String|Set-Content -Encoding UTF8 "$run/$trial-failure.txt";throw}
 finally{
  foreach($p in $children){if(!$p.HasExited){Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue};$null=$p.WaitForExit(5000);$p.Dispose()}
  Save $processes "$run/$trial-processes.json";$env:CACHE_ENABLED=$oldCache
 }
}
'completed' | Set-Content -Encoding UTF8 "$run/status.txt"
Write-Output $run
