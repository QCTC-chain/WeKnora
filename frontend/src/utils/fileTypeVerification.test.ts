import assert from 'node:assert/strict'
import test from 'node:test'

import { shouldRejectKnowledgeFileType } from './fileTypeVerification.ts'

test('shouldRejectKnowledgeFileType accepts HTML in the fallback whitelist', () => {
  assert.equal(shouldRejectKnowledgeFileType('page.html'), false)
  assert.equal(shouldRejectKnowledgeFileType('legacy.HTM'), false)
  assert.equal(shouldRejectKnowledgeFileType('payload.exe'), true)
})

test('shouldRejectKnowledgeFileType preserves dynamic whitelist behavior', () => {
  assert.equal(shouldRejectKnowledgeFileType('custom.xyz', ['xyz']), false)
  assert.equal(shouldRejectKnowledgeFileType('page.html', ['pdf']), true)
  assert.equal(shouldRejectKnowledgeFileType('page.html', []), false)
})

test('XMind uploads work before engine discovery and respect discovered capabilities', () => {
  for (const filename of ['architecture.xmind', 'ARCHITECTURE.XMIND']) {
    assert.equal(shouldRejectKnowledgeFileType(filename), false)
    assert.equal(shouldRejectKnowledgeFileType(filename, []), false)
    assert.equal(shouldRejectKnowledgeFileType(filename, new Set()), false)
    assert.equal(shouldRejectKnowledgeFileType(filename, ['pdf', 'xmind']), false)
    assert.equal(shouldRejectKnowledgeFileType(filename, ['pdf']), true)
  }
})

test('zip knowledge packages pass every whitelist shape', () => {
  // Zip is expanded by the upload endpoint, not a parser engine, so it must
  // be accepted before engines load (fallback whitelist), with an empty
  // engine set, and alongside a dynamic whitelist that has no zip entry.
  for (const validTypes of [undefined, [], new Set<string>(), ['pdf', 'md']]) {
    assert.equal(shouldRejectKnowledgeFileType('docs.zip', validTypes), false)
    assert.equal(shouldRejectKnowledgeFileType('手册.ZIP', validTypes), false)
  }
})
