import { readFile } from 'node:fs/promises';
import assert from 'node:assert/strict';
import { value } from 'fixture-helper';
const manifest = JSON.parse(await readFile('package.json', 'utf8'));
const expected = JSON.parse(await readFile(`${manifest.dependencies['fixture-helper'].slice(5)}/package.json`, 'utf8'));
const installed = JSON.parse(await readFile('node_modules/fixture-helper/package.json', 'utf8'));
assert.equal(installed.version, expected.version);
assert.equal(value, expected.fixtureValue);
