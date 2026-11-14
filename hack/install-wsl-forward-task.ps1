<#
Register one current-user logon task for the optional Windows TCP supervisor.
This does not install Python, alter WSL settings, or store a password. The
supervisor package/config/state must already be prepared in permanent paths.
Use Render first: it shows the exact executable, arguments, account and limits.
An existing task is never overwritten. Disable preserves the task and files.
#>
param(
    [ValidateSet('Render','Install','Start','Status','Disable')][string]$Action = 'Render',
    [Parameter(Mandatory=$true)][string]$PackageDir,
    [Parameter(Mandatory=$true)][string]$Config,
    [Parameter(Mandatory=$true)][string]$StateDir,
    [Parameter(Mandatory=$true)][string]$Pythonw,
    [ValidatePattern('^Shearwater-[A-Za-z0-9-]+$')][string]$TaskName = 'Shearwater-Windows-Forward'
)
$ErrorActionPreference = 'Stop'
foreach ($item in @($PackageDir,$Config,$StateDir,$Pythonw)) {
    if (-not [IO.Path]::IsPathRooted($item) -or $item -match '["\r\n]') {
        throw 'All paths must be absolute and contain no quotes or line breaks.'
    }
}
$PackageDir = [IO.Path]::GetFullPath($PackageDir).TrimEnd('\')
$StateDir = [IO.Path]::GetFullPath($StateDir).TrimEnd('\')
$program = Join-Path $PackageDir 'wsl-forward-autostart.py'
foreach ($file in @($program, (Join-Path $PackageDir 'wsl-forward.py'), $Config, $Pythonw)) {
    if (-not (Test-Path -LiteralPath $file -PathType Leaf)) { throw "Missing file: $file" }
}
if (-not (Test-Path -LiteralPath $StateDir -PathType Container)) { throw 'State directory is missing.' }
if ([IO.Path]::GetFileName($Pythonw) -ine 'pythonw.exe') { throw 'Use pythonw.exe to avoid opening a console at login.' }

$account = [Security.Principal.WindowsIdentity]::GetCurrent().Name
# Task Scheduler directly invokes this program; no shell interprets these
# arguments. Quotes preserve path spaces. Valid Windows paths cannot contain ".
$arguments = '-I "{0}" run --config "{1}" --state-dir "{2}"' -f $program,$Config,$StateDir
$description = 'Shearwater Windows TCP forwarding supervisor (current-user, schema 1)'
$taskAction = New-ScheduledTaskAction -Execute $Pythonw -Argument $arguments -WorkingDirectory $PackageDir
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $account
$principal = New-ScheduledTaskPrincipal -UserId $account -LogonType Interactive -RunLevel Limited
# A long-lived supervisor must not be killed after the usual three-day task
# limit. Battery use also must not silently stop the local app entrypoint.
$settings = New-ScheduledTaskSettingsSet -MultipleInstances IgnoreNew -StartWhenAvailable `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)
$definition = New-ScheduledTask -Action $taskAction -Trigger $trigger -Principal $principal -Settings $settings -Description $description
$existing = Get-ScheduledTask -TaskName $TaskName -TaskPath '\' -ErrorAction SilentlyContinue

if ($Action -eq 'Render') {
    [pscustomobject]@{TaskName=$TaskName; User=$account; LogonType='Interactive'; RunLevel='Limited';
        Executable=$Pythonw; Arguments=$arguments; Trigger='At user logon';
        ExecutionTimeLimit='unlimited'; MultipleInstances='IgnoreNew'; Existing=($null -ne $existing)} | ConvertTo-Json
    exit 0
}
if ($Action -eq 'Install') {
    if ($null -ne $existing) { throw 'Task already exists; inspect it before deciding how to update it.' }
    Register-ScheduledTask -TaskName $TaskName -TaskPath '\' -InputObject $definition | Out-Null
    Get-ScheduledTask -TaskName $TaskName -TaskPath '\' | Select-Object TaskName,State
    exit 0
}
if ($null -eq $existing) { throw 'The task has not been installed.' }
# Even control actions must not operate on an unrelated task using this name.
if ($existing.Description -ne $description -or $existing.Actions.Execute -ne $Pythonw -or $existing.Actions.Arguments -ne $arguments) {
    throw 'Existing task ownership/configuration does not match this package.'
}
switch ($Action) {
    'Start' {
        # IgnoreNew is the race-safe backstop. Avoid knowingly submitting a
        # duplicate here: Windows can record a refused launch in LastTaskResult
        # while the original healthy instance keeps running, confusing users.
        if ($existing.State -ne 'Running') { Start-ScheduledTask -TaskName $TaskName -TaskPath '\' }
        Get-ScheduledTask -TaskName $TaskName -TaskPath '\' | Select-Object TaskName,State
    }
    'Disable' { Disable-ScheduledTask -TaskName $TaskName -TaskPath '\' | Out-Null }
    'Status' {
        $info = Get-ScheduledTaskInfo -TaskName $TaskName -TaskPath '\'
        [pscustomobject]@{TaskName=$TaskName; State=[string]$existing.State;
            LastRunTime=$info.LastRunTime; LastTaskResult=$info.LastTaskResult; NextRunTime=$info.NextRunTime}
    }
}
