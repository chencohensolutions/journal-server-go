; Inno Setup script for the journal server on Windows (docs/install.md). Built by scripts/release.sh, or on
; Windows with: ISCC.exe /DAppVersion=<version> installer\journal-server.iss (after building dist\*.exe).
;
; The installer copies the program, then runs `journal-server setup`, which writes config.json and the
; certificates and downloads ONNX Runtime, the models, libvips and ffmpeg (so the installer stays small), and
; `journal-server service install`, which sets up the service that starts the server at boot and the firewall rule.
; Uninstalling removes the programs and what was downloaded, never config.json, the certificates, devices.json or
; any data folder.

#ifndef AppVersion
  #define AppVersion "0.0.0-dev"
#endif
#ifndef Dist
  #define Dist "..\dist"
#endif

[Setup]
AppId={{6F0C7E62-3B8A-4E0B-9C1E-5A1D2F9B7C41}
AppName=Journal server
AppVersion={#AppVersion}
AppPublisher=Journal
; Not Program Files: the server writes admin.token, devices.json and server.log beside itself.
DefaultDirName={sd}\JournalServer
DisableProgramGroupPage=yes
PrivilegesRequired=admin
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
ChangesEnvironment=yes
OutputDir={#Dist}
OutputBaseFilename=journal-server-setup-{#AppVersion}
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
UninstallDisplayName=Journal server

[Files]
Source: "{#Dist}\journal-server.exe"; DestDir: "{app}"; Flags: ignoreversion

[InstallDelete]
; Before 1.2 the import and jxl were programs of their own; they are journal-server import and jxl now.
Type: files; Name: "{app}\journal-import.exe"
Type: files; Name: "{app}\journal-jxl.exe"

[Icons]
Name: "{autoprograms}\Journal server folder"; Filename: "{cmd}"; Parameters: "/k cd /d ""{app}"" && journal-server"; WorkingDir: "{app}"; Comment: "A console in the server's folder, for journal-server and its commands"
Name: "{autoprograms}\Journal server log"; Filename: "{app}\server.log"

[UninstallRun]
Filename: "{app}\journal-server.exe"; Parameters: "service uninstall"; Flags: runhidden waituntilterminated; RunOnceId: "RemoveService"

[UninstallDelete]
; Only what setup downloaded; config.json, certs\, devices.json and the data folders stay.
Type: filesandordirs; Name: "{app}\onnxruntime"
Type: filesandordirs; Name: "{app}\models"
Type: filesandordirs; Name: "{app}\vips"
Type: filesandordirs; Name: "{app}\ffmpeg"
Type: files; Name: "{app}\admin.token"
; What updates leave beside the program (docs/install.md, "Updates").
Type: files; Name: "{app}\journal-server.exe.old"
Type: files; Name: "{app}\journal-server.exe.new"
Type: files; Name: "{app}\journal-server.exe.rollback"

[Code]
var
  ModePage, CAPage, ModelPage: TInputOptionWizardPage;
  MovePage, DataPage, CAFolderPage: TInputDirWizardPage;
  UserPage: TInputQueryWizardPage;

function IsUpgrade(): Boolean;
begin
  Result := FileExists(ExpandConstant('{app}\config.json'));
end;

function IsMove(): Boolean;
begin
  Result := ModePage.SelectedValueIndex = 0;
end;

procedure InitializeWizard();
begin
  ModePage := CreateInputOptionPage(wpSelectDir, 'Server', 'Is this a new server, or does it replace one?',
    'Moving copies the old server''s config.json, certificates and enrolled devices; the old folder is only read. ' +
    'Move the data folders yourself, to the same place or anywhere you then set in config.json.', True, False);
  ModePage.Add('Move my existing server to this PC');
  ModePage.Add('Set up a new server');
  ModePage.SelectedValueIndex := 0;

  MovePage := CreateInputDirPage(ModePage.ID, 'Move a server', 'Where is the old server''s folder?',
    'The folder with its config.json and certs folder (for example a copy on a USB disk, or a network share).', False, '');
  MovePage.Add('');

  UserPage := CreateInputQueryPage(MovePage.ID, 'First user', 'Who is the first user?',
    'The name is the user''s identity in their devices'' certificates. Letters and digits only.');
  UserPage.Add('User name:', False);

  DataPage := CreateInputDirPage(UserPage.ID, 'First user', 'Where should this user''s photos be kept?',
    'An existing journal folder is used as it is; setup writes nothing into it.', False, '');
  DataPage.Add('');
  DataPage.Values[0] := ExpandConstant('{sd}\Journal');

  CAPage := CreateInputOptionPage(DataPage.ID, 'Certificates', 'Which certificate authority (CA)?',
    'Each server has its own CA. Phones learn it when they enroll by scanning the QR code the server prints.',
    True, False);
  CAPage.Add('Create this server''s own CA (recommended)');
  CAPage.Add('Use a CA I already have (ca.pem and ca-key.pem), so devices of another server keep working');
  CAPage.SelectedValueIndex := 0;

  CAFolderPage := CreateInputDirPage(CAPage.ID, 'Certificates', 'Where are ca.pem and ca-key.pem?', '', False, '');
  CAFolderPage.Add('');

  ModelPage := CreateInputOptionPage(CAFolderPage.ID, 'Search', 'Which text search model?',
    'Setup downloads the models for search, faces, text in photos, similar photos and the nudity scan: about 3 GB.',
    True, False);
  ModelPage.Add('Base: reads a photo in about a tenth of a second (recommended)');
  ModelPage.Add('Large: finds better, about four times slower to read photos');
  ModelPage.SelectedValueIndex := 0;
end;

function ShouldSkipPage(PageID: Integer): Boolean;
begin
  Result := False;
  // Installing again over a server keeps everything it has: setup only adds what is missing.
  if IsUpgrade() then
    Result := (PageID = ModePage.ID) or (PageID = MovePage.ID) or (PageID = UserPage.ID) or (PageID = DataPage.ID) or
      (PageID = CAPage.ID) or (PageID = CAFolderPage.ID) or (PageID = ModelPage.ID)
  else if IsMove() then
    Result := (PageID = UserPage.ID) or (PageID = DataPage.ID) or (PageID = CAPage.ID) or (PageID = CAFolderPage.ID) or
      (PageID = ModelPage.ID)
  else
    Result := (PageID = MovePage.ID) or ((PageID = CAFolderPage.ID) and (CAPage.SelectedValueIndex = 0));
end;

function NextButtonClick(CurPageID: Integer): Boolean;
begin
  Result := True;
  if (CurPageID = MovePage.ID) and not FileExists(AddBackslash(MovePage.Values[0]) + 'config.json') then
  begin
    MsgBox('There is no config.json in that folder.', mbError, MB_OK);
    Result := False;
  end
  else if (CurPageID = UserPage.ID) and (Trim(UserPage.Values[0]) = '') then
  begin
    MsgBox('Enter a user name.', mbError, MB_OK);
    Result := False;
  end
  else if (CurPageID = CAFolderPage.ID) and not (FileExists(AddBackslash(CAFolderPage.Values[0]) + 'ca.pem') and
    FileExists(AddBackslash(CAFolderPage.Values[0]) + 'ca-key.pem')) then
  begin
    MsgBox('That folder needs both ca.pem and ca-key.pem.', mbError, MB_OK);
    Result := False;
  end;
end;

// Stops a running server before its program is replaced.
function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  Code: Integer;
begin
  Result := '';
  if FileExists(ExpandConstant('{app}\journal-server.exe')) then
    Exec(ExpandConstant('{app}\journal-server.exe'), 'service stop', '', SW_HIDE, ewWaitUntilTerminated, Code);
end;

function Quote(S: String): String;
begin
  Result := '"' + S + '"';
end;

function SetupParameters(): String;
begin
  Result := 'setup --dir ' + Quote(ExpandConstant('{app}'));
  if IsUpgrade() then
    exit;
  if IsMove() then
    Result := Result + ' --move-from ' + Quote(MovePage.Values[0])
  else
  begin
    Result := Result + ' --user ' + Quote(Trim(UserPage.Values[0])) + ' --data-folder ' + Quote(DataPage.Values[0]);
    if CAPage.SelectedValueIndex = 1 then
      Result := Result + ' --import-ca ' + Quote(CAFolderPage.Values[0])
    else
      Result := Result + ' --new-ca';
    if ModelPage.SelectedValueIndex = 1 then
      Result := Result + ' --model large';
  end;
end;

// Runs journal-server in a console window that stays open on an error, so its messages can be read.
function RunServer(Parameters: String): Boolean;
var
  Code: Integer;
begin
  Result := Exec(ExpandConstant('{cmd}'),
    '/c ""' + ExpandConstant('{app}\journal-server.exe') + '" ' + Parameters + ' || (echo. & echo Setup did not finish. Run the installer again to go on. & pause & exit /b 1)"',
    ExpandConstant('{app}'), SW_SHOW, ewWaitUntilTerminated, Code) and (Code = 0);
end;

procedure AddToPath(Dir: String);
var
  Path: String;
begin
  if not RegQueryStringValue(HKLM, 'SYSTEM\CurrentControlSet\Control\Session Manager\Environment', 'Path', Path) then
    Path := '';
  if Pos(';' + Uppercase(Dir) + ';', ';' + Uppercase(Path) + ';') = 0 then
    RegWriteExpandStringValue(HKLM, 'SYSTEM\CurrentControlSet\Control\Session Manager\Environment', 'Path', Path + ';' + Dir);
end;

procedure RemoveFromPath(Dir: String);
var
  Path: String;
  P: Integer;
begin
  if not RegQueryStringValue(HKLM, 'SYSTEM\CurrentControlSet\Control\Session Manager\Environment', 'Path', Path) then
    exit;
  P := Pos(';' + Uppercase(Dir) + ';', ';' + Uppercase(Path) + ';');
  if P = 0 then
    exit;
  Delete(Path, P - 1, Length(Dir) + 1);
  if (Length(Path) > 0) and (Path[1] = ';') then
    Delete(Path, 1, 1);
  RegWriteExpandStringValue(HKLM, 'SYSTEM\CurrentControlSet\Control\Session Manager\Environment', 'Path', Path);
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  if CurStep <> ssPostInstall then
    exit;
  AddToPath(ExpandConstant('{app}'));
  WizardForm.StatusLabel.Caption := 'Setting up the server and downloading what it needs (this takes a while)...';
  if not RunServer(SetupParameters()) then
  begin
    MsgBox('The server''s setup did not finish (see the console window). The programs are installed; run this ' +
      'installer again to go on where it stopped.', mbError, MB_OK);
    exit;
  end;
  WizardForm.StatusLabel.Caption := 'Starting the server...';
  if not (RunServer('service install') and RunServer('service start')) then
    MsgBox('The server is set up but its service did not start. Check server.log in ' + ExpandConstant('{app}') + '.',
      mbError, MB_OK);
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep = usPostUninstall then
    RemoveFromPath(ExpandConstant('{app}'));
end;
