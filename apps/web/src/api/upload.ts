export const maxSingleUploadSizeMiB = 16
const maxSingleUploadSizeBytes = maxSingleUploadSizeMiB * 1024 * 1024
const maxUploadParentId = 18446744073709551615n
const maxEntryNameLength = 120
const reservedDeviceName = /^(CON|PRN|AUX|NUL|COM[1-9¹²³]|LPT[1-9¹²³])$/iu

interface SingleUploadDeclaration {
  parent_id: string
  name: string
  size_bytes: number
}

type SingleUploadPreparation = { declaration: SingleUploadDeclaration } | { error: string }

function isValidUploadName(rawName: string): boolean {
  // 按后端顺序先拒绝控制字符，再按 Unicode 空白检查展示名。
  if (/[\p{Cc}\p{Cs}]/u.test(rawName)) return false
  const name = rawName.replace(/^\p{White_Space}+|\p{White_Space}+$/gu, '')
  if (!name || name.length > maxEntryNameLength || name.endsWith('.') || /[<>:"/\\|?*]/u.test(name)) return false

  // Windows 设备名带扩展名时仍可能保留，基名尾随空格也要忽略。
  const baseName = name.split('.', 1)[0].replace(/ +$/u, '')
  return !reservedDeviceName.test(baseName) && !/^(CONIN|CONOUT)\$$/iu.test(name)
}

export function prepareSingleUpload(parentId: string, file: Pick<File, 'name' | 'size'>): SingleUploadPreparation {
  // 1. 按字符串保留完整目录 ID，避免转换为 JS 数字后丢失精度。
  if (!/^[1-9][0-9]{0,19}$/.test(parentId) || BigInt(parentId) > maxUploadParentId) {
    return { error: '上传目标目录无效。' }
  }

  // 2. 校验文件原名和字节数，零字节文件也可准备上传。
  if (!isValidUploadName(file.name)) return { error: '文件名不符合命名规则，请重命名后重新选择。' }
  if (!Number.isSafeInteger(file.size) || file.size < 0) return { error: '无法取得有效的文件大小。' }
  if (file.size > maxSingleUploadSizeBytes) return { error: `文件超过 ${maxSingleUploadSizeMiB} MiB，请重新选择。` }

  // 3. 组装创建任务字段，保留文件原名供后端统一处理。
  return { declaration: { parent_id: parentId, name: file.name, size_bytes: file.size } }
}
