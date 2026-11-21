import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import { prepareSingleUpload } from '../src/api/upload.ts'

const nameCases = JSON.parse(readFileSync(new URL('../../../api/testdata/entry_name_cases.json', import.meta.url), 'utf8'))

for (const example of nameCases) {
  test(`共享文件名规则 ${example.case}`, () => {
    const result = prepareSingleUpload('33', { name: example.input, size: 8 })
    if (example.valid) {
      assert.deepEqual(result, { declaration: { parent_id: '33', name: example.input, size_bytes: 8 } })
    } else {
      assert.match(result.error, /文件名不符合命名规则/)
    }
  })
}

test('允许零字节和 16 MiB，拒绝超限及无效的字节数', () => {
  const name = '课程资料.bin'
  for (const size of [0, 16_777_216]) {
    assert.deepEqual(prepareSingleUpload('33', { name, size }), {
      declaration: { parent_id: '33', name, size_bytes: size },
    })
  }
  assert.match(prepareSingleUpload('33', { name, size: 16_777_217 }).error, /超过 16 MiB/)
  for (const size of [-1, 0.5, Infinity, NaN]) {
    assert.match(prepareSingleUpload('33', { name, size }).error, /有效的文件大小/)
  }
})

test('名称按 UTF-16 代码单元限长，检查 Unicode 空白但保留原文', () => {
  const name = `\u3000${'🌱'.repeat(60)}\u2009`
  assert.equal(prepareSingleUpload('33', { name, size: 0 }).declaration.name, name)
  for (const invalid of ['🌱'.repeat(61), '表'.repeat(121), '\uD800.bin', 'COM1 .txt']) {
    assert.match(prepareSingleUpload('33', { name: invalid, size: 0 }).error, /文件名不符合命名规则/)
  }
  assert.equal(prepareSingleUpload('33', { name: '\uFEFF', size: 0 }).declaration.name, '\uFEFF')
})

test('使用当前目录的标准十进制 ID，保留 uint64 精度', () => {
  const file = { name: 'example.txt', size: 1 }
  for (const id of ['33', '9007199254740993', '18446744073709551615']) {
    assert.equal(prepareSingleUpload(id, file).declaration.parent_id, id)
  }
  for (const id of ['', '0', '033', ' 33', '33\n', '18446744073709551616']) {
    assert.match(prepareSingleUpload(id, file).error, /上传目标目录无效/)
  }
})
