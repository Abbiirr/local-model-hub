# Expose the LocalHub dashboard to your private tailnet (NOT the public internet).
# Prereq: Tailscale running and logged in. If `tailscale status` says "NoState" or
# "starting", restart the Tailscale service from an ELEVATED shell first:
#   Restart-Service Tailscale -Force
# or quit + relaunch the Tailscale tray app.
param(
    [int]$Port = 39876
)

$status = tailscale status 2>&1 | Out-String
if ($status -match "NoState|starting|Logged out") {
    Write-Error "Tailscale backend is not ready:`n$status`nFix Tailscale first (see comments above), then re-run."
    exit 1
}

# Serve proxies https://<machine>.<tailnet>.ts.net -> 127.0.0.1:$Port, tailnet-only.
tailscale serve --bg $Port
tailscale serve status
Write-Host ""
Write-Host "Open the printed https://....ts.net URL on your phone (same tailnet)."
Write-Host "To stop sharing: tailscale serve --https=443 off"
