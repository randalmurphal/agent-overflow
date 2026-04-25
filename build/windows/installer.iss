; Inno Setup script for the Phase D Windows .exe + bundled Linux ELF.
;
; Builds an installer that drops both binaries into Program Files\Agent
; Overflow and creates a Start Menu / Desktop shortcut for the Windows
; .exe. The Linux ELF is shipped beside the .exe so the launcher's
; payload-install path (cmd/agent-overflow-windows/main.go) can find
; it on first launch.
;
; This script assumes the Taskfile target `common:windows:build` (see
; build/Taskfile.yml) has already produced:
;   - bin/agent-overflow.exe
;   - cmd/agent-overflow-windows/payload/agent-overflow-linux
;
; Build the installer with:
;   iscc build/windows/installer.iss
;
; Inno Setup is Windows-only — this file is provided for the eventual
; Windows-host build pipeline. macOS / Linux developers can hand-zip
; the same files using the `package:windows:zip` Taskfile target.

#define AppName "Agent Overflow"
#define AppVersion "0.0.1"
#define AppPublisher "Agent Overflow"
#define AppExeName "agent-overflow.exe"
#define LinuxPayload "agent-overflow-linux"

[Setup]
AppId={{F4A6E1F2-A2D2-4B6E-8B8C-3F2A6E1F2D2D}
AppName={#AppName}
AppVersion={#AppVersion}
AppPublisher={#AppPublisher}
DefaultDirName={autopf}\{#AppName}
DefaultGroupName={#AppName}
DisableProgramGroupPage=yes
OutputBaseFilename=agent-overflow-setup
Compression=lzma
SolidCompression=yes
WizardStyle=modern
PrivilegesRequired=lowest
ArchitecturesInstallIn64BitMode=x64compatible

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"

[Files]
; The Windows launcher binary, produced by the Taskfile target.
Source: "..\..\bin\agent-overflow.exe"; DestDir: "{app}"; Flags: ignoreversion
; The cross-compiled Linux ELF, shipped beside the .exe so the launcher
; can install it into the chosen WSL distro on first launch.
Source: "..\..\cmd\agent-overflow-windows\payload\{#LinuxPayload}"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{group}\{#AppName}"; Filename: "{app}\{#AppExeName}"
Name: "{autodesktop}\{#AppName}"; Filename: "{app}\{#AppExeName}"; Tasks: desktopicon

[Run]
Filename: "{app}\{#AppExeName}"; Description: "Launch {#AppName}"; Flags: nowait postinstall skipifsilent
