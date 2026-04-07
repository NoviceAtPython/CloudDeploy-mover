$WingetArgs = "--accept-package-agreements --accept-source-agreements --silent"

winget install -e --id Google.Chrome $WingetArgs
winget install -e --id Tailscale.Tailscale $WingetArgs
winget install -e --id Valve.Steam $WingetArgs
winget install -e --id EpicGames.EpicGamesLauncher $WingetArgs
winget install -e --id PrismLauncher.PrismLauncher $WingetArgs
winget install -e --id ViGEm.ViGEmBus $WingetArgs
winget install -e --id LizardByte.Sunshine $WingetArgs
Invoke-WebRequest -Uri "https://github.com/VirtualDrivers/Virtual-Display-Driver/releases/download/25.7.23/VDD.Control.25.7.23.zip" -OutFile "C:\VDD.zip"
Expand-Archive -Path "C:\VDD.zip" -DestinationPath "C:\VDD"

## We will have to add more code for individual validation of security later but this if fine for now

## //////////////////////////////////////////////////////////////////////////////////////////////////

$TailscaleAuthKey = "tskey-auth-kNatGervUa11CNTRL-jKpWxbykvf7hF6btz5dXg7EuZeMdTToD"
& "C:\Program Files\Tailscale IPN\tailscale.exe" up --authkey=$TailscaleAuthKey --ssh

$autoLogonUser = "Administrator"
$autoLogonPassword = "Awesome11107@13"

$registryPath = "HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon"
Set-ItemProperty -Path $registryPath -Name "AutoAdminLogon" -Value "1"
Set-ItemProperty -Path $registryPath -Name "DefaultUsername" -Value $autoLogonUser
Set-ItemProperty -Path $registryPath -Name "DefaultPassword" -Value $autoLogonPassword

Stop-service -Name SunshineService -ErrorAction SilentlyContinue

New-Item -Path "C:\Program Files\Sunshine\config" -ItemType Directory -Force

$sunshineStateData = @"
{
    "username": "aedyn",
    "salt": "Dt)8KZpjk!ZQEPKM",
    "password": "F59B3E9C51D8BAA6E537E1D35A9AC2F757164B7E675C384E7EE11A93D8163AC7",
    "root": {
        "uniqueid": "3CDD5745-1C66-F323-2307-95EAE24A0A01",
        "named_devices": [
            {
                "name": "amsomnibook5",
                "cert": "-----BEGIN CERTIFICATE-----\nMIICvzCCAaegAwIBAgIBADANBgkqhkiG9w0BAQsFADAjMSEwHwYDVQQDDBhOVklE\nSUEgR2FtZVN0cmVhbSBDbGllbnQwHhcNMjYwMzE4MDM0NDI5WhcNNDYwMzEzMDM0\nNDI5WjAjMSEwHwYDVQQDDBhOVklESUEgR2FtZVN0cmVhbSBDbGllbnQwggEiMA0G\nCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQC5MNHWSQmrJAzGXNBYRLo2zzNCRCYx\na3E\/RSpaKUU6YEueSfMDmKgAhehoQpzookzECopkaMSws5UJ\/G\/xSQ27ajx\/\/1j4\nE08DkdsLfJDUgety9QQhPO27Hl7PChhnTzRS0u4IwOG5j2Q2cBye+Sakd9kdGzL0\n+lH2WFt3KEaXI0gBuuMByESUgOP1dQcX63SZQWGWHv+79DvhxV6\/ClIC2vvAB58U\niM5wfY8UWwhB7SzZqph4zRt392+l7zTp1iq8HcdbXjykoFID49S5BQFbZIrNogbV\n+mdcQY10LSPAQPZ8PhP2YO9NZFkX5Klou2rO+uw6QHPSrTkhHlAING+hAgMBAAEw\nDQYJKoZIhvcNAQELBQADggEBAHWjq4DaehE0FyW9wlq\/0Hw\/vuTf7uolRgzKoUwq\n3s\/eVz1YovIaNG49FDPJdNqrSs5tt1YYySUZe0LVFKRSu9in6BnEXlzbvLd\/ww1n\nZ\/HqoJBMgjnGU5uSCUcoekos4cJqTz1tbPXjcoeLip\/LPk7hZHZNuclR1ldWKDlr\nre5T8FaeUSTX6RwM1dGyWirt64W+\/TinSvNHCWU4ET4V\/DzY+r6Z0BPQB4RDsghd\n1n4UikAqHC7JoIMAoL0fB2c1IbDdhSnjZPrMGrMr5EIicwFg+uCOP\/+h0DhTAi+Y\nHIv92+bPq2n3kSsXnbsCspf4jfzsj5sDnLVTl6hA7cjvMgQ=\n-----END CERTIFICATE-----\n",
                "uuid": "5F980492-EB13-1C03-2091-A4749FDA8951"
            }
        ]
    }
}
"@

