# Copyright (C) 2026 the SeaTime authors. Licensed under the GNU AGPL v3.
# SPDX-License-Identifier: AGPL-3.0-only
# SeaTime installer for Windows (PowerShell 5.1 or newer).
#   irm @@SERVER@@/install.ps1 | iex
# Installs seatime.exe into %LOCALAPPDATA%\seatime\bin (no admin), checks its SHA-256
# against the server's SHA256SUMS, adds that folder to your user PATH, then runs
# `seatime setup --yes`. Nothing here calls `exit`, so it is safe under iex.

function Install-Seatime {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'   # Invoke-WebRequest is very slow with progress on 5.1
    $server = '@@SERVER@@'
    try {
        # Old Windows PowerShell defaults to TLS 1.0/1.1; most servers require 1.2.
        [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

        # 32-bit PowerShell on 64-bit Windows reports x86 here; ARCHITEW6432 has the real CPU.
        $cpu = $env:PROCESSOR_ARCHITEW6432
        if (-not $cpu) { $cpu = $env:PROCESSOR_ARCHITECTURE }
        switch ($cpu.ToUpper()) {
            'AMD64' { $arch = 'amd64' }
            'ARM64' { $arch = 'amd64' }   # Windows on ARM runs x64 binaries under emulation
            'X86'   { $arch = '386' }
            default { throw "unsupported CPU '$cpu'" }
        }
        $name = "seatime-windows-$arch.exe"

        $tmp = Join-Path ([IO.Path]::GetTempPath()) ("seatime-install-" + [Guid]::NewGuid().ToString('N'))
        New-Item -ItemType Directory -Path $tmp | Out-Null
        try {
            Write-Host "Downloading $name from $server ..."
            Invoke-WebRequest -UseBasicParsing -Uri "$server/dl/SHA256SUMS" -OutFile (Join-Path $tmp 'SHA256SUMS')
            Invoke-WebRequest -UseBasicParsing -Uri "$server/dl/$name" -OutFile (Join-Path $tmp $name)

            $want = $null
            foreach ($line in (Get-Content -Path (Join-Path $tmp 'SHA256SUMS'))) {
                $parts = ($line.Trim()) -split '\s+'
                if ($parts.Count -ge 2 -and $parts[1].TrimStart('*') -eq $name) { $want = $parts[0].ToLower(); break }
            }
            if (-not $want) { throw "$name is not listed in SHA256SUMS" }
            $got = (Get-FileHash -Algorithm SHA256 -Path (Join-Path $tmp $name)).Hash.ToLower()
            if ($got -ne $want) { throw "checksum mismatch for $name (expected $want, got $got); refusing to install" }
            Write-Host 'Checksum OK.'

            $dir = Join-Path $env:LOCALAPPDATA 'seatime\bin'
            New-Item -ItemType Directory -Force -Path $dir | Out-Null
            $exe = Join-Path $dir 'seatime.exe'
            # A running seatime.exe cannot be overwritten on Windows; setup restarts the daemon afterwards.
            Get-Process -Name seatime -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
            Start-Sleep -Milliseconds 300
            Copy-Item -Force -Path (Join-Path $tmp $name) -Destination $exe
            Write-Host "Installed $exe"
        } finally {
            Remove-Item -Recurse -Force -Path $tmp -ErrorAction SilentlyContinue
        }

        # Add the install folder to the *user* PATH (no admin needed).
        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        if (-not $userPath) { $userPath = '' }
        $entries = $userPath -split ';' | Where-Object { $_ -ne '' }
        if ($entries -notcontains $dir) {
            $newPath = (($entries + $dir) -join ';')
            [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
            Write-Host "Added $dir to your PATH (open a new terminal to use 'seatime')."
        }
        if (($env:Path -split ';') -notcontains $dir) { $env:Path = "$env:Path;$dir" }

        if ($env:SEATIME_NO_SETUP) {
            Write-Host "Skipping setup (SEATIME_NO_SETUP). Run: seatime setup --yes --server $server"
            return
        }
        $ErrorActionPreference = 'Continue'   # native stderr must not turn into terminating errors on 5.1
        & $exe setup --yes --server $server
        if ($LASTEXITCODE -ne 0) { Write-Host "seatime setup exited with code $LASTEXITCODE" -ForegroundColor Red }
    } catch {
        Write-Host "seatime install: $($_.Exception.Message)" -ForegroundColor Red
    }
}

Install-Seatime
