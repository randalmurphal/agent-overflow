param(
  [Parameter(Mandatory = $true)]
  [string] $PidTypesJson,
  [Parameter(Mandatory = $true)]
  [string] $ManifestPath,
  [Parameter(Mandatory = $true)]
  [int] $ExpectedBrowserPid,
  [Parameter(Mandatory = $true)]
  [string] $Out,
  [int] $Seconds = 600,
  [int] $EveryMs = 1000,
  [int] $KillAtPrivateWorkingSetMB = 0,
  [string] $RequiredBrowserArg = '',
  [switch] $Append
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

if ($Seconds -lt 1) { throw "Seconds must be at least 1, got $Seconds" }
if ($EveryMs -lt 1000 -or $EveryMs % 1000 -ne 0) {
  throw "EveryMs must be a whole number of seconds and at least 1000, got $EveryMs"
}

if (-not [System.IO.File]::Exists($ManifestPath)) { throw "Instance manifest does not exist: $ManifestPath" }
$manifest = Get-Content -Raw -LiteralPath $ManifestPath | ConvertFrom-Json
if ($null -eq $manifest.instanceId -or [string]::IsNullOrWhiteSpace([string] $manifest.instanceId)) {
  throw 'Instance manifest has no instanceId'
}
$manifestTarget = if ($null -ne $manifest.target) { $manifest.target } else { $manifest.page }
$manifestTargetId = if ($null -ne $manifest.targetId) { $manifest.targetId } else { $manifestTarget.id }
$manifestMarker = if ($null -ne $manifest.pageMarker) { $manifest.pageMarker } else { $manifestTarget.pageMarker }
if ([string]::IsNullOrWhiteSpace([string] $manifestTargetId)) { throw 'Instance manifest has no targetId' }
if ([string]::IsNullOrWhiteSpace([string] $manifestMarker)) { throw 'Instance manifest has no page marker' }
if ([string]::IsNullOrWhiteSpace([string] $manifest.origin) -and [string]::IsNullOrWhiteSpace([string] $manifestTarget.origin)) {
  throw 'Instance manifest has no exact page origin'
}
if ($null -ne $manifest.browserPid -and [int] $manifest.browserPid -ne $ExpectedBrowserPid) {
  throw "Instance manifest browser PID $($manifest.browserPid) does not match $ExpectedBrowserPid"
}

$entries = @()
foreach ($entry in (ConvertFrom-Json -InputObject $PidTypesJson)) {
  $entries += $entry
}
if ($entries.Count -eq 0) { throw 'CDP returned no WebView2 processes' }

$typesByProcessId = @{}
foreach ($entry in $entries) {
  $processId = [int] $entry.id
  $typesByProcessId[$processId] = [string] $entry.type
}

$browserEntries = @($entries | Where-Object { $_.type -eq 'browser' })
if ($browserEntries.Count -ne 1) {
  throw "Expected exactly one browser process from CDP, got $($browserEntries.Count)"
}
$browserProcessId = [int] $browserEntries[0].id
if ($browserProcessId -ne $ExpectedBrowserPid) {
  throw "CDP browser PID $browserProcessId does not match the supervisor-selected browser PID $ExpectedBrowserPid"
}
$browser = Get-CimInstance Win32_Process -Filter "ProcessId = $browserProcessId"
if ($null -eq $browser -or $browser.Name -notlike 'msedgewebview2*') {
  throw "CDP browser PID $browserProcessId is not a live msedgewebview2 process"
}
$browserCreationDate = [string] $browser.CreationDate
if (-not [string]::IsNullOrWhiteSpace($RequiredBrowserArg)) {
  $argumentPattern = '(?:^|\s)' + [regex]::Escape($RequiredBrowserArg) + '(?:\s|$)'
  if ([string]::IsNullOrEmpty($browser.CommandLine) -or $browser.CommandLine -notmatch $argumentPattern) {
    throw "CDP browser PID $browserProcessId is missing required argument $RequiredBrowserArg"
  }
}

function Get-UserDataDir([string] $commandLine) {
  if ([string]::IsNullOrEmpty($commandLine)) { return $null }
  $match = [regex]::Match(
    $commandLine,
    '(?:^|\s)--user-data-dir=(?:"([^"]+)"|(\S+))',
    [System.Text.RegularExpressions.RegexOptions]::IgnoreCase
  )
  if (-not $match.Success) { return $null }
  if ($match.Groups[1].Success) { return $match.Groups[1].Value }
  return $match.Groups[2].Value
}

$userDataDir = Get-UserDataDir $browser.CommandLine
if ([string]::IsNullOrEmpty($userDataDir)) {
  throw "WebView2 browser PID $browserProcessId has no --user-data-dir"
}

$launcherProcessId = 0
$launcherCreationDate = ''
if ($KillAtPrivateWorkingSetMB -gt 0) {
  if ($userDataDir -notmatch '(?i)\\agent-overflow\\webview2-perf\\EBWebView\\?$') {
    throw "Safety termination requires the isolated perf WebView2 profile, got $userDataDir"
  }
  $launcherProcessId = [int] $browser.ParentProcessId
  $launcher = Get-CimInstance Win32_Process -Filter "ProcessId = $launcherProcessId"
  if (
    $null -eq $launcher -or
    $launcher.Name -notlike 'agent-overflow*.exe' -or
    $launcher.CommandLine -notmatch '(?i)(?:^|\s)--profile(?:=|\s+)perf(?:\s|$)'
  ) {
    throw "CDP browser PID $browserProcessId does not have a verified perf-profile launcher parent"
  }
  $launcherCreationDate = [string] $launcher.CreationDate
}

function Get-ProfileProcesses {
  return @(
    Get-CimInstance Win32_Process -Filter "Name like 'msedgewebview2%'" |
    Where-Object {
      [string]::Equals(
        (Get-UserDataDir $_.CommandLine),
        $userDataDir,
        [System.StringComparison]::OrdinalIgnoreCase
      )
    }
  )
}

function Get-ProcessType([object] $process) {
  $processId = [int] $process.ProcessId
  if ($typesByProcessId.ContainsKey($processId)) {
    return [string] $typesByProcessId[$processId]
  }
  if ($process.CommandLine -match '(?:^|\s)--type=crashpad-handler(?:\s|$)') {
    return 'crashpad-handler'
  }
  $typeMatch = [regex]::Match(
    [string] $process.CommandLine,
    '(?:^|\s)--type=([^\s]+)',
    [System.Text.RegularExpressions.RegexOptions]::IgnoreCase
  )
  if ($typeMatch.Success) {
    switch ($typeMatch.Groups[1].Value.ToLowerInvariant()) {
      'renderer' { return 'renderer' }
      'gpu-process' { return 'GPU' }
      default { return 'utility' }
    }
  }
  # The profile's one process without --type is its browser process. This also
  # covers a browser replacement long enough for the sampler to report it
  # before the CDP endpoint itself disconnects.
  return 'browser'
}

# CDP proves which isolated profile this endpoint owns. The live census below
# then follows every process carrying that exact user-data directory. A frozen
# startup PID list silently missed renderer/utility replacements and orphaned
# children, which made the safety ceiling smaller than the Task Manager group
# it was meant to guard.
$webviews = @(Get-ProfileProcesses)
foreach ($process in $webviews) {
  $processUserDataDir = Get-UserDataDir $process.CommandLine
  if (-not [string]::Equals(
      $processUserDataDir,
      $userDataDir,
      [System.StringComparison]::OrdinalIgnoreCase
    )) {
    continue
  }

  $processId = [int] $process.ProcessId
  if (-not $typesByProcessId.ContainsKey($processId)) {
    $typesByProcessId[$processId] = Get-ProcessType $process
  }
}

foreach ($processId in @($typesByProcessId.Keys)) {
  $process = $webviews | Where-Object { [int] $_.ProcessId -eq $processId } | Select-Object -First 1
  if ($null -eq $process) { throw "CDP process PID $processId is no longer live" }
  $processUserDataDir = Get-UserDataDir $process.CommandLine
  if (-not [string]::Equals(
      $processUserDataDir,
      $userDataDir,
      [System.StringComparison]::OrdinalIgnoreCase
    )) {
    throw "CDP process PID $processId does not belong to $userDataDir"
  }
}

function Get-Group([string] $type) {
  switch -Regex ($type) {
    '^browser$' { return 'browser' }
    '^(GPU|gpu-process)$' { return 'gpu' }
    '^renderer$' { return 'renderer' }
    '^crashpad-handler$' { return 'crashpad' }
    default { return 'utility' }
  }
}

$columns = @(
  'utc', 'elapsedMs', 'processCount', 'censusMissingCount',
  'groupPrivateBytes', 'groupWorkingSetBytes', 'groupWorkingSetPrivateBytes',
  'browserPrivateBytes', 'browserWorkingSetBytes', 'browserWorkingSetPrivateBytes',
  'gpuPrivateBytes', 'gpuWorkingSetBytes', 'gpuWorkingSetPrivateBytes',
  'rendererPrivateBytes', 'rendererWorkingSetBytes', 'rendererWorkingSetPrivateBytes',
  'utilityPrivateBytes', 'utilityWorkingSetBytes', 'utilityWorkingSetPrivateBytes',
  'crashpadPrivateBytes', 'crashpadWorkingSetBytes', 'crashpadWorkingSetPrivateBytes'
)

$parent = Split-Path -Parent $Out
if (-not [string]::IsNullOrEmpty($parent)) {
  [System.IO.Directory]::CreateDirectory($parent) | Out-Null
}
$utf8NoBom = New-Object System.Text.UTF8Encoding($false)
$appendExisting = $Append -and [System.IO.File]::Exists($Out) -and (Get-Item $Out).Length -gt 0
if ($appendExisting) {
  $reader = [System.IO.File]::Open($Out, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read)
  try {
    [void] $reader.Seek(-1, [System.IO.SeekOrigin]::End)
    if ($reader.ReadByte() -ne 10) {
      throw "Cannot append to $Out because its final row is incomplete"
    }
  } finally {
    $reader.Dispose()
  }
}
$writer = New-Object System.IO.StreamWriter($Out, $appendExisting, $utf8NoBom)
if (-not $appendExisting) { $writer.WriteLine(($columns -join ',')) }
$writer.Flush()

$clock = [System.Diagnostics.Stopwatch]::StartNew()
$nextProgressMs = 30000L
$sampleIntervalSeconds = [int] ($EveryMs / 1000)
$sampleCount = [int] ([Math]::Ceiling($Seconds / $sampleIntervalSeconds) + 1)

Write-Output "webview-memory: user-data-dir=$userDataDir"
if (-not [string]::IsNullOrWhiteSpace($RequiredBrowserArg)) {
  Write-Output "webview-memory: verified-browser-arg=$RequiredBrowserArg"
}
Write-Output (
  'webview-memory: processes=' +
  ((@($typesByProcessId.Keys | Sort-Object) | ForEach-Object { "$_/$($typesByProcessId[$_])" }) -join ',')
)

try {
  $counterPaths = @(
    '\Process(msedgewebview2*)\ID Process',
    '\Process(msedgewebview2*)\Private Bytes',
    '\Process(msedgewebview2*)\Working Set',
    '\Process(msedgewebview2*)\Working Set - Private'
  )
  Get-Counter -Counter $counterPaths -SampleInterval $sampleIntervalSeconds -MaxSamples $sampleCount |
  ForEach-Object {
    $elapsedMs = $clock.ElapsedMilliseconds
    $byInstance = @{}
    foreach ($sample in $_.CounterSamples) {
      # PerformanceCounterSample.InstanceName drops the numeric suffix and
      # reports every msedgewebview2 process as the same string. The path keeps
      # the unique msedgewebview2#N instance that joins the three counters.
      $pathMatch = [regex]::Match(
        [string] $sample.Path,
        '\\process\(([^)]+)\)\\',
        [System.Text.RegularExpressions.RegexOptions]::IgnoreCase
      )
      if (-not $pathMatch.Success) {
        throw "Unexpected process counter path: $($sample.Path)"
      }
      $instance = $pathMatch.Groups[1].Value
      if (-not $byInstance.ContainsKey($instance)) {
        $byInstance[$instance] = @{}
      }
      if ($sample.Path -like '*\ID Process') {
        $byInstance[$instance].ProcessId = [int] $sample.CookedValue
      } elseif ($sample.Path -like '*\Private Bytes') {
        $byInstance[$instance].PrivateBytes = [long] $sample.CookedValue
      } elseif ($sample.Path -like '*\Working Set') {
        $byInstance[$instance].WorkingSet = [long] $sample.CookedValue
      } elseif ($sample.Path -like '*\Working Set - Private') {
        $byInstance[$instance].WorkingSetPrivate = [long] $sample.CookedValue
      }
    }

    $rowsByProcessId = @{}
    foreach ($row in $byInstance.Values) {
      if ($row.ContainsKey('ProcessId')) { $rowsByProcessId[$row.ProcessId] = $row }
    }

    $currentTypesByProcessId = @{}
    $currentProcessesById = @{}
    foreach ($process in @(Get-ProfileProcesses)) {
      $processId = [int] $process.ProcessId
      $type = Get-ProcessType $process
      $currentTypesByProcessId[$processId] = $type
      $currentProcessesById[$processId] = $process
      $typesByProcessId[$processId] = $type
    }
    if (-not $currentTypesByProcessId.ContainsKey($browserProcessId)) {
      throw "CDP browser PID $browserProcessId disappeared during sampling"
    }
    $processIds = @($currentTypesByProcessId.Keys | Sort-Object)
    $missing = @($processIds | Where-Object { -not $rowsByProcessId.ContainsKey($_) })
    # Get-Counter's sample and the CIM census are not atomic. A child born
    # between them is absent from this one snapshot. Some Chromium service
    # processes also do not publish a Process-counter instance at all. Record
    # either gap instead of pretending the sample is complete.
    $sampledProcessIds = @($processIds | Where-Object { $rowsByProcessId.ContainsKey($_) })

    $totals = @{}
    foreach ($group in @('browser', 'gpu', 'renderer', 'utility', 'crashpad')) {
      $totals[$group] = @{ Private = 0L; WorkingSet = 0L; WorkingSetPrivate = 0L }
    }
    foreach ($processId in $sampledProcessIds) {
      $group = Get-Group $currentTypesByProcessId[$processId]
      $row = $rowsByProcessId[$processId]
      $totals[$group].Private += [long] $row.PrivateBytes
      $totals[$group].WorkingSet += [long] $row.WorkingSet
      $totals[$group].WorkingSetPrivate += [long] $row.WorkingSetPrivate
    }

    $groupPrivate = 0L
    $groupWorkingSet = 0L
    $groupWorkingSetPrivate = 0L
    foreach ($group in $totals.Keys) {
      $groupPrivate += $totals[$group].Private
      $groupWorkingSet += $totals[$group].WorkingSet
      $groupWorkingSetPrivate += $totals[$group].WorkingSetPrivate
    }

    # A process can appear in the CIM census after Get-Counter took its
    # snapshot. Keep the CSV explicitly incomplete, but do not let that race
    # weaken the opt-in OOM guard. Total working set is an upper bound for the
    # missing process's private working set, so adding it can terminate a perf
    # run early but can never hide memory from the safety ceiling.
    $safetyWorkingSetPrivate = $groupWorkingSetPrivate
    foreach ($processId in $missing) {
      $process = $currentProcessesById[$processId]
      if ($null -ne $process) {
        $safetyWorkingSetPrivate += [long] $process.WorkingSetSize
      }
    }

    $values = @(
      [DateTime]::UtcNow.ToString('o'), $elapsedMs, $processIds.Count, $missing.Count,
      $groupPrivate, $groupWorkingSet, $groupWorkingSetPrivate,
      $totals.browser.Private, $totals.browser.WorkingSet, $totals.browser.WorkingSetPrivate,
      $totals.gpu.Private, $totals.gpu.WorkingSet, $totals.gpu.WorkingSetPrivate,
      $totals.renderer.Private, $totals.renderer.WorkingSet, $totals.renderer.WorkingSetPrivate,
      $totals.utility.Private, $totals.utility.WorkingSet, $totals.utility.WorkingSetPrivate,
      $totals.crashpad.Private, $totals.crashpad.WorkingSet, $totals.crashpad.WorkingSetPrivate
    )
    $writer.WriteLine(($values -join ','))
    $writer.Flush()

    if (
      $KillAtPrivateWorkingSetMB -gt 0 -and
      $safetyWorkingSetPrivate -ge ($KillAtPrivateWorkingSetMB * 1MB)
    ) {
      Write-Warning (
        ('webview-memory safety ceiling crossed: {0:N1}MB >= {1}MB; ' +
        'terminating verified perf launcher PID {2} and browser PID {3}') -f
        ($safetyWorkingSetPrivate / 1MB), $KillAtPrivateWorkingSetMB,
        $launcherProcessId, $browserProcessId
      )
      # Revalidate process identity immediately before each forced stop. A PID
      # captured at sampler start can be reused during a long run. Creation
      # time plus the exact perf profile prevents the safety mechanism from
      # ever terminating an unrelated process that inherited the number.
      $liveLauncher = Get-CimInstance Win32_Process -Filter "ProcessId = $launcherProcessId"
      if (
        $null -ne $liveLauncher -and
        [string] $liveLauncher.CreationDate -eq $launcherCreationDate -and
        $liveLauncher.Name -like 'agent-overflow*.exe' -and
        $liveLauncher.CommandLine -match '(?i)(?:^|\s)--profile(?:=|\s+)perf(?:\s|$)'
      ) {
        Stop-Process -Id $launcherProcessId -Force -ErrorAction Stop
      }
      $liveBrowser = Get-CimInstance Win32_Process -Filter "ProcessId = $browserProcessId"
      if (
        $null -ne $liveBrowser -and
        [string] $liveBrowser.CreationDate -eq $browserCreationDate -and
        $liveBrowser.Name -like 'msedgewebview2*' -and
        [string]::Equals(
          (Get-UserDataDir $liveBrowser.CommandLine),
          $userDataDir,
          [System.StringComparison]::OrdinalIgnoreCase
        )
      ) {
        Stop-Process -Id $browserProcessId -Force -ErrorAction Stop
      }
      throw "WebView2 private-working-set safety ceiling crossed"
    }

    if ($elapsedMs -ge $nextProgressMs) {
      Write-Output (
        'webview-memory: t={0:N1}s private={1:N1}MB working-set={2:N1}MB private-working-set={3:N1}MB processes={4}' -f
        ($elapsedMs / 1000), ($groupPrivate / 1MB), ($groupWorkingSet / 1MB),
        ($groupWorkingSetPrivate / 1MB), $processIds.Count
      )
      $nextProgressMs += 30000L
    }

  }
} finally {
  $writer.Dispose()
}

Write-Output "webview-memory: csv=$Out"
