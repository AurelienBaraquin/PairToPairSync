# Windows: run once in PowerShell to start p2psync silently at every logon.
#   powershell -ExecutionPolicy Bypass -File windows-autostart.ps1 -Exe "C:\Tools\p2psync.exe"
param([Parameter(Mandatory)][string]$Exe)
$action  = New-ScheduledTaskAction -Execute $Exe -Argument "run"
$trigger = New-ScheduledTaskTrigger -AtLogOn
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -RestartCount 5 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit ([TimeSpan]::Zero)
Register-ScheduledTask -TaskName "PairToPairSync" -Action $action -Trigger $trigger -Settings $settings -Force
