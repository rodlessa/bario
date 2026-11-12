'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { CLASSES, detectImageFormat, formatProbabilities, isSupportedImage } = require('../src/images');

function padded(prefix) {
    const buffer = Buffer.alloc(32);
    Buffer.from(prefix).copy(buffer);
    return buffer;
}

test('classes are in canonical API order', () => {
    assert.deepEqual([...CLASSES], ['Neutral', 'Drawing', 'Sexy', 'Hentai', 'Porn']);
});

test('detects supported image formats', () => {
    assert.equal(detectImageFormat(padded([0xff, 0xd8, 0xff, 0xe0])), 'jpeg');
    assert.equal(detectImageFormat(padded([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])), 'png');
    assert.equal(detectImageFormat(padded('GIF89a')), 'gif');
    assert.equal(detectImageFormat(padded('BM')), 'bmp');
    assert.equal(detectImageFormat(padded('RIFFxxxxWEBP')), 'webp');
});

test('accepts supported image magic bytes', () => {
    assert.equal(isSupportedImage(padded([0xff, 0xd8, 0xff, 0xe0])), true);
    assert.equal(isSupportedImage(padded([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])), true);
    assert.equal(isSupportedImage(padded('GIF89a')), true);
    assert.equal(isSupportedImage(padded('BM')), true);
    assert.equal(isSupportedImage(padded('RIFFxxxxWEBP')), true);
});

test('rejects non-images and formats uploads does not accept', () => {
    assert.equal(isSupportedImage(Buffer.from('hello world, not an image')), false);
    assert.equal(isSupportedImage(Buffer.alloc(0)), false);
    assert.equal(isSupportedImage(Buffer.alloc(32)), false);
    // Go's sniffer returns image/x-icon for ICO; uploads rejects it so both
    // sides support exactly the same set of formats.
    assert.equal(isSupportedImage(padded([0x00, 0x00, 0x01, 0x00])), false);
    assert.equal(detectImageFormat(padded([0x00, 0x00, 0x01, 0x00])), null);
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
