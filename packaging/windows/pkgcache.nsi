; pkgcache installer for Windows.
;
; Per-user, deliberately. Everything lands under %LOCALAPPDATA% and HKCU, so there is no
; UAC prompt and no administrator needed — which matters because the people who install
; this are developers on managed laptops, and an installer that needs IT is an installer
; that does not get run.
;
; What it installs, which is the whole product rather than a binary:
;
;   pkgcache.exe      the daemon and the CLI
;   pkgcache-app.exe  the window and the notification-area icon
;   a Start Menu shortcut, so it is launchable without a terminal
;   an Add/Remove Programs entry, so it is removable the way everything else is
;   PATH, for this user only
;   a disk budget, where the machine has none — see LIMIT below
;   optionally, a login entry and the machine's configuration
;
; Built with:
;   makensis -DVERSION=1.2.0 pkgcache.nsi
;   makensis -DVERSION=1.2.0 -DSERVER=https://cache:8443 -DCASHA256=AA:BB:.. pkgcache.nsi
;
; The two defines make a self-configuring installer: with them, installing is also the
; setup, which is the property the macOS package has had from the start.

!ifndef VERSION
  !define VERSION "0.0.0"
!endif
; The disk budget this installs when the machine has none.
;
; pkgcache refuses to start without one and will not guess a size for somebody else's
; disk, which is right — but on Windows the consequence was a first run that showed a
; webview error and no way to learn why. "none" is the answer that guesses least: no cap,
; and the free-space floor still applies, so the cache cannot fill the disk. Override with
; -DLIMIT=25G at build time.
!ifndef LIMIT
  !define LIMIT "none"
!endif
!define NAME "pkgcache"
!define PUBLISHER "pkgreg"
!define UNINSTKEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\${NAME}"

Name "${NAME} ${VERSION}"
OutFile "pkgcache-${VERSION}-setup.exe"
Unicode true
; Compressed as one solid block: two Go binaries are ~45 MB of very similar bytes.
SetCompressor /SOLID lzma
RequestExecutionLevel user
InstallDir "$LOCALAPPDATA\Programs\${NAME}"
InstallDirRegKey HKCU "Software\${NAME}" "InstallDir"
ShowInstDetails show
ShowUninstDetails show

!include "MUI2.nsh"
!include "FileFunc.nsh"
!include "LogicLib.nsh"
!include "WinMessages.nsh"
!include "StrFunc.nsh"

; StrFunc requires each function to be declared before use, and separately for the
; uninstaller — the Un- prefixed forms are compiled into a different half of the binary.
; Without these the ${StrStr} and ${StrRep} below are undefined macros, which is a
; compile error rather than a silent one, but only for somebody who runs makensis.
${StrStr}
${UnStrRep}

!define MUI_ICON "pkgcache.ico"
!define MUI_UNICON "pkgcache.ico"
!define MUI_ABORTWARNING
!define MUI_FINISHPAGE_RUN "$INSTDIR\pkgcache-app.exe"
!define MUI_FINISHPAGE_RUN_TEXT "Open pkgcache"

; Components before directory: PATH and the login item are genuinely optional, and an
; installer that adds a startup entry without asking is one people uninstall. The
; descriptions below are written for this page — without it makensis warns that they have
; nowhere to go, which is how their absence was noticed.
!insertmacro MUI_PAGE_COMPONENTS
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"

; stopRunning shuts down what is already there before the files are replaced.
;
; Windows will not overwrite a running executable, and an installer that failed halfway
; through would leave a directory holding one new binary and one old one — two halves of
; one product from different builds, which is the failure this project takes most care to
; avoid. `pkgcache stop` rather than taskkill for the daemon, so it exits the way it
; normally does and writes its state out.
!macro stopRunning
  DetailPrint "Stopping anything already running..."
  nsExec::ExecToLog '"$INSTDIR\pkgcache.exe" stop'
  Pop $0
  nsExec::ExecToLog 'taskkill /IM pkgcache-app.exe /F'
  Pop $0
  Sleep 500
!macroend

Section "pkgcache" SecMain
  SectionIn RO
  !insertmacro stopRunning

  SetOutPath "$INSTDIR"
  SetOverwrite try
  File "pkgcache.exe"
  File "pkgcache-app.exe"
  File "pkgcache.ico"

  WriteRegStr HKCU "Software\${NAME}" "InstallDir" "$INSTDIR"

  ; Launchable without a terminal, which is most of what "installed" means to somebody
  ; who found this through a link rather than a shell.
  CreateDirectory "$SMPROGRAMS\${NAME}"
  CreateShortcut "$SMPROGRAMS\${NAME}\${NAME}.lnk" "$INSTDIR\pkgcache-app.exe" "" \
    "$INSTDIR\pkgcache.ico"

  ; Add/Remove Programs. EstimatedSize is read from the directory rather than guessed,
  ; because a wrong number there is visible and slightly insulting.
  WriteUninstaller "$INSTDIR\uninstall.exe"
  ${GetSize} "$INSTDIR" "/S=0K" $0 $1 $2
  WriteRegStr   HKCU "${UNINSTKEY}" "DisplayName"     "${NAME}"
  WriteRegStr   HKCU "${UNINSTKEY}" "DisplayVersion"  "${VERSION}"
  WriteRegStr   HKCU "${UNINSTKEY}" "Publisher"       "${PUBLISHER}"
  WriteRegStr   HKCU "${UNINSTKEY}" "DisplayIcon"     "$INSTDIR\pkgcache.ico"
  WriteRegStr   HKCU "${UNINSTKEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr   HKCU "${UNINSTKEY}" "UninstallString" "$\"$INSTDIR\uninstall.exe$\""
  WriteRegStr   HKCU "${UNINSTKEY}" "QuietUninstallString" \
    "$\"$INSTDIR\uninstall.exe$\" /S"
  WriteRegDWORD HKCU "${UNINSTKEY}" "NoModify" 1
  WriteRegDWORD HKCU "${UNINSTKEY}" "NoRepair" 1
  WriteRegDWORD HKCU "${UNINSTKEY}" "EstimatedSize" "$0"

  ; Verified before anything is claimed to have worked. A truncated copy is still a valid
  ; PE header: it installs cleanly and then fails at run time with nothing useful said.
  DetailPrint "Checking the installed binary runs..."
  nsExec::ExecToLog '"$INSTDIR\pkgcache.exe" version'
  Pop $0
  ${If} $0 != 0
    MessageBox MB_ICONSTOP "The installed pkgcache.exe does not run. Nothing was configured."
    Abort
  ${EndIf}

  ; A disk budget, but only where there is not one already.
  ;
  ; `pkgcache limit` with no argument exits non-zero exactly when none is set, which makes
  ; it the query as well as the setter. Asking first is what stops an upgrade from
  ; overwriting a size somebody chose deliberately — this section runs on every install,
  ; and silently resetting a 200G cap to "none" would be a poor way to repay an upgrade.
  nsExec::ExecToStack '"$INSTDIR\pkgcache.exe" limit'
  Pop $0
  Pop $1
  ${If} $0 != 0
    DetailPrint "Setting the cache limit to ${LIMIT}..."
    nsExec::ExecToLog '"$INSTDIR\pkgcache.exe" limit ${LIMIT}'
    Pop $0
    ${If} $0 != 0
      MessageBox MB_ICONEXCLAMATION \
        "pkgcache is installed but has no disk budget, and will not start without one.$\r$\nSet one yourself:$\r$\n$\r$\n  pkgcache limit ${LIMIT}"
    ${EndIf}
  ${Else}
    DetailPrint "A cache limit is already set; leaving it alone."
  ${EndIf}
SectionEnd

Section "Add to PATH" SecPath
  ; HKCU only, so no elevation. The broadcast is what stops this needing a logout: without
  ; it, already-open programs keep the old PATH until they restart, which reads as the
  ; installer not having worked.
  ReadRegStr $0 HKCU "Environment" "Path"
  ${StrStr} $1 "$0" "$INSTDIR"
  ${If} $1 == ""
    ${If} $0 == ""
      WriteRegExpandStr HKCU "Environment" "Path" "$INSTDIR"
    ${Else}
      WriteRegExpandStr HKCU "Environment" "Path" "$0;$INSTDIR"
    ${EndIf}
    SendMessage ${HWND_BROADCAST} ${WM_WININICHANGE} 0 "STR:Environment" /TIMEOUT=5000
    DetailPrint "Added $INSTDIR to your PATH."
  ${EndIf}
SectionEnd

Section "Start with Windows" SecLogin
  ; The app's own flag rather than a registry Run value written here: it writes a .cmd in
  ; the Startup folder, and -off-login removes exactly what it wrote. One place that knows
  ; the format beats two that have to agree.
  nsExec::ExecToLog '"$INSTDIR\pkgcache-app.exe" -on-login'
  Pop $0
SectionEnd

!ifdef SERVER
Section "Point this machine at ${SERVER}" SecConfigure
  DetailPrint "Configuring for ${SERVER}..."
  nsExec::ExecToLog '"$INSTDIR\pkgcache.exe" setup -server "${SERVER}" \
    -ca-sha256 "${CASHA256}" -limit ${LIMIT}'
  Pop $0
  ${If} $0 != 0
    ; Installed but unconfigured is a recoverable state, and worth saying rather than
    ; failing the whole install over.
    MessageBox MB_ICONEXCLAMATION \
      "pkgcache is installed but could not be configured.$\r$\nRun this yourself:$\r$\n$\r$\n  pkgcache setup -server ${SERVER} -ca-sha256 ${CASHA256} -limit ${LIMIT}"
  ${EndIf}
SectionEnd
!endif

LangString DESC_SecMain   ${LANG_ENGLISH} "The cache, and the app that watches it."
LangString DESC_SecPath   ${LANG_ENGLISH} "Run pkgcache from any terminal."
LangString DESC_SecLogin  ${LANG_ENGLISH} \
  "Put the icon in the notification area when you log in. It never keeps the cache running."

!insertmacro MUI_FUNCTION_DESCRIPTION_BEGIN
  !insertmacro MUI_DESCRIPTION_TEXT ${SecMain}  $(DESC_SecMain)
  !insertmacro MUI_DESCRIPTION_TEXT ${SecPath}  $(DESC_SecPath)
  !insertmacro MUI_DESCRIPTION_TEXT ${SecLogin} $(DESC_SecLogin)
!insertmacro MUI_FUNCTION_DESCRIPTION_END

Section "Uninstall"
  ; Everything the installer put anywhere, taken out again — including the login entry and
  ; the PATH edit, which are the two an uninstaller usually forgets and which are the two
  ; that leave a machine haunted.
  nsExec::ExecToLog '"$INSTDIR\pkgcache-app.exe" -off-login'
  Pop $0
  nsExec::ExecToLog '"$INSTDIR\pkgcache.exe" stop'
  Pop $0
  nsExec::ExecToLog 'taskkill /IM pkgcache-app.exe /F'
  Pop $0
  Sleep 500

  ReadRegStr $0 HKCU "Environment" "Path"
  ${UnStrRep} $1 "$0" ";$INSTDIR" ""
  ${UnStrRep} $1 "$1" "$INSTDIR" ""
  WriteRegExpandStr HKCU "Environment" "Path" "$1"
  SendMessage ${HWND_BROADCAST} ${WM_WININICHANGE} 0 "STR:Environment" /TIMEOUT=5000

  Delete "$INSTDIR\pkgcache.exe"
  Delete "$INSTDIR\pkgcache-app.exe"
  Delete "$INSTDIR\pkgcache.ico"
  Delete "$INSTDIR\uninstall.exe"
  RMDir "$INSTDIR"

  Delete "$SMPROGRAMS\${NAME}\${NAME}.lnk"
  RMDir "$SMPROGRAMS\${NAME}"

  DeleteRegKey HKCU "${UNINSTKEY}"
  DeleteRegKey HKCU "Software\${NAME}"

  ; The cache itself is the person's data and is left alone. Said rather than assumed:
  ; somebody uninstalling to reclaim disk needs to know where the disk went.
  MessageBox MB_OK \
    "pkgcache has been removed.$\r$\n$\r$\nYour cached packages were left at:$\r$\n  $LOCALAPPDATA\pkgcache$\r$\n$\r$\nDelete that folder to reclaim the space."
SectionEnd
