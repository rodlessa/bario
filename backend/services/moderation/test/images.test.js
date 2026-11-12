'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { CLASSES, formatProbabilities, isSupportedImage } = require('../src/images');

function padded(prefix) {
    const buffer = Buffer.alloc(32);
    Buffer.from(prefix).copy(buffer);
    return buffer;
}

test('classes are in canonical API order', () => {
    assert.deepEqual([...CLASSES], ['Neutral', 'Drawing', 'Sexy', 'Hentai', 'Porn']);
});

test('accepts supported image magic bytes', () => {
    assert.equal(isSupportedImage(padded([0xff, 0xd8, 0xff, 0xe0])), true);
    assert.equal(isSupportedImage(padded([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])), true);
    assert.equal(isSupportedImage(padded('GIF89a')), true);
    assert.equal(isSupportedImage(padded('BM')), true);
});

test('rejects non-images and unsupported formats', () => {
    assert.equal(isSupportedImage(Buffer.from('hello world, not an image')), false);
    assert.equal(isSupportedImage(Buffer.alloc(0)), false);
    assert.equal(isSupportedImage(Buffer.alloc(32)), false);
    assert.equal(isSupportedImage(padded('RIFFxxxxWEBP')), false);
});

test('formatProbabilities fills every class in order', () => {
    const result = formatProbabilities([
        { className: 'Porn', probability: 0.75 },
        { className: 'Neutral', probability: 0.25 },
        { className: 'Bogus', probability: 0.5 },
    ]);

    assert.deepEqual(Object.keys(result), [...CLASSES]);
    assert.equal(result.Neutral, 0.25);
    assert.equal(result.Porn, 0.75);
    assert.equal(result.Drawing, 0);
    assert.equal(result.Sexy, 0);
    assert.equal(result.Hentai, 0);
});

test('formatProbabilities tolerates missing input', () => {
    const result = formatProbabilities(null);
    assert.deepEqual(result, {
        Neutral: 0,
        Drawing: 0,
        Sexy: 0,
        Hentai: 0,
        Porn: 0,
    });
});
