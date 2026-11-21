import { useState } from 'react'
import { Button, Modal } from 'antd'
import { maxSingleUploadSizeMiB, prepareSingleUpload } from './api/upload'

export default function SingleUploadDialog({ parentId, targetPath, onClose }: {
  parentId: string
  targetPath: string
  onClose: () => void
}) {
  const [file, setFile] = useState<File | null>(null)
  const preparation = file ? prepareSingleUpload(parentId, file) : null
  const declaration = preparation && 'declaration' in preparation ? preparation.declaration : null

  return (
    <Modal title="准备单文件上传" open onCancel={onClose} width={440}
      footer={<Button onClick={onClose}>关闭</Button>}>
      <div className="entry-name-form">
        <label htmlFor="single-upload-file">选择本地文件</label>
        <input id="single-upload-file" type="file" aria-describedby="single-upload-limit"
          onChange={event => setFile(event.currentTarget.files?.item(0) ?? null)} />
        <small id="single-upload-limit">单文件上限 {maxSingleUploadSizeMiB} MiB。</small>
      </div>
      <dl className="auth-profile">
        <div><dt>目标目录</dt><dd>{targetPath}</dd></div>
        {declaration && (
          <>
            <div><dt>文件名</dt><dd>{declaration.name}</dd></div>
            <div><dt>文件大小</dt><dd>{declaration.size_bytes} 字节</dd></div>
          </>
        )}
      </dl>
      {declaration && <p role="status">文件已就绪，尚未上传。</p>}
      {preparation && 'error' in preparation && <p className="auth-error" role="alert">{preparation.error}</p>}
    </Modal>
  )
}
