# Windows: start PairToPairSync silently (no console window) at every logon.
#
#   powershell -ExecutionPolicy Bypass -File windows-autostart.ps1 -Exe "C:\Tools\p2psyncd.exe"
#
# p2psyncd.exe is the console-less build of the daemon (see `make windows`). The
# log goes to %LOCALAPPDATA%\p2psync\p2psync.log. Remove it with:
#   Unregister-ScheduledTask -TaskName PairToPairSync -Confirm:$false
param(
    [Parameter(Mandatory)][string]$Exe,
    [string]$DataDir = ""
)
$ErrorActionPreference = "Stop"
if (-not (Test-Path -LiteralPath $Exe)) { throw "Not found: $Exe" }
$Exe = (Resolve-Path -LiteralPath $Exe).Path

$dataPath = if ($DataDir) { $DataDir } else { Join-Path $env:LOCALAPPDATA "p2psync" }
$log = Join-Path $dataPath "p2psync.log"
$args = @()
if ($DataDir) { $args += "--home", "`"$DataDir`"" }
$args += "run", "--log", "`"$log`""

$action   = New-ScheduledTaskAction -Execute $Exe -Argument ($args -join " ")
$trigger  = New-ScheduledTaskTrigger -AtLogOn -User "$env:USERDOMAIN\$env:USERNAME"
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
    -RestartCount 5 -RestartInterval (New-TimeSpan -Minutes 1) `
    -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew
Register-ScheduledTask -TaskName "PairToPairSync" -Action $action -Trigger $trigger -Settings $settings `
    -Description "Folder synchronisation (PairToPairSync)" -Force | Out-Null
Write-Host "Registered. It starts at your next logon; to start it now: Start-ScheduledTask -TaskName PairToPairSync"
Write-Host "Log: $log"
