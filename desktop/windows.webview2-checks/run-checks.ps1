# Runs Cage.WebView2Checks in the signed-in user's interactive session (a
# WebView2 cannot start in an SSH session: 0x80070578) through a scheduled
# task, waits for its results file and removes the task. Only the process the
# task started is ever stopped.
param(
    [Parameter(Mandatory)][string]$Dotnet,
    [Parameter(Mandatory)][string]$Checks,
    [Parameter(Mandatory)][string]$Results,
    [int]$TimeoutSeconds = 300
)
$ErrorActionPreference = 'Stop'
$name = 'cage-webview2-checks-' + [guid]::NewGuid().ToString('N').Substring(0, 8)
$data = Join-Path (Split-Path $Results) ('wv2-data-' + $name)
$pidFile = $Results + '.pid'
Remove-Item -LiteralPath $Results, $pidFile -ErrorAction SilentlyContinue
# The task records the process id it started, so this script stops that one
# process and nothing else.
$command = "`$p = Start-Process -FilePath '$Dotnet' -ArgumentList @('$Checks', '$Results', '$data') -PassThru; Set-Content -LiteralPath '$pidFile' -Value `$p.Id; `$p.WaitForExit()"
$action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument ('-NoProfile -NonInteractive -WindowStyle Hidden -Command "' + $command + '"')
$principal = New-ScheduledTaskPrincipal -UserId ([System.Security.Principal.WindowsIdentity]::GetCurrent().Name) -LogonType Interactive
Register-ScheduledTask -TaskName $name -Action $action -Principal $principal | Out-Null
try {
    Start-ScheduledTask -TaskName $name
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline -and -not (Test-Path -LiteralPath $Results)) { Start-Sleep -Milliseconds 500 }
    Start-Sleep -Seconds 2
    if (Test-Path -LiteralPath $pidFile) {
        $started = [int](Get-Content -LiteralPath $pidFile)
        $process = Get-Process -Id $started -ErrorAction SilentlyContinue
        if ($process -and -not $process.HasExited) { Stop-Process -Id $started -Force; 'stopped ' + $started }
    }
} finally {
    Unregister-ScheduledTask -TaskName $name -Confirm:$false
}
if (Test-Path -LiteralPath $Results) { 'results ' + $Results } else { 'no results' }
