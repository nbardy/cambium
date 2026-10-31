import { test } from 'node:test';
import assert from 'node:assert/strict';
import { message } from './dist/index.mjs';
test('built module uses this workspace and the requested dependency', () => {
  assert.equal(message, 'baseline:dependency-v1');
});
