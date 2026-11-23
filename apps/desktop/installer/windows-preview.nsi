# -*- coding: utf-8 -*-
Unicode True
RequestExecutionLevel user
CRCCheck force

!include "LogicLib.nsh"
!include "MUI2.nsh"
!include "x64.nsh"

!define PRODUCT_NAME "CloudNest Preview"
!define PRODUCT_VERSION "2.0.0-preview.1"
!define UNINSTALL_KEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\CloudNestPreview"
!define WEBVIEW2_KEY "Software\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}"

Name "${PRODUCT_NAME}"
OutFile "${INSTALLER_PATH}"
InstallDir "$LOCALAPPDATA\Programs\CloudNest Preview"
VIProductVersion "2.0.0.0"
VIAddVersionKey /LANG=2052 "ProductName" "${PRODUCT_NAME}"
VIAddVersionKey /LANG=2052 "ProductVersion" "${PRODUCT_VERSION}"
VIAddVersionKey /LANG=2052 "FileVersion" "2.0.0.0"
VIAddVersionKey /LANG=2052 "FileDescription" "CloudNest Windows 试用版安装程序"
VIAddVersionKey /LANG=2052 "LegalCopyright" "CloudNest"

!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "SimpChinese"

!macro LockSetup
  System::Call 'kernel32::CreateMutexW(p 0, i 0, w "Local\CloudNestPreviewSetup") p.r8 ?e'
  Pop $9
  ${If} $8 = 0
  ${OrIf} $9 = 183
    MessageBox MB_OK|MB_ICONSTOP "另一个 CloudNest 试用版安装或卸载正在运行。" /SD IDOK
    SetErrorLevel 2
    Abort
  ${EndIf}
!macroend

Function .onInit
  # 1. 检查架构及 Microsoft 文档列出的 WebView2 运行时注册位置。
  ${IfNot} ${IsNativeAMD64}
    MessageBox MB_OK|MB_ICONSTOP "此试用包需要 Windows x64。" /SD IDOK
    SetErrorLevel 2
    Abort
  ${EndIf}
  SetRegView 32
  ReadRegStr $0 HKLM "${WEBVIEW2_KEY}" "pv"
  ${If} $0 == ""
  ${OrIf} $0 == "0.0.0.0"
    ReadRegStr $0 HKCU "${WEBVIEW2_KEY}" "pv"
  ${EndIf}
  ${If} $0 == ""
  ${OrIf} $0 == "0.0.0.0"
    MessageBox MB_OK|MB_ICONSTOP "请先从 Microsoft 安装 WebView2 Evergreen Runtime，再重新运行安装包。" /SD IDOK
    SetErrorLevel 2
    Abort
  ${EndIf}
  SetRegView 64
  SetShellVarContext current
  !insertmacro LockSetup
FunctionEnd

Function un.onInit
  SetRegView 64
  SetShellVarContext current
  !insertmacro LockSetup
FunctionEnd

Section "CloudNest Preview"
  # 2. 只接受新的注册项与安装文件，避免误覆盖旧安装或同名用户文件。
  GetFullPathName $INSTDIR "$INSTDIR"
  ReadRegStr $0 HKCU "${UNINSTALL_KEY}" "InstallLocation"
  ReadRegStr $1 HKCU "${UNINSTALL_KEY}" "DisplayName"
  StrCmp $0 "" +2
  Goto install_conflict
  StrCmp $1 "" +2
  Goto install_conflict
  IfFileExists "$INSTDIR\CloudNest.exe" install_conflict
  IfFileExists "$INSTDIR\Uninstall.exe" install_conflict
  IfFileExists "$SMPROGRAMS\CloudNest Preview.lnk" install_conflict

  ClearErrors
  SetOutPath "$INSTDIR"
  IfErrors install_failed
  File /oname=CloudNest.exe "${APP_BINARY}"
  IfErrors install_failed
  WriteUninstaller "$INSTDIR\Uninstall.exe"
  IfErrors install_failed
  CreateShortcut "$SMPROGRAMS\CloudNest Preview.lnk" "$INSTDIR\CloudNest.exe"
  IfErrors install_failed
  WriteRegStr HKCU "${UNINSTALL_KEY}" "DisplayName" "${PRODUCT_NAME}"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "DisplayVersion" "${PRODUCT_VERSION}"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "DisplayIcon" "$INSTDIR\CloudNest.exe"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "UninstallString" '$\"$INSTDIR\Uninstall.exe$\"'
  WriteRegStr HKCU "${UNINSTALL_KEY}" "QuietUninstallString" '$\"$INSTDIR\Uninstall.exe$\" /S'
  WriteRegDWORD HKCU "${UNINSTALL_KEY}" "NoModify" 1
  WriteRegDWORD HKCU "${UNINSTALL_KEY}" "NoRepair" 1
  IfErrors install_failed
  Goto install_done

install_failed:
  SetOutPath "$TEMP"
  Delete "$INSTDIR\CloudNest.exe"
  Delete "$INSTDIR\Uninstall.exe"
  Delete "$SMPROGRAMS\CloudNest Preview.lnk"
  DeleteRegKey HKCU "${UNINSTALL_KEY}"
  RMDir "$INSTDIR"
  MessageBox MB_OK|MB_ICONSTOP "无法完成试用版安装，请检查目录权限和可用空间。" /SD IDOK
  SetErrorLevel 3
  Abort
install_conflict:
  MessageBox MB_OK|MB_ICONSTOP "试用版已安装或目标有同名文件。请先卸载旧试用版，或选择其他安装目录。" /SD IDOK
  SetErrorLevel 3
  Abort
install_done:
SectionEnd

Section "Uninstall"
  # 3. 校验卸载器所在目录，按文件名移除程序资产。账号数据和用户文件留在原处。
  GetFullPathName $INSTDIR "$INSTDIR"
  ReadRegStr $0 HKCU "${UNINSTALL_KEY}" "InstallLocation"
  StrCmp $0 "$INSTDIR" 0 uninstall_failed
  SetOutPath "$TEMP"
  ClearErrors
  Delete "$INSTDIR\CloudNest.exe"
  IfErrors uninstall_failed
  Delete "$SMPROGRAMS\CloudNest Preview.lnk"
  IfErrors uninstall_failed
  DeleteRegKey HKCU "${UNINSTALL_KEY}"
  IfErrors uninstall_failed
  Delete "$INSTDIR\Uninstall.exe"
  RMDir "$INSTDIR"
  Goto uninstall_done
uninstall_failed:
  MessageBox MB_OK|MB_ICONSTOP "无法完成卸载。请先从托盘退出 CloudNest，或核对当前试用版的安装位置。" /SD IDOK
  SetErrorLevel 4
  Abort
uninstall_done:
SectionEnd
